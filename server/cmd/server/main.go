// Package main is the entry point for the FundAI Simulator server.
// It wires together configuration, database, services, and HTTP routing,
// then serves the React SPA alongside the JSON API.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/lotbackfill"
	"github.com/fundai/server/internal/mailer"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/marketplace"
	"github.com/fundai/server/internal/promotion"
	"github.com/fundai/server/internal/recall"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/scheduler"
	"github.com/fundai/server/internal/quota"
	"github.com/fundai/server/internal/secrets"
	"github.com/fundai/server/internal/subscription"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

const requestIDHeader = "X-Request-ID"
const traceIDHeader = "X-Trace-ID"
const spanIDHeader = "X-Span-ID"
const userLanguageHeader = "X-User-Language"

type requestIDContextKey string
type traceIDContextKey string
type spanIDContextKey string
type userLanguageContextKey string

const requestIDKey requestIDContextKey = "requestID"
const traceIDKey traceIDContextKey = "traceID"
const spanIDKey spanIDContextKey = "spanID"
const userLanguageKey userLanguageContextKey = "userLanguage"

// UserLanguage represents the resolved frontend language preference for a request.
type UserLanguage string

const (
	UserLanguageZH UserLanguage = "zh-CN"
	UserLanguageEN UserLanguage = "en-US"
)

// normalizeUserLanguage maps free-form Accept-Language / X-User-Language values
// to the supported set. Unknown values fall back to zh-CN to preserve current
// behaviour for callers without explicit language hints.
func normalizeUserLanguage(value string) UserLanguage {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return UserLanguageZH
	}
	if strings.HasPrefix(trimmed, "en") {
		return UserLanguageEN
	}
	return UserLanguageZH
}

// LanguageFromContext returns the user language attached to the request
// context. Defaults to zh-CN if no value is present.
func LanguageFromContext(ctx context.Context) UserLanguage {
	if ctx == nil {
		return UserLanguageZH
	}
	value, _ := ctx.Value(userLanguageKey).(UserLanguage)
	if value == UserLanguageEN || value == UserLanguageZH {
		return value
	}
	return UserLanguageZH
}

// Build-time variables injected via -ldflags.
var (
	version     = "dev"
	buildTime   = "unknown"
	buildCommit = "unknown"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Config holds all runtime configuration parsed from environment variables.
type LLMEnvModelConfig struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

type LLMDefaultsConfig struct {
	Global   LLMEnvModelConfig
	Critical LLMEnvModelConfig
	Standard LLMEnvModelConfig
	Simple   LLMEnvModelConfig
}

type Config struct {
	Port                    string
	Env                     string
	LogLevel                string
	DatabaseURL             string
	DBMaxOpenConns          int
	DBMaxIdleConns          int
	DBConnMaxLife           time.Duration
	JWTSecret               string
	// JWTKeyring is the F29 multi-key store backing JWT_SECRETS_JSON.
	// Populated lazily by LoadConfig from JWT_SECRETS_JSON or by falling
	// back to a single-key ring built from JWTSecret. Nil only in tests
	// that construct Config{} manually.
	JWTKeyring              *secrets.JWTKeyring
	SessionTTL              time.Duration
	ModelConfigAPIKeySecret string
	CORSOrigins             []string
	MigrationsPath          string
	StaticFilesPath         string
	LLMDefaults             LLMDefaultsConfig
	MarketData              marketdata.Config
	NewsTranslator          marketdata.TranslatorConfig
	Mailer                  mailer.Config
	AppPublicURL            string
	RecallEmbed             RecallEmbedConfig
}

// RecallEmbedConfig 控制 L3 memory pgvector backfill worker。
// APIKey 为空 → loop 不启动；recall.Service 没有 embedding 列时
// 也会自动短路，所以两端都是软启用。
type RecallEmbedConfig struct {
	APIKey  string
	BaseURL string
	Model   string
}

// LoadConfig reads configuration from environment with sensible defaults.
func LoadConfig() *Config {
	cfg := &Config{
		Port:                    firstEnv("APP_PORT", "SERVER_PORT", "8080"),
		Env:                     firstEnv("APP_ENV", "development"),
		LogLevel:                firstEnv("LOG_LEVEL", "info"),
		DatabaseURL:             firstEnv("DATABASE_URL", legacyDatabaseURLFallback()),
		DBMaxOpenConns:          envInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns:          envInt("DB_MAX_IDLE_CONNS", 10),
		DBConnMaxLife:           envDuration("DB_CONN_MAX_LIFETIME", 5*time.Minute),
		JWTSecret:               loadJWTPrimarySecret(),
		SessionTTL:              envDuration("SESSION_TTL", 12*time.Hour),
		ModelConfigAPIKeySecret: loadModelConfigSecret(),
		CORSOrigins:             splitCSV(firstEnv("CORS_ORIGINS", "http://localhost:3000,http://localhost:5173")),
		MigrationsPath:          firstEnv("MIGRATIONS_PATH", "MIGRATIONS_DIR", "migrations"),
		StaticFilesPath:         firstEnv("STATIC_FILES_PATH", "STATIC_DIR", "web/dist"),
		LLMDefaults:             loadLLMDefaultsFromEnv(),
		MarketData: marketdata.Config{
			ChinaStockURL:          firstEnv("MCP_CHINA_STOCK_URL", ""),
			AkshareURL:             firstEnv("MCP_AKSHARE_URL", ""),
			TALibURL:               firstEnv("MCP_TA_LIB_URL", ""),
			WebSearchURL:           firstEnv("MCP_WEB_SEARCH_URL", ""),
			QuantDingerURL:         firstEnv("QUANTDINGER_URL", ""),
			CoinGeckoBaseURL:       firstEnv("MARKETDATA_COINGECKO_BASE_URL", ""),
			CryptoWSEnabled:        envBoolWithDefault("MARKETDATA_CRYPTO_WS_ENABLED", true),
			BinanceWSURL:           firstEnv("MARKETDATA_BINANCE_WS_URL", ""),
			BinanceWSSymbols:       splitCSV(firstEnv("MARKETDATA_BINANCE_WS_SYMBOLS", "")),
			CoinbaseWSURL:          firstEnv("MARKETDATA_COINBASE_WS_URL", ""),
			CoinbaseWSProducts:     splitCSV(firstEnv("MARKETDATA_COINBASE_WS_PRODUCTS", "")),
			CryptoWSStaleAfter:     envDuration("MARKETDATA_CRYPTO_WS_STALE_AFTER", 30*time.Second),
			QuoteProviders:         splitCSV(firstEnv("MARKETDATA_QUOTE_PROVIDERS", "quantdinger,china-stock,akshare")),
			QuoteProvidersByMarket: readQuoteProvidersByMarket(),
			NewsProviders:          splitCSV(firstEnv("MARKETDATA_NEWS_PROVIDERS", "eastmoney,sina,local-search,web-search")),
			SerpAPIKeys:            splitCSV(firstEnv("SERPAPI_KEYS", "")),
			TavilyAPIKeys:          splitCSV(firstEnv("TAVILY_API_KEYS", "")),
			SerpAPIBaseURL:         firstEnv("SERPAPI_BASE_URL", ""),
			TavilyBaseURL:          firstEnv("TAVILY_BASE_URL", ""),
			EastmoneyNewsBaseURL:   firstEnv("EASTMONEY_NEWS_BASE_URL", ""),
			SinaNewsBaseURL:        firstEnv("SINA_NEWS_BASE_URL", ""),
			NewsHybridEnabled:      envBoolWithDefault("MARKETDATA_NEWS_HYBRID", true),
			QuoteTTL:               envDuration("MARKETDATA_QUOTE_TTL", 10*time.Second),
			NewsTTL:                envDuration("MARKETDATA_NEWS_TTL", 2*time.Minute),
			ProviderTimeout:        envDuration("MARKETDATA_PROVIDER_TIMEOUT", 5*time.Second),
			// Resilience knobs added in phase 3a. Defaults are conservative:
			// stale = 15 minutes (covers HK-lunch gaps without nagging),
			// circuit trips after 3 consecutive failures, 30s cool-down,
			// adaptive TTL on so we don't hammer upstreams overnight.
			StaleQuoteAfter:              envDuration("MARKETDATA_STALE_AFTER", 15*time.Minute),
			QuoteCircuitFailureThreshold: envInt("MARKETDATA_CIRCUIT_FAILURES", 3),
			QuoteCircuitCooldown:         envDuration("MARKETDATA_CIRCUIT_COOLDOWN", 30*time.Second),
			QuoteThrottleCooldown:        envDuration("MARKETDATA_THROTTLE_COOLDOWN", 5*time.Minute),
			AdaptiveTTLEnabled:           envBoolWithDefault("MARKETDATA_ADAPTIVE_TTL", true),
			// Adaptive quote TTL piggy-backs on the same MARKETDATA_ADAPTIVE_TTL
			// master switch so an operator who has explicitly disabled adaptive
			// caching for news doesn't get surprise quote-cache changes. Both
			// in-/off-session durations have safe defaults handled by Config.normalize().
			AdaptiveQuoteTTLEnabled: envBoolWithDefault("MARKETDATA_ADAPTIVE_QUOTE_TTL", true),
			QuoteTTLInSession:       envDuration("MARKETDATA_QUOTE_TTL_INSESSION", 5*time.Second),
			QuoteTTLOffSession:      envDuration("MARKETDATA_QUOTE_TTL_OFFSESSION", 60*time.Second),
			ProviderRateLimitsSpec:  firstEnv("MARKETDATA_QUOTE_RATE_LIMITS", ""),
		},
		NewsTranslator: marketdata.TranslatorConfig{
			Provider: firstEnv("MARKETDATA_TRANSLATOR_PROVIDER", "none"),
			BaseURL:  firstEnv("MARKETDATA_TRANSLATOR_BASE_URL", ""),
			APIKey:   firstEnv("MARKETDATA_TRANSLATOR_API_KEY", ""),
			Model:    firstEnv("MARKETDATA_TRANSLATOR_MODEL", ""),
			Timeout:  envDuration("MARKETDATA_TRANSLATOR_TIMEOUT", 8*time.Second),
		},
		Mailer: mailer.Config{
			Host:      firstEnv("SMTP_HOST", ""),
			Port:      envInt("SMTP_PORT", 587),
			Username:  firstEnv("SMTP_USERNAME", "SMTP_USER", ""),
			Password:  firstEnv("SMTP_PASSWORD", "SMTP_PASS", ""),
			From:      firstEnv("SMTP_FROM", ""),
			FromName:  firstEnv("SMTP_FROM_NAME", "FundAI"),
			UseTLS:    envBoolWithDefault("SMTP_USE_TLS", false),
			StartTLS:  envBoolWithDefault("SMTP_STARTTLS", true),
			Timeout:   envDuration("SMTP_TIMEOUT", 15*time.Second),
			AppURL:    strings.TrimRight(firstEnv("APP_PUBLIC_URL", "http://localhost:5173"), "/"),
			BrandName: firstEnv("BRAND_NAME", "FundAI"),
		},
		AppPublicURL: strings.TrimRight(firstEnv("APP_PUBLIC_URL", "http://localhost:5173"), "/"),
		RecallEmbed: RecallEmbedConfig{
			APIKey:  firstEnv("RECALL_OPENAI_API_KEY", "OPENAI_API_KEY", ""),
			BaseURL: firstEnv("RECALL_OPENAI_BASE_URL", "OPENAI_BASE_URL", ""),
			Model:   firstEnv("RECALL_EMBED_MODEL", "text-embedding-3-small"),
		},
	}

	if len(cfg.CORSOrigins) == 0 {
		cfg.CORSOrigins = []string{"http://localhost:3000", "http://localhost:5173"}
	}

	// F29: assemble the JWT keyring after JWTSecret has settled. Errors
	// are deferred to validateConfig so LoadConfig stays infallible
	// (matches its callers' expectations); a nil keyring will surface
	// as a startup-time validation failure.
	if ring, err := secrets.LoadJWTKeyringFromEnv(); err == nil {
		cfg.JWTKeyring = ring
		cfg.JWTSecret = ring.Active().Secret
	} else if strings.TrimSpace(cfg.JWTSecret) != "" {
		if ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{{Kid: "default", Secret: cfg.JWTSecret, Active: true}}); err == nil {
			cfg.JWTKeyring = ring
		}
	}

	return cfg
}

// effectiveJWTKeyring returns cfg.JWTKeyring when set, otherwise
// builds a single-key ring from cfg.JWTSecret. Tests that construct
// Config{JWTSecret: "x"} without going through LoadConfig rely on this
// fallback; production wiring always populates JWTKeyring up front so
// this branch is a no-op there.
func (cfg *Config) effectiveJWTKeyring() *secrets.JWTKeyring {
	if cfg == nil {
		return nil
	}
	if cfg.JWTKeyring != nil {
		return cfg.JWTKeyring
	}
	if strings.TrimSpace(cfg.JWTSecret) == "" {
		return nil
	}
	ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{{Kid: "default", Secret: cfg.JWTSecret, Active: true}})
	if err != nil {
		return nil
	}
	return ring
}

// loadModelConfigSecret reads MODEL_CONFIG_API_KEY_SECRET via the
// shared secrets loader (so _FILE variants work uniformly), falling
// back to the legacy API_KEY_ENCRYPTION_SECRET name for compatibility
// with deployments that haven't renamed their env yet.
func loadModelConfigSecret() string {
	if v, err := secrets.FromEnv("MODEL_CONFIG_API_KEY_SECRET"); err == nil && v != "" {
		return v
	}
	if v, err := secrets.FromEnv("API_KEY_ENCRYPTION_SECRET"); err == nil && v != "" {
		return v
	}
	return ""
}

// loadJWTPrimarySecret prefers JWT_SECRETS_JSON's active secret when
// available, otherwise reads JWT_SECRET / JWT_SECRET_FILE, falling back
// to the historical dev default. The result is stored on cfg.JWTSecret
// for back-compat with callers that haven't migrated to keyring yet.
func loadJWTPrimarySecret() string {
	if ring, err := secrets.LoadJWTKeyringFromEnv(); err == nil {
		return ring.Active().Secret
	}
	if v, err := secrets.FromEnv("JWT_SECRET"); err == nil && v != "" {
		return v
	}
	return "dev-secret-do-not-use-in-prod"
}

func loadLLMDefaultsFromEnv() LLMDefaultsConfig {
	providerDefaults := providerEnvModelConfigs()
	global := resolveLLMEnvModelConfig("LLM", LLMEnvModelConfig{Provider: "openai"}, providerDefaults)
	return LLMDefaultsConfig{
		Global:   global,
		Critical: resolveLLMEnvModelConfig("LLM_CRITICAL", global, providerDefaults),
		Standard: resolveLLMEnvModelConfig("LLM_STANDARD", global, providerDefaults),
		Simple:   resolveLLMEnvModelConfig("LLM_SIMPLE", global, providerDefaults),
	}
}

func providerEnvModelConfigs() map[string]LLMEnvModelConfig {
	return map[string]LLMEnvModelConfig{
		"openai": {
			Provider: "openai",
			Model:    strings.TrimSpace(firstEnv("OPENAI_MODEL", "")),
			BaseURL:  strings.TrimSpace(firstEnv("OPENAI_BASE_URL", "")),
			APIKey:   strings.TrimSpace(firstEnv("OPENAI_API_KEY", "")),
		},
		"claude": {
			Provider: "claude",
			Model:    strings.TrimSpace(firstEnv("CLAUDE_MODEL", "ANTHROPIC_MODEL", "")),
			BaseURL:  strings.TrimSpace(firstEnv("CLAUDE_BASE_URL", "ANTHROPIC_BASE_URL", "")),
			APIKey:   strings.TrimSpace(firstEnv("CLAUDE_API_KEY", "ANTHROPIC_API_KEY", "")),
		},
		"deepseek": {
			Provider: "deepseek",
			Model:    strings.TrimSpace(firstEnv("DEEPSEEK_MODEL", "")),
			BaseURL:  strings.TrimSpace(firstEnv("DEEPSEEK_BASE_URL", "")),
			APIKey:   strings.TrimSpace(firstEnv("DEEPSEEK_API_KEY", "")),
		},
		"qwen": {
			Provider: "qwen",
			Model:    strings.TrimSpace(firstEnv("QWEN_MODEL", "")),
			BaseURL:  strings.TrimSpace(firstEnv("QWEN_BASE_URL", "")),
			APIKey:   strings.TrimSpace(firstEnv("QWEN_API_KEY", "")),
		},
		"gemini": {
			Provider: "gemini",
			Model:    strings.TrimSpace(firstEnv("GEMINI_MODEL", "GOOGLE_MODEL", "")),
			BaseURL:  strings.TrimSpace(firstEnv("GEMINI_BASE_URL", "GOOGLE_BASE_URL", "")),
			APIKey:   strings.TrimSpace(firstEnv("GEMINI_API_KEY", "GOOGLE_API_KEY", "")),
		},
	}
}

func resolveLLMEnvModelConfig(prefix string, inherited LLMEnvModelConfig, providerDefaults map[string]LLMEnvModelConfig) LLMEnvModelConfig {
	cfg := inherited
	provider := normalizeLLMProvider(firstEnv(prefix+"_PROVIDER", cfg.Provider))
	if provider == "" {
		provider = cfg.Provider
	}
	if provider == "" {
		provider = "openai"
	}
	alias := providerDefaults[provider]
	providerChanged := provider != cfg.Provider
	cfg.Provider = provider
	if value := strings.TrimSpace(firstEnv(prefix+"_MODEL", "")); value != "" {
		cfg.Model = value
	} else if providerChanged || strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = alias.Model
	}
	if value := strings.TrimSpace(firstEnv(prefix+"_BASE_URL", "")); value != "" {
		cfg.BaseURL = value
	} else if providerChanged || strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = alias.BaseURL
	}
	if value := strings.TrimSpace(firstEnv(prefix+"_API_KEY", "")); value != "" {
		cfg.APIKey = value
	} else if providerChanged || strings.TrimSpace(cfg.APIKey) == "" {
		cfg.APIKey = alias.APIKey
	}
	return cfg
}

func normalizeLLMProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	case "deepseek":
		return "deepseek"
	case "qwen":
		return "qwen"
	case "gemini", "google":
		return "gemini"
	case "custom":
		return "custom"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

// connectDB opens a PostgreSQL connection with retry logic so the app can
// wait for the database container to become ready.
func connectDB(cfg *Config) (*sql.DB, error) {
	var db *sql.DB
	var err error

	maxRetries := 15
	for i := 0; i < maxRetries; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err != nil {
			slog.Warn("failed to open database", "attempt", i+1, "error", err)
			time.Sleep(2 * time.Second)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = db.PingContext(ctx)
		cancel()

		if err == nil {
			break
		}

		slog.Warn("database not ready, retrying", "attempt", i+1, "error", err)
		_ = db.Close()
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		return nil, fmt.Errorf("database unreachable after %d retries: %w", maxRetries, err)
	}

	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(cfg.DBConnMaxLife)

	slog.Info("connected to PostgreSQL")
	return db, nil
}

// runMigrations applies all .sql files from the migrations directory in order.
func runMigrations(db *sql.DB, migrationsPath string) error {
	entries, err := os.ReadDir(migrationsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			slog.Warn("migrations directory not found, skipping", "path", migrationsPath)
			return nil
		}
		return fmt.Errorf("read migrations dir: %w", err)
	}

	// Ensure migration tracking table exists.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		// Skip *.down.sql files — they are reverse migrations
		// for manual rollback (operator runs them via psql), not
		// for the boot-time forward runner. Alphabetical order
		// would otherwise execute `040_x.down.sql` BEFORE
		// `040_x.sql`, dropping the column the up migration is
		// about to add — harmless today because every down uses
		// IF EXISTS, but the wasted DROP/CREATE cycle is
		// confusing in the slog and one missing IF EXISTS would
		// fail the boot.
		if strings.HasSuffix(entry.Name(), ".down.sql") {
			continue
		}

		// Skip already-applied migrations.
		var exists bool
		_ = db.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", entry.Name()).Scan(&exists)
		if exists {
			continue
		}

		content, err := os.ReadFile(filepath.Join(migrationsPath, entry.Name()))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", entry.Name(), err)
		}

		if _, err := tx.Exec(string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("execute migration %s: %w", entry.Name(), err)
		}

		if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", entry.Name()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", entry.Name(), err)
		}

		slog.Info("applied migration", "file", entry.Name())
	}

	return nil
}

// ---------------------------------------------------------------------------
// Placeholder service layer (wiring stubs)
// ---------------------------------------------------------------------------

type Services struct {
	DB                     *sql.DB
	SubscriptionService    *subscription.SubscriptionService
	UsageTracker           *subscription.UsageTracker
	LLMRuntime             *llmRuntime
	Metrics                *serverMetrics
	SubscriptionHandler    *api.SubscriptionHandler
	FundHandler            *api.FundHandler
	WorkflowService        *workflowServiceAdapter
	MarketplaceReconciler  *marketplaceReconcilerLoop
	AuctionSettlement      *auctionSettlementLoop
	ActivityRetention      *activityRetentionLoop
	PositionQuoteRefresher *positionQuoteRefresher
	LeaseManager           *scheduler.LeaseManager
	MarketDataService      *marketdata.Service
	PromotionAdapter       *promotionServiceAdapter
	PromotionResolver      *promotion.Resolver
	PromotionDecayLoop     *promotionDecayLoop
	LessonScoringLoop      *lessonScoringLoop
	MemoryArchiveLoop      *memoryArchiveLoop
	MemoryEmbedLoop        *memoryEmbedLoop
	Mailer                 mailer.Mailer
}

func (s *Services) Stop() {
	if s == nil {
		return
	}
	if s.UsageTracker != nil {
		s.UsageTracker.Stop()
	}
	if s.WorkflowService != nil {
		s.WorkflowService.StopBackgroundScheduler()
	}
	if s.MarketplaceReconciler != nil {
		s.MarketplaceReconciler.Stop()
	}
	if s.AuctionSettlement != nil {
		s.AuctionSettlement.Stop()
	}
	if s.ActivityRetention != nil {
		s.ActivityRetention.Stop()
	}
	if s.PositionQuoteRefresher != nil {
		s.PositionQuoteRefresher.Stop()
	}
	if s.PromotionDecayLoop != nil {
		s.PromotionDecayLoop.Stop()
	}
	if s.LessonScoringLoop != nil {
		s.LessonScoringLoop.Stop()
	}
	if s.MemoryArchiveLoop != nil {
		s.MemoryArchiveLoop.Stop()
	}
	if s.MemoryEmbedLoop != nil {
		s.MemoryEmbedLoop.Stop()
	}
	if s.LeaseManager != nil {
		s.LeaseManager.Stop()
	}
	if s.MarketDataService != nil {
		if err := s.MarketDataService.Close(5 * time.Second); err != nil {
			slog.Warn("marketdata shutdown timed out", "err", err)
		}
	}
}

func initServices(db *sql.DB, cfg *Config) (*Services, error) {
	metrics := newServerMetrics()
	subscriptionService := subscription.NewSubscriptionService(db)
	usageTracker := subscription.NewUsageTracker(db)
	usageTracker.Start()
	modelConfigService := subscription.NewModelConfigService(db)
	budgetService := subscription.NewBudgetService(db)
	quotaService := quota.NewService(db)
	llmRuntime, err := newLLMRuntime(context.Background(), modelConfigService, usageTracker, subscriptionService, budgetService, quotaService, metrics, cfg.LLMDefaults)
	if err != nil {
		usageTracker.Stop()
		return nil, err
	}
	// Wire the agents-table fallback. Without this, SyncUser/SyncAll
	// only read user_model_configs; agents whose model was set via the
	// agent editor (which writes the agents row directly) silently
	// fall through to the platform default provider — the P2 root
	// cause. Resync once now so the fallback is in effect from the
	// first request.
	llmRuntime.SetAgentRepo(repository.NewAgentRepo(db))
	if err := llmRuntime.SyncAll(context.Background()); err != nil {
		usageTracker.Stop()
		return nil, fmt.Errorf("llm runtime: resync after agentRepo wiring: %w", err)
	}

	marketDataService := marketdata.NewService(cfg.MarketData).WithTranslator(marketdata.NewTranslator(cfg.NewsTranslator))
	// Boot the Binance / Coinbase websocket streamers (no-op when
	// CryptoWSEnabled=false). Uses context.Background so the streams live
	// for the process lifetime; Services.Stop calls Close to unwind.
	marketDataService.StartCryptoStreams(context.Background())
	workflowService := newWorkflowServiceAdapter(db, subscriptionService, metrics, marketDataService).
		WithLLMRuntime(llmRuntime).
		WithQuotaService(quotaService)

	// Distributed leader election: only one replica drives the schedulers.
	// Lease ttl=30s, renew every 10s. Failover bounded by ttl on crash.
	leaseManager := scheduler.NewLeaseManager(db, "", 30*time.Second, 10*time.Second)
	leaseManager.Register(SchedulerLeaseName)
	leaseManager.Register(MarketplaceReconcilerLeaseName)
	leaseManager.Register(AuctionSettlementLeaseName)
	leaseManager.Register(ActivityRetentionLeaseName)
	leaseManager.Register(PositionQuoteRefreshLeaseName)
	leaseManager.Register(LessonLineageLeaseName)
	leaseManager.Register(MemoryArchiveLeaseName)
	leaseManager.Register(MemoryEmbedLeaseName)

	workflowService.scheduler.SetLeaderChecker(leaseManager)
	workflowService.StartBackgroundScheduler()

	// F12: rehydrate workflow_runs that were still in flight when the
	// previous process died. Runs as a goroutine so initServices doesn't
	// block on the leader-election grace period; the helper polls the
	// lease and executes recovery exactly once on the leader replica.
	go workflowService.runRecoveryWhenLeader(context.Background(), leaseManager, 5*time.Second)

	marketplaceReconciler := newMarketplaceReconcilerLoop(marketplace.NewReconciler(db, repository.NewMarketplaceRepo(db)), metrics)
	marketplaceReconciler.SetLeaderChecker(leaseManager)
	marketplaceReconciler.Start()

	// Mailer: prefer real SMTP when configured, fall back to an
	// in-memory recorder in dev so /forgot-password still completes
	// with the link/code logged out for click-through testing.
	var mailerInstance mailer.Mailer
	if cfg.Mailer.Enabled() {
		mailerInstance = mailer.NewSMTPSender(cfg.Mailer)
		slog.Info("mailer: SMTP enabled", "host", cfg.Mailer.Host, "port", cfg.Mailer.Port)
	} else {
		mailerInstance = &mailer.Recorder{}
		slog.Warn("mailer: SMTP not configured, using in-memory recorder (dev only)")
	}

	services := &Services{
		DB:                    db,
		SubscriptionService:   subscriptionService,
		UsageTracker:          usageTracker,
		LLMRuntime:            llmRuntime,
		Metrics:               metrics,
		WorkflowService:       workflowService,
		MarketplaceReconciler: marketplaceReconciler,
		LeaseManager:          leaseManager,
		MarketDataService:     marketDataService,
		Mailer:                mailerInstance,
		SubscriptionHandler: api.NewSubscriptionHandler(
			newSubscriptionServiceAdapter(subscriptionService),
			newUsageTrackerAdapter(usageTracker),
			newModelConfigServiceAdapter(modelConfigService, llmRuntime),
			llmRuntime,
			newWalletServiceAdapter(db),
		),
	}
	marketplaceAdapter := newMarketplaceServiceAdapter(db, modelConfigService, subscriptionService, llmRuntime)
	auctionAdapter := newMarketplaceAuctionAdapter(marketplaceAdapter)
	services.FundHandler = api.NewFundHandler(
		newFundServiceAdapter(db, workflowService),
		newTeamServiceAdapter(db, usageTracker, modelConfigService, subscriptionService, llmRuntime).WithActivityBus(workflowService.activityBus),
		newPlanServiceAdapter(db, workflowService, llmRuntime),
		newTradeServiceAdapter(db).WithMarketData(marketDataService),
		workflowService,
		newMemoryServiceAdapter(db),
		newDecisionTraceServiceAdapter(db, marketDataService, llmRuntime),
		newMarketServiceAdapter(db, marketDataService, llmRuntime),
		newABTestServiceAdapter(db),
		marketplaceAdapter,
	).WithReflectionService(newReflectionServiceAdapter(db)).
		WithAgentSkillService(newAgentSkillServiceAdapter(db)).
		WithAuctionService(auctionAdapter).
		WithBacktestService(buildBacktestService(db, llmRuntime))
	// Phase 3A-5: a SINGLE attribution adapter feeds both the HTTP
	// surface (GET /api/funds/:id/strategy-attribution) and the
	// daily-review hook (runDailyAttribution inside the memory
	// system). Sharing the instance keeps the data consistent and
	// halves the per-process repo footprint.
	attributionAdapter := newAttributionServiceAdapter(db)
	services.FundHandler = services.FundHandler.WithAttributionService(attributionAdapter)
	workflowService = workflowService.WithAttributionService(attributionAdapter.Service())

	// Phase 2J/K/L: strategy promotion lifecycle.
	// The promotion adapter is wired only when persistence is
	// available — without a DB there's no meaningful baseline /
	// audit trail to maintain. Without it, the API endpoints
	// degrade to 503 / empty list, matching the rest of the
	// nil-safe pattern in the platform.
	promotionAdapter := newPromotionServiceAdapter(
		db,
		repository.NewBacktestRepo(db),
		buildLiveMetricsLookup(db),
	)
	if promotionAdapter != nil {
		// Wire the resolver back into the adapter so transitions
		// invalidate per-fund cache entries used by the PMAgent.
		resolver := promotion.NewResolver(
			promotionAdapter.Service(),
			buildDefaultEngineLookup(db),
		)
		promotionAdapter.WithResolver(resolver)
		services.PromotionAdapter = promotionAdapter
		services.PromotionResolver = resolver
		services.FundHandler = services.FundHandler.WithPromotionService(promotionAdapter)

		// Phase 2L decay-monitor scheduler: samples every live
		// promotion at the configured cadence (default daily). The
		// loop short-circuits when not the leader to keep the
		// downgrade decision single-writer across HA replicas.
		decayLoop := newPromotionDecayLoop(promotionAdapter.Decay())
		decayLoop.SetLeaderChecker(leaseManager)
		decayLoop.Start()
		services.PromotionDecayLoop = decayLoop
	}

	auctionSettlement := newAuctionSettlementLoop(auctionAdapter)
	auctionSettlement.SetLeaderChecker(leaseManager)
	auctionSettlement.Start()
	services.AuctionSettlement = auctionSettlement

	// Activity retention sweep: deletes workflow_activity_events older
	// than each fund's configured retentionDays so the Team Live
	// Activity table doesn't grow unboundedly. Safe to skip when the
	// activity repo isn't wired (test paths) — the persistence layer
	// just won't be installed and the bus stays in-memory only.
	if workflowService.activityRepo != nil {
		activityRetention := newActivityRetentionLoop(repository.NewFundRepo(db), workflowService.activityRepo)
		activityRetention.SetLeaderChecker(leaseManager)
		activityRetention.Start()
		services.ActivityRetention = activityRetention
	}

	// PR-3 position-quote refresher: keeps `holding_positions.current_price`
	// + market_value + unrealized_pnl in sync with the latest upstream
	// tick so the dashboard never displays a multi-hour-old price. Safe
	// to skip when the market-data service is disabled (single-binary
	// smoke runs) — the loop short-circuits each pass with a debug log.
	if marketDataService != nil && marketDataService.Enabled() {
		positionRefresher := newPositionQuoteRefresher(
			repository.NewFundRepo(db),
			repository.NewPositionRepo(db),
			marketDataService,
		)
		positionRefresher.SetLeaderChecker(leaseManager)
		// Phase 3A-2: feed every refresh through the lot ledger
		// so per-lot highest_price_seen / lowest_price_seen stay
		// current. Trailing stops + closed_lots MFE/MAE both
		// depend on these extremes being populated by the time
		// the next decision slot runs.
		positionRefresher.SetLotRepo(repository.NewLotRepo(db))
		// Allow operators to dial cadence at runtime without code
		// changes. Defaults match the plan: 30s / 5min.
		positionRefresher.SetIntervals(
			envDuration("MARKETDATA_QUOTE_REFRESH_INTERVAL_INSESSION", 30*time.Second),
			envDuration("MARKETDATA_QUOTE_REFRESH_INTERVAL_OFFSESSION", 5*time.Minute),
		)
		if metrics != nil {
			positionRefresher.SetMetrics(metrics)
		}
		positionRefresher.Start()
		services.PositionQuoteRefresher = positionRefresher
	}

	// Sprint 3 / M1 lesson lineage scoring + M4 memory archive.
	// Both are leader-gated, daily-ish cadence, soft-failing.
	// Wired only when DB is present (every prod path) — skipping
	// in tests / smoke runs that don't have a DB.
	if db != nil {
		lessonScoring := newLessonScoringLoop(db)
		lessonScoring.SetLeaderChecker(leaseManager)
		lessonScoring.Start()
		services.LessonScoringLoop = lessonScoring

		memoryArchive := newMemoryArchiveLoop(db)
		memoryArchive.SetLeaderChecker(leaseManager)
		memoryArchive.Start()
		services.MemoryArchiveLoop = memoryArchive

		// L3: pgvector backfill + read service. Only spins up when
		// OPENAI_API_KEY (or an OpenAI-compatible drop-in) is
		// configured. Without it, embedding column stays NULL and
		// recall.Service short-circuits to nil — existing behavior
		// unchanged.
		if apiKey := strings.TrimSpace(cfg.RecallEmbed.APIKey); apiKey != "" {
			embedder := recall.NewOpenAIEmbedder(apiKey)
			if cfg.RecallEmbed.BaseURL != "" {
				embedder.BaseURL = cfg.RecallEmbed.BaseURL
			}
			if cfg.RecallEmbed.Model != "" {
				embedder.ModelID = cfg.RecallEmbed.Model
			}
			memoryEmbed := newMemoryEmbedLoop(db, embedder)
			memoryEmbed.SetLeaderChecker(leaseManager)
			memoryEmbed.Start()
			services.MemoryEmbedLoop = memoryEmbed
			recallSvc := recall.New(db)
			workflowService.WithSemanticRecall(recallSvc, embedder)
			slog.Info("memory embed loop enabled", "model", embedder.Model())
		} else {
			slog.Info("memory embed loop disabled (no OPENAI_API_KEY)")
		}
	}

	// One-shot lot backfill at boot. Funds that held positions
	// before migration 038 (lot ledger) ran will otherwise never
	// produce closed_lots rows on a sell — their attribution
	// signal would stay dark forever. Running it on every boot is
	// safe because the underlying SQL is gated by NOT EXISTS so
	// re-runs after the first pass are a no-op. Soft failure: a
	// backfill stall mustn't block server startup, so we log and
	// move on.
	go func() {
		backfillCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stats, err := lotbackfill.New(db, slog.Default()).Run(backfillCtx)
		if err != nil {
			slog.Warn("lotbackfill: startup backfill failed",
				"err", err,
			)
			return
		}
		slog.Info("lotbackfill: startup backfill complete",
			"inserted", stats.Inserted,
			"skipped_no_buy_trade", stats.Skipped,
		)
	}()

	return services, nil
}

// ---------------------------------------------------------------------------
// HTTP Router
// ---------------------------------------------------------------------------

func buildRouter(svc *Services, cfg *Config) http.Handler {
	mux := http.NewServeMux()
	adminHandler := newAdminHandler(svc)

	// ---- Health & meta ----
	mux.HandleFunc("GET /api/health", handleHealth(svc))
	mux.HandleFunc("GET /api/version", handleVersion())
	mux.HandleFunc("GET /api/metrics", handleMetrics(svc))
	mux.HandleFunc("POST /api/auth/register", handleRegister(svc, cfg))
	mux.HandleFunc("POST /api/auth/login", handleLogin(svc, cfg))
	mux.HandleFunc("POST /api/auth/wechat-login", handleWechatLogin(svc, cfg))
	mux.HandleFunc("POST /api/auth/logout", handleLogout(cfg))
	mux.HandleFunc("GET /api/auth/session", handleSession(svc, cfg))
	mux.HandleFunc("POST /api/auth/send-verification", handleSendVerification(svc, cfg))
	mux.HandleFunc("POST /api/auth/verify-email", handleVerifyEmail(svc, cfg))
	mux.HandleFunc("POST /api/auth/forgot-password", handleForgotPassword(svc, cfg))
	mux.HandleFunc("POST /api/auth/reset-password", handleResetPassword(svc, cfg))
	mux.HandleFunc("POST /api/auth/change-password", handleChangePassword(svc, cfg))
	mux.HandleFunc("GET /api/account/kyc", handleGetAccountKYC(svc))
	mux.HandleFunc("POST /api/account/kyc", handleSubmitAccountKYC(svc))

	// Sprint 4 / android-core: FCM device-token registry + push
	// fan-out hook for terminal plan transitions.
	deviceTokens := newDeviceTokensService(svc.DB)
	mux.HandleFunc("POST /api/devices/register", deviceTokens.handleRegister)
	mux.HandleFunc("POST /api/devices/unregister", deviceTokens.handleUnregister)
	if svc.WorkflowService != nil {
		svc.WorkflowService.WithPlanLifecycleNotifier(newPlanLifecycleNotifierAdapter(deviceTokens))
	}

	// ---- Real application routes ----
	if svc.SubscriptionHandler != nil {
		svc.SubscriptionHandler.RegisterRoutes(mux)
	}
	if svc.FundHandler != nil {
		svc.FundHandler.RegisterRoutes(mux)
	}
	if adminHandler != nil {
		adminHandler.RegisterRoutes(mux)
	}

	// ---- SPA fallback: serve React static files ----
	spa := spaHandler(cfg.StaticFilesPath)
	mux.Handle("/", spa)

	// Wrap with middleware.
	var handler http.Handler = mux
	handler = pathAliasMiddleware(handler)
	handler = authMiddlewareWithKeyring(svc.DB, cfg.effectiveJWTKeyring())(handler)
	handler = corsMiddleware(cfg.CORSOrigins)(handler)
	handler = requestLogger(svc.Metrics, handler)
	handler = recoverer(svc.Metrics, handler)

	return handler
}

// pathAliasMiddleware rewrites kebab-case API path variants to the canonical
// camelCase the handlers are registered under. Lets callers that learned the
// kebab-case spelling from older docs / informal references hit the right
// handler instead of bouncing off F9.3's JSON 404. Keep this list minimal —
// only add aliases for paths where ambiguity has actually caused confusion.
func pathAliasMiddleware(next http.Handler) http.Handler {
	aliases := map[string]string{
		"/api/ab-tests": "/api/abtests",
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for from, to := range aliases {
			if r.URL.Path == from || strings.HasPrefix(r.URL.Path, from+"/") {
				r.URL.Path = to + strings.TrimPrefix(r.URL.Path, from)
				break
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handleHealth(svc *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		httpCode := http.StatusOK
		dependencies := map[string]any{
			"database":      map[string]any{"status": "ok"},
			"llm_runtime":   map[string]any{"status": "ok"},
			"usage_tracker": map[string]any{"status": "ok"},
		}

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if svc.DB == nil {
			status = "degraded"
			httpCode = http.StatusServiceUnavailable
			dependencies["database"] = map[string]any{
				"status": "degraded",
				"error":  "database unavailable",
			}
		} else if err := svc.DB.PingContext(ctx); err != nil {
			status = "degraded"
			httpCode = http.StatusServiceUnavailable
			dependencies["database"] = map[string]any{
				"status": "degraded",
				"error":  err.Error(),
			}
		}
		if svc.LLMRuntime == nil {
			status = "degraded"
			httpCode = http.StatusServiceUnavailable
			dependencies["llm_runtime"] = map[string]any{
				"status": "degraded",
				"error":  "runtime unavailable",
			}
		}
		if svc.UsageTracker == nil {
			status = "degraded"
			httpCode = http.StatusServiceUnavailable
			dependencies["usage_tracker"] = map[string]any{
				"status": "degraded",
				"error":  "tracker unavailable",
			}
		}

		writeJSON(w, httpCode, map[string]any{
			"status":       status,
			"time":         time.Now().UTC().Format(time.RFC3339),
			"version":      version,
			"dependencies": dependencies,
		})
	}
}

func handleVersion() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"version":      version,
			"build_time":   buildTime,
			"build_commit": buildCommit,
			"go_version":   runtime.Version(),
		})
	}
}

func handleMetrics(svc *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || svc.Metrics == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("metrics unavailable\n"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(svc.Metrics.ExportPrometheus()))
		_, _ = w.Write([]byte(exportRuntimePrometheus(svc.DB, svc.LeaseManager)))
		_, _ = w.Write([]byte(exportMarketDataPrometheus(svc.MarketDataService)))
	}
}

const (
	userRoleSuperAdmin = "super_admin"
	userRoleUser       = "user"
	userStatusActive   = "active"
)

const authSerializationRetries = 3

type authRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`
}

type authenticatedUser struct {
	ID           string
	Email        string
	DisplayName  string
	Role         string
	Status       string
	PasswordHash string
	KYCStatus    string
	KYCLevel     string
}

func handleRegister(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "detail": "database unavailable", "request_id": requestID})
			return
		}
		input, ok := decodeAuthRequest(w, r, requestID)
		if !ok {
			return
		}
		normalizedEmail, ok := validateEmail(w, input.Email, requestID)
		if !ok {
			return
		}
		password, ok := validatePassword(w, input.Password, requestID)
		if !ok {
			return
		}
		displayName := strings.TrimSpace(input.DisplayName)
		if displayName == "" {
			displayName = strings.Split(normalizedEmail, "@")[0]
		}
		passwordHash, err := hashPassword(password)
		if err != nil {
			slog.Error("failed to hash password", "request_id", requestID, "email", normalizedEmail, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to create account", "request_id": requestID})
			return
		}
		user, err := registerUser(r.Context(), svc.DB, normalizedEmail, passwordHash, displayName)
		if err != nil {
			status := http.StatusInternalServerError
			message := "failed to create account"
			detail := "注册失败，请稍后再试。"
			if errors.Is(err, errEmailAlreadyExists) {
				status = http.StatusConflict
				message = "email already exists"
				detail = "该邮箱已被注册，请直接登录。"
			}
			writeJSON(w, status, map[string]any{"error": message, "detail": detail, "request_id": requestID})
			return
		}
		writeAuthSuccess(w, cfg, requestID, user)
	}
}

func handleLogin(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		input, ok := decodeAuthRequest(w, r, requestID)
		if !ok {
			return
		}
		normalizedEmail, ok := validateEmail(w, input.Email, requestID)
		if !ok {
			return
		}
		if strings.TrimSpace(input.Password) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid password", "detail": "密码不能为空。", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "detail": "database unavailable", "request_id": requestID})
			return
		}
		user, err := authenticateUserByPassword(r.Context(), svc.DB, normalizedEmail, input.Password)
		if err != nil {
			status := http.StatusInternalServerError
			message := "login failed"
			detail := "登录失败，请稍后再试。"
			if errors.Is(err, errAccountLocked) {
				status = http.StatusTooManyRequests
				message = "account locked"
				detail = "账号已临时锁定，请稍后再试或重置密码。"
			} else if errors.Is(err, errInvalidCredentials) {
				status = http.StatusUnauthorized
				message = "invalid credentials"
				detail = "邮箱或密码错误。"
			}
			writeJSON(w, status, map[string]any{"error": message, "detail": detail, "request_id": requestID})
			return
		}
		writeAuthSuccess(w, cfg, requestID, user)
	}
}

func writeAuthSuccess(w http.ResponseWriter, cfg *Config, requestID string, user *authenticatedUser) {
	// F29: new tokens carry the active kid so future verifiers can route
	// to the right key after a rotation. effectiveJWTKeyring synthesises
	// a single-key ring from cfg.JWTSecret when no explicit ring is
	// configured (e.g. legacy test setups).
	activeSecret, activeKid := cfg.JWTSecret, ""
	if ring := cfg.effectiveJWTKeyring(); ring != nil {
		k := ring.Active()
		activeSecret, activeKid = k.Secret, k.Kid
	}
	token, expiresAt, err := issueSessionTokenWithKid(user.ID, activeSecret, activeKid, cfg.SessionTTL)
	if err != nil {
		slog.Error("failed to issue session token", "request_id", requestID, "user_id", user.ID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to create session", "request_id": requestID})
		return
	}
	setSessionCookie(w, token, expiresAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        token,
		"user_id":      user.ID,
		"email":        user.Email,
		"display_name": user.DisplayName,
		"role":         user.Role,
		"kyc_status":   user.KYCStatus,
		"kyc_level":    user.KYCLevel,
		"expires_at":   expiresAt.UTC().Format(time.RFC3339),
		"request_id":   requestID,
	})
}

func decodeAuthRequest(w http.ResponseWriter, r *http.Request, requestID string) (*authRequest, bool) {
	if r.Body == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": "请求体不能为空。", "request_id": requestID})
		return nil, false
	}
	var input authRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": "请求体必须是合法 JSON。", "request_id": requestID})
		return nil, false
	}
	return &input, true
}

func validateEmail(w http.ResponseWriter, rawEmail string, requestID string) (string, bool) {
	normalized := normalizeEmail(rawEmail)
	if normalized == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid email", "detail": "邮箱不能为空。", "request_id": requestID})
		return "", false
	}
	if _, err := mail.ParseAddress(normalized); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid email", "detail": "请输入合法邮箱地址。", "request_id": requestID})
		return "", false
	}
	return normalized, true
}

func validatePassword(w http.ResponseWriter, rawPassword string, requestID string) (string, bool) {
	password := strings.TrimSpace(rawPassword)
	if len(password) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid password", "detail": "密码长度至少为 8 位。", "request_id": requestID})
		return "", false
	}
	return password, true
}

func handleLogout(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		clearSessionCookie(w)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "logged_out",
			"request_id": requestID,
		})
	}
}

func handleSession(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		token := resolveSessionToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"authenticated": false,
				"error":         "missing session",
				"detail":        "当前没有有效登录会话。",
				"request_id":    requestID,
			})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"authenticated": false,
				"error":         "service unavailable",
				"detail":        "database unavailable",
				"request_id":    requestID,
			})
			return
		}
		claims, err := validateJWTWithKeyring(token, cfg.effectiveJWTKeyring())
		if err != nil {
			clearSessionCookie(w)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"authenticated": false,
				"error":         "invalid session",
				"detail":        "当前登录会话已失效，请重新登录。",
				"request_id":    requestID,
			})
			return
		}
		user, err := loadActiveUserByID(r.Context(), svc.DB, claims.Subject)
		if err != nil {
			clearSessionCookie(w)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"authenticated": false,
				"error":         "invalid session",
				"detail":        "当前登录会话无效或用户不存在。",
				"request_id":    requestID,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"user_id":       user.ID,
			"email":         user.Email,
			"display_name":  user.DisplayName,
			"role":          user.Role,
			"kyc_status":    user.KYCStatus,
			"kyc_level":     user.KYCLevel,
			"expires_at":    time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339),
			"request_id":    requestID,
		})
	}
}

type accountKYCApplication struct {
	ID                string    `json:"id"`
	UserID            string    `json:"user_id"`
	KYCLevel          string    `json:"kyc_level"`
	Status            string    `json:"status"`
	FullName          string    `json:"full_name"`
	IDDocumentType    string    `json:"id_document_type"`
	IDDocumentNumber  string    `json:"id_document_number"`
	DocumentImageURLs []string  `json:"document_image_urls,omitempty"`
	RejectionReason   string    `json:"rejection_reason,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func handleGetAccountKYC(svc *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		userID, ok := api.AuthenticatedUserID(r)
		if !ok || strings.TrimSpace(userID) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing authenticated user", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "detail": "database unavailable", "request_id": requestID})
			return
		}
		user, err := loadActiveUserByID(r.Context(), svc.DB, userID)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid user", "request_id": requestID})
			return
		}
		applications, err := loadAccountKYCApplications(r.Context(), svc.DB, userID, 10)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load kyc applications", "detail": err.Error(), "request_id": requestID})
			return
		}
		safeAuditLogAccess(r.Context(), audit.NewDBLogger(svc.DB), userID, "read", "account_kyc", userID, map[string]any{
			"kyc_status":        user.KYCStatus,
			"kyc_level":         user.KYCLevel,
			"application_count": len(applications),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"kyc_status":   user.KYCStatus,
			"kyc_level":    user.KYCLevel,
			"applications": applications,
			"request_id":   requestID,
		})
	}
}

func handleSubmitAccountKYC(svc *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		userID, ok := api.AuthenticatedUserID(r)
		if !ok || strings.TrimSpace(userID) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing authenticated user", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "detail": "database unavailable", "request_id": requestID})
			return
		}
		defer r.Body.Close()

		var payload struct {
			KYCLevel          string   `json:"kyc_level"`
			FullName          string   `json:"full_name"`
			IDDocumentType    string   `json:"id_document_type"`
			IDDocumentNumber  string   `json:"id_document_number"`
			DocumentImageURLs []string `json:"document_image_urls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": "请求体必须是合法 JSON。", "request_id": requestID})
			return
		}
		payload.KYCLevel = strings.TrimSpace(payload.KYCLevel)
		if payload.KYCLevel == "" {
			payload.KYCLevel = "tier1_basic"
		}
		if !validKYCLevel(payload.KYCLevel) || !validKYCDocumentType(payload.IDDocumentType) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid kyc payload", "detail": "KYC 等级或证件类型无效。", "request_id": requestID})
			return
		}
		fullName := strings.TrimSpace(payload.FullName)
		documentNumber := strings.TrimSpace(payload.IDDocumentNumber)
		if fullName == "" || documentNumber == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing kyc fields", "detail": "姓名和证件号码不能为空。", "request_id": requestID})
			return
		}

		user, err := loadActiveUserByID(r.Context(), svc.DB, userID)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid user", "request_id": requestID})
			return
		}
		if user.KYCStatus == "verified" && kycLevelRank(user.KYCLevel) >= kycLevelRank(payload.KYCLevel) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "kyc_already_verified", "detail": "当前账号已具备该等级或更高等级的 KYC 认证。", "request_id": requestID})
			return
		}
		var pendingCount int
		if err := svc.DB.QueryRowContext(r.Context(), `SELECT COUNT(1) FROM user_kyc_records WHERE user_id = $1 AND status = 'pending'`, userID).Scan(&pendingCount); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "database error", "detail": err.Error(), "request_id": requestID})
			return
		}
		if pendingCount > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "kyc_pending", "detail": "当前已有待审核的 KYC 申请。", "request_id": requestID})
			return
		}

		documentJSON, err := json.Marshal(payload.DocumentImageURLs)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid document urls", "request_id": requestID})
			return
		}
		tx, err := svc.DB.BeginTx(r.Context(), nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "transaction start failed", "request_id": requestID})
			return
		}
		defer tx.Rollback()

		var application accountKYCApplication
		err = tx.QueryRowContext(r.Context(), `
			INSERT INTO user_kyc_records (user_id, kyc_level, status, full_name, id_document_type, id_document_number, document_image_urls)
			VALUES ($1, $2, 'pending', $3, $4, $5, $6)
			RETURNING id, user_id, kyc_level, status, full_name, id_document_type, id_document_number, COALESCE(rejection_reason, ''), created_at, updated_at
		`, userID, payload.KYCLevel, fullName, strings.TrimSpace(payload.IDDocumentType), documentNumber, documentJSON).
			Scan(&application.ID, &application.UserID, &application.KYCLevel, &application.Status, &application.FullName, &application.IDDocumentType, &application.IDDocumentNumber, &application.RejectionReason, &application.CreatedAt, &application.UpdatedAt)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to create kyc application", "detail": err.Error(), "request_id": requestID})
			return
		}
		application.DocumentImageURLs = parseKYCImageURLs(documentJSON)
		if _, err := tx.ExecContext(r.Context(), `UPDATE users SET kyc_status = CASE WHEN kyc_status = 'verified' THEN kyc_status ELSE 'pending' END, updated_at = NOW() WHERE id = $1`, userID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update kyc status", "detail": err.Error(), "request_id": requestID})
			return
		}
		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "transaction commit failed", "request_id": requestID})
			return
		}
		safeAuditLogAccess(r.Context(), audit.NewDBLogger(svc.DB), userID, "submit", "kyc_application", application.ID, map[string]any{
			"kyc_level":           application.KYCLevel,
			"id_document_type":    application.IDDocumentType,
			"document_urls_count": len(payload.DocumentImageURLs),
			"previous_kyc_status": user.KYCStatus,
			"previous_kyc_level":  user.KYCLevel,
		})
		writeJSON(w, http.StatusCreated, map[string]any{"application": application, "request_id": requestID})
	}
}

func loadAccountKYCApplications(ctx context.Context, db *sql.DB, userID string, limit int) ([]accountKYCApplication, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, kyc_level, status, full_name, id_document_type, id_document_number,
		       COALESCE(document_image_urls, '[]'::jsonb), COALESCE(rejection_reason, ''), created_at, updated_at
		FROM user_kyc_records
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applications := make([]accountKYCApplication, 0)
	for rows.Next() {
		var application accountKYCApplication
		var documentURLs json.RawMessage
		if err := rows.Scan(&application.ID, &application.UserID, &application.KYCLevel, &application.Status, &application.FullName, &application.IDDocumentType, &application.IDDocumentNumber, &documentURLs, &application.RejectionReason, &application.CreatedAt, &application.UpdatedAt); err != nil {
			return nil, err
		}
		application.DocumentImageURLs = parseKYCImageURLs(documentURLs)
		applications = append(applications, application)
	}
	return applications, rows.Err()
}

func validKYCLevel(level string) bool {
	switch strings.TrimSpace(level) {
	case "tier1_basic", "tier2_advanced", "tier3_enterprise":
		return true
	default:
		return false
	}
}

func validKYCDocumentType(documentType string) bool {
	switch strings.TrimSpace(documentType) {
	case "id_card", "passport", "driver_license":
		return true
	default:
		return false
	}
}

func kycLevelRank(level string) int {
	switch strings.TrimSpace(level) {
	case "tier3_enterprise":
		return 3
	case "tier2_advanced":
		return 2
	case "tier1_basic":
		return 1
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// SPA file server
// ---------------------------------------------------------------------------

// spaHandler serves static files from dir and falls back to index.html
// for any path that doesn't match a real file (client-side routing).
//
// API paths (anything starting with /api/) that fall through to this handler
// are unmatched routes and must return a JSON 404, NOT the SPA HTML. Without
// this guard, a typo'd API path silently returns 200 + index.html which is
// near-impossible to debug from a curl/fetch caller. The SSE event-stream
// path also needs to be excluded so misrouted /events requests fail loudly.
//
// Static asset paths (anything with a file extension like .js, .css, .png)
// that don't resolve to a real file must also 404 instead of falling back
// to index.html. Otherwise the browser's dynamic `import()` sees a 200 +
// text/html response and throws "Failed to fetch dynamically imported
// module" — which is exactly what happens when a user keeps a stale tab
// open across a frontend rebuild: the old entry chunk lazy-imports a
// hashed chunk name that no longer exists, and the SPA fallback masks
// the 404 as a misleading "module" failure.
func spaHandler(dir string) http.Handler {
	fsys := http.Dir(dir)
	fileServer := http.FileServer(fsys)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPILikePath(r.URL.Path) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error":  "not_found",
				"path":   r.URL.Path,
				"method": r.Method,
			})
			return
		}

		path := filepath.Clean(r.URL.Path)
		if path == "/" {
			path = "/index.html"
		}

		f, err := fsys.Open(path)
		if err != nil {
			// Asset-style paths (anything with a file extension) must 404
			// — falling back to index.html corrupts module imports.
			if isStaticAssetPath(path) {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"error": "not_found",
					"path":  r.URL.Path,
				})
				return
			}
			r.URL.Path = "/"
			setSPACacheHeaders(w, "/index.html")
			fileServer.ServeHTTP(w, r)
			return
		}
		_ = f.Close()

		setSPACacheHeaders(w, path)
		fileServer.ServeHTTP(w, r)
	})
}

// isAPILikePath returns true for request paths that must never serve the SPA
// fallback. Includes the public /api/ surface plus the streaming /events/
// surface used for SSE.
func isAPILikePath(path string) bool {
	return strings.HasPrefix(path, "/api/") ||
		path == "/api" ||
		strings.HasPrefix(path, "/events/") ||
		path == "/events"
}

// isStaticAssetPath reports whether `path` looks like a static asset request
// (i.e. the last path segment has a file extension). React-router paths are
// extension-less ("/", "/dashboard", "/funds/abc/settings") so they correctly
// route to the SPA fallback; "/assets/foo-HASH.js", "/favicon.ico", and
// "/vite.svg" all match and so return real 404s when missing.
func isStaticAssetPath(path string) bool {
	last := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		last = path[i+1:]
	}
	if last == "" {
		return false
	}
	return strings.Contains(last, ".")
}

// setSPACacheHeaders writes Cache-Control headers tuned for a Vite-style
// build:
//
//   - Hashed chunks under /assets/ are content-addressed (filename embeds
//     the rolldown hash), so they can be cached aggressively forever.
//     `immutable` tells the browser it never even needs to revalidate.
//   - index.html must not be cached: it's the entry that pins the current
//     hashed chunk names, and a stale copy is precisely how users end up
//     loading deleted chunks after a rebuild. `no-cache` forces a
//     revalidation hop (ETag-based) on every request so users pick up the
//     new entry as soon as we redeploy.
//   - Everything else gets a short, conservative default.
func setSPACacheHeaders(w http.ResponseWriter, path string) {
	switch {
	case strings.HasPrefix(path, "/assets/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case path == "/index.html" || strings.HasSuffix(path, "/index.html"):
		w.Header().Set("Cache-Control", "no-cache")
	default:
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.TrimSpace(o)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if allowed[origin] || allowed["*"] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Request-ID,X-User-Language")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type responseRecorder struct {
	http.ResponseWriter
	status       int
	bytesWritten int
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status != 0 {
		return
	}
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytesWritten += n
	return n, err
}

// Flush forwards to the underlying ResponseWriter when it implements
// http.Flusher. The Team Live Activity SSE handler (and any future
// streaming endpoint) type-asserts the response writer to http.Flusher
// to push incremental frames; without this passthrough the assertion
// would fail and the handler returns 500 "sse unsupported", which
// looked to the user like a permanent "连接已断开，正在自动重连" loop.
//
// We intentionally do NOT call WriteHeader(200) here — SSE handlers
// set the status code themselves before the first flush, and the
// double WriteHeader guard in our own implementation would swallow it.
func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		if r.status == 0 {
			r.status = http.StatusOK
		}
		flusher.Flush()
	}
}

// Hijack lets WebSocket / connection-upgrade handlers take over the
// underlying TCP connection. Mirrors the Flush() rationale: a wrapping
// middleware that captures status/bytes must transparently expose
// hijack capability so handlers like net/http's WebSocket upgrade or
// `http.ResponseController` keep working.
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not implement http.Hijacker")
	}
	// Once a connection is hijacked the response is owned by the
	// caller, so any subsequent recorder Write/Flush would be a bug.
	// We mark the status as 101 (Switching Protocols) so request logs
	// reflect what actually happened on the wire.
	if r.status == 0 {
		r.status = http.StatusSwitchingProtocols
	}
	return hijacker.Hijack()
}

func requestLogger(metrics *serverMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api") {
			next.ServeHTTP(w, r)
			return
		}

		requestID := ensureRequestID(w, r)
		traceID := requestTraceID(r)
		spanID := newTraceSegmentID()
		language := normalizeUserLanguage(firstNonEmpty(r.Header.Get(userLanguageHeader), r.Header.Get("Accept-Language")))
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		ctx = context.WithValue(ctx, traceIDKey, traceID)
		ctx = context.WithValue(ctx, spanIDKey, spanID)
		ctx = context.WithValue(ctx, userLanguageKey, language)
		ctx = marketdata.WithLanguage(ctx, string(language))
		r = r.WithContext(ctx)
		w.Header().Set(traceIDHeader, traceID)
		w.Header().Set(spanIDHeader, spanID)
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		userID, _ := api.AuthenticatedUserID(r)
		userRole, _ := api.AuthenticatedUserRole(r)
		duration := time.Since(start)
		if metrics != nil {
			metrics.ObserveHTTP(r.Method, r.URL.Path, recorder.status, duration)
		}
		slog.Info("request",
			"request_id", requestID,
			"trace_id", traceID,
			"span_id", spanID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", duration.Milliseconds(),
			"bytes", recorder.bytesWritten,
			"remote", r.RemoteAddr,
			"user_id", userID,
			"user_role", userRole,
		)
	})
}

func recoverer(metrics *serverMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				requestID := requestIDFromContext(r.Context())
				if requestID == "" {
					requestID = ensureRequestID(w, r)
				}
				traceID := traceIDFromContext(r.Context())
				if traceID == "" {
					traceID = requestTraceID(r)
				}
				if metrics != nil {
					metrics.RecordPanic(r.URL.Path)
				}
				slog.Error("panic recovered", "request_id", requestID, "trace_id", traceID, "error", rec, "path", r.URL.Path)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":      "internal server error",
					"request_id": requestID,
					"trace_id":   traceID,
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// authMiddleware accepts either a JWT secret (legacy single-key tests)
// or a JWTKeyring (production, supports F29 rotation). Pass an empty
// jwtSecret + non-nil ring for production; tests can pass a string and
// nil ring for the historical path.
func authMiddleware(db *sql.DB, jwtSecret string) func(http.Handler) http.Handler {
	ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{{Kid: "default", Secret: jwtSecret, Active: true}})
	if err != nil {
		// jwtSecret==""; downstream verification will fail with a clear
		// error rather than nil-deref. Tests that exercise the auth
		// middleware always supply a non-empty secret.
		ring = nil
	}
	return authMiddlewareWithKeyring(db, ring)
}

// authMiddlewareWithKeyring is the F29 production entry point. The ring
// holds all valid verification keys; new tokens are signed with the
// active key only.
func authMiddlewareWithKeyring(db *sql.DB, ring *secrets.JWTKeyring) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isPublicRoute(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			requestID := requestIDFromContext(r.Context())
			if requestID == "" {
				requestID = ensureRequestID(w, r)
			}
			token := resolveSessionToken(r)
			if token == "" {
				slog.Warn("authentication failed", "request_id", requestID, "path", r.URL.Path, "reason", "missing_session")
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error":      "missing or invalid bearer token",
					"detail":     "当前请求缺少 Bearer Token 或登录会话 Cookie。",
					"request_id": requestID,
				})
				return
			}
			claims, err := validateJWTWithKeyring(token, ring)
			if err != nil {
				slog.Warn("authentication failed", "request_id", requestID, "path", r.URL.Path, "reason", "invalid_token", "error", err)
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error":      "missing or invalid bearer token",
					"detail":     "Bearer Token 无效、已过期或尚未生效。",
					"request_id": requestID,
				})
				return
			}

			user, err := loadActiveUserByID(r.Context(), db, claims.Subject)
			if err != nil {
				slog.Warn("failed to load authenticated user", "request_id", requestID, "path", r.URL.Path, "subject", claims.Subject, "error", err)
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error":      "missing or invalid bearer token",
					"detail":     "访问凭证已通过校验，但当前用户不存在或已停用。",
					"request_id": requestID,
				})
				return
			}

			ctx := api.WithAuthenticatedUserID(r.Context(), user.ID)
			ctx = api.WithAuthenticatedUserRole(ctx, user.Role)
			ctx = api.WithAuthenticatedUserKYC(ctx, user.KYCStatus, user.KYCLevel)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

var (
	errEmailAlreadyExists     = errors.New("email already exists")
	errInvalidCredentials     = errors.New("invalid credentials")
	errUserNotFoundOrInactive = errors.New("user not found or inactive")
	errAccountLocked          = errors.New("account temporarily locked")
)

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func hashPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

func authenticateUserByPassword(ctx context.Context, db *sql.DB, email, password string) (*authenticatedUser, error) {
	user, err := loadUserByEmail(ctx, db, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errUserNotFoundOrInactive) {
			return nil, errInvalidCredentials
		}
		return nil, err
	}
	if strings.TrimSpace(user.PasswordHash) == "" {
		return nil, errInvalidCredentials
	}
	// Login throttling. Sprint 2A: 5 misses in a row locks the
	// account for 15 minutes so credential-stuffing scripts can't
	// burn through it. We tolerate missing columns (migration 042
	// not yet applied) so this code still works against an older
	// schema during rolling deployments.
	if locked, lockedUntil := lockedAccount(ctx, db, user.ID); locked {
		slog.Warn("login blocked: account locked", "user_id", user.ID, "locked_until", lockedUntil)
		return nil, errAccountLocked
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		recordFailedLogin(ctx, db, user.ID)
		return nil, errInvalidCredentials
	}
	recordSuccessfulLogin(ctx, db, user.ID)
	return user, nil
}

func lockedAccount(ctx context.Context, db *sql.DB, userID string) (bool, time.Time) {
	if db == nil {
		return false, time.Time{}
	}
	var until sql.NullTime
	err := db.QueryRowContext(ctx, `SELECT locked_until FROM users WHERE id = $1`, userID).Scan(&until)
	if err != nil {
		return false, time.Time{}
	}
	if !until.Valid {
		return false, time.Time{}
	}
	if time.Now().UTC().After(until.Time) {
		return false, time.Time{}
	}
	return true, until.Time
}

func recordFailedLogin(ctx context.Context, db *sql.DB, userID string) {
	if db == nil {
		return
	}
	// Bump attempt counter; if we cross the threshold, set locked_until.
	// Computing the new lock window in SQL avoids a separate round-trip.
	_, err := db.ExecContext(ctx, `
		UPDATE users
		SET failed_login_attempts = failed_login_attempts + 1,
		    locked_until = CASE
		        WHEN failed_login_attempts + 1 >= $2 THEN NOW() + $3::interval
		        ELSE locked_until
		    END
		WHERE id = $1
	`, userID, loginLockThreshold, fmt.Sprintf("%d seconds", int(loginLockDuration.Seconds())))
	if err != nil {
		slog.Debug("record failed login (best-effort)", "user_id", userID, "error", err)
	}
}

func recordSuccessfulLogin(ctx context.Context, db *sql.DB, userID string) {
	if db == nil {
		return
	}
	_, err := db.ExecContext(ctx, `
		UPDATE users
		SET failed_login_attempts = 0,
		    locked_until = NULL,
		    last_login_at = NOW()
		WHERE id = $1
	`, userID)
	if err != nil {
		slog.Debug("record successful login (best-effort)", "user_id", userID, "error", err)
	}
}

func loadUserByEmail(ctx context.Context, db *sql.DB, email string) (*authenticatedUser, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	var user authenticatedUser
	err := db.QueryRowContext(ctx, `
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE LOWER(email) = LOWER($1)
		LIMIT 1
	`, email).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.Status, &user.PasswordHash, &user.KYCStatus, &user.KYCLevel)
	if err != nil {
		return nil, err
	}
	if user.Status != userStatusActive {
		return nil, errUserNotFoundOrInactive
	}
	return &user, nil
}

func loadActiveUserByID(ctx context.Context, db *sql.DB, userID string) (*authenticatedUser, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	parsedUserID, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil {
		return nil, errUserNotFoundOrInactive
	}
	var user authenticatedUser
	err = db.QueryRowContext(ctx, `
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic')
		FROM users
		WHERE id = $1
		LIMIT 1
	`, parsedUserID.String()).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.Status, &user.PasswordHash, &user.KYCStatus, &user.KYCLevel)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errUserNotFoundOrInactive
		}
		return nil, err
	}
	if user.Status != userStatusActive {
		return nil, errUserNotFoundOrInactive
	}
	return &user, nil
}

func registerUser(ctx context.Context, db *sql.DB, email, passwordHash, displayName string) (*authenticatedUser, error) {
	if db == nil {
		return nil, errors.New("database unavailable")
	}
	var lastErr error
	for attempt := 0; attempt < authSerializationRetries; attempt++ {
		user, err := registerUserOnce(ctx, db, email, passwordHash, displayName)
		if err == nil {
			return user, nil
		}
		lastErr = err
		if !isSerializationError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func registerUserOnce(ctx context.Context, db *sql.DB, email, passwordHash, displayName string) (*authenticatedUser, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE LOWER(email) = LOWER($1) LIMIT 1`, email).Scan(&existingID)
	if err == nil {
		return nil, errEmailAlreadyExists
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	role := userRoleUser
	var superAdminCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = $1`, userRoleSuperAdmin).Scan(&superAdminCount); err != nil {
		return nil, err
	}
	if superAdminCount == 0 {
		role = userRoleSuperAdmin
	}

	userID := uuid.NewString()
	username := email
	user := &authenticatedUser{
		ID:          userID,
		Email:       email,
		DisplayName: displayName,
		Role:        role,
		Status:      userStatusActive,
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO users (id, username, display_name, email, password_hash, status, role)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status
	`, userID, username, displayName, email, passwordHash, userStatusActive, role).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.Status); err != nil {
		if isUniqueViolation(err) {
			return nil, errEmailAlreadyExists
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return user, nil
}

func isSerializationError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "could not serialize")
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") || strings.Contains(message, "unique constraint")
}

func isPublicRoute(path string) bool {
	if strings.HasPrefix(path, "/api/") {
		switch path {
		case "/api/health", "/api/version", "/api/metrics", "/api/auth/register", "/api/auth/login", "/api/auth/logout", "/api/auth/session",
			"/api/auth/forgot-password", "/api/auth/reset-password", "/api/auth/wechat-login":
			return true
		default:
			return false
		}
	}
	return true
}

type jwtClaims struct {
	Subject   string
	ExpiresAt int64
	NotBefore int64
	IssuedAt  int64
}

func validateJWT(token string, secret []byte) (*jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid token format")
	}

	headerJSON, err := decodeJWTPart(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}
	if !strings.EqualFold(header.Alg, "HS256") {
		return nil, errors.New("unsupported jwt alg")
	}

	signed := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signed))
	expectedSig := mac.Sum(nil)
	actualSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	if !hmac.Equal(actualSig, expectedSig) {
		return nil, errors.New("invalid jwt signature")
	}

	payloadJSON, err := decodeJWTPart(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}

	claims := &jwtClaims{
		Subject:   firstNonEmptyString(payload, "sub", "user_id", "uid"),
		ExpiresAt: numericClaim(payload, "exp"),
		NotBefore: numericClaim(payload, "nbf"),
		IssuedAt:  numericClaim(payload, "iat"),
	}

	now := time.Now().Unix()
	if claims.ExpiresAt > 0 && now >= claims.ExpiresAt {
		return nil, errors.New("jwt expired")
	}
	if claims.NotBefore > 0 && now < claims.NotBefore {
		return nil, errors.New("jwt not active yet")
	}
	if claims.Subject == "" {
		return nil, errors.New("jwt subject missing")
	}

	return claims, nil
}

func decodeJWTPart(part string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(part)
}

func issueSessionToken(userID string, secret string, ttl time.Duration) (string, time.Time, error) {
	return issueSessionTokenWithKid(userID, secret, "", ttl)
}

// issueSessionTokenWithKid embeds the F29 kid header so verifiers can
// route the token to the correct secret in a multi-key keyring. When
// kid is empty, the header omits the field and the token verifies
// against any secret in the legacy multi-key fallback path — keeping
// backward compatibility with pre-rotation deployments.
func issueSessionTokenWithKid(userID, secret, kid string, ttl time.Duration) (string, time.Time, error) {
	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	expiresAt := now.Add(ttl)

	headerMap := map[string]any{"alg": "HS256", "typ": "JWT"}
	if strings.TrimSpace(kid) != "" {
		headerMap["kid"] = kid
	}
	headerJSON, err := json.Marshal(headerMap)
	if err != nil {
		return "", time.Time{}, err
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"sub": userID,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": expiresAt.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}

	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signed := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signed))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signed + "." + signature, expiresAt, nil
}

// validateJWTWithKeyring is the F29 verification path used by the auth
// middleware. It prefers the kid header to pick the correct secret;
// when the kid header is absent (legacy tokens issued before rotation),
// it falls back to trying every key in the keyring. Unknown kid → fail
// (NEVER fall back to active key as that breaks rotation safety).
func validateJWTWithKeyring(token string, ring *secrets.JWTKeyring) (*jwtClaims, error) {
	if ring == nil {
		return nil, errors.New("jwt keyring not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid token format")
	}
	headerJSON, err := decodeJWTPart(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}
	if !strings.EqualFold(header.Alg, "HS256") {
		return nil, errors.New("unsupported jwt alg")
	}

	if strings.TrimSpace(header.Kid) != "" {
		key, ok := ring.LookupKid(header.Kid)
		if !ok {
			return nil, fmt.Errorf("unknown jwt kid: %s", header.Kid)
		}
		return validateJWT(token, []byte(key.Secret))
	}

	// Legacy path: no kid header. Try every secret. We rely on
	// validateJWT's HMAC equality check (which uses hmac.Equal /
	// constant time) so this loop doesn't leak per-attempt timing info.
	for _, secret := range ring.LegacyVerificationKeys() {
		if claims, err := validateJWT(token, secret); err == nil {
			return claims, nil
		}
	}
	return nil, errors.New("invalid jwt signature")
}

func firstNonEmptyString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := payload[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func numericClaim(payload map[string]any, key string) int64 {
	value, ok := payload[key]
	if !ok {
		return 0
	}
	switch v := value.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey).(string)
	return strings.TrimSpace(requestID)
}

func traceIDFromContext(ctx context.Context) string {
	traceID, _ := ctx.Value(traceIDKey).(string)
	return strings.TrimSpace(traceID)
}

func spanIDFromContext(ctx context.Context) string {
	spanID, _ := ctx.Value(spanIDKey).(string)
	return strings.TrimSpace(spanID)
}

func ensureRequestID(w http.ResponseWriter, r *http.Request) string {
	requestID := requestIDFromContext(r.Context())
	if requestID == "" {
		requestID = strings.TrimSpace(r.Header.Get(requestIDHeader))
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set(requestIDHeader, requestID)
	return requestID
}

func requestTraceID(r *http.Request) string {
	traceID := traceIDFromContext(r.Context())
	if traceID == "" {
		traceID = strings.TrimSpace(r.Header.Get(traceIDHeader))
	}
	if traceID == "" {
		traceID = newTraceSegmentID()
	}
	return traceID
}

func newTraceSegmentID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

func resolveSessionToken(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return strings.TrimSpace(authHeader[len("Bearer "):])
	}
	cookie, err := r.Cookie("fundai_session")
	if err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	secure := strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
	http.SetCookie(w, &http.Cookie{
		Name:     "fundai_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	secure := strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
	http.SetCookie(w, &http.Cookie{
		Name:     "fundai_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

type serverMetrics struct {
	mu                            sync.Mutex
	httpRequestsTotal             map[string]int64
	httpRequestDurationMS         map[string]int64
	httpRequestCount              map[string]int64
	httpRequestDurationBuckets    map[string]int64
	httpRequestDurationSumSeconds map[string]float64
	panicTotal                    map[string]int64
	llmCallsTotal                 map[string]int64
	llmLatencyMS                  map[string]int64
	llmCallCount                  map[string]int64
	workflowTransitions           map[string]int64
	marketplaceReconciliations    map[string]int64
	marketplaceReconciliationLast map[string]int64
	hardRiskRejections            map[string]int64
	// Phase 3A-1 lot ledger counters. When the lot ledger fails
	// to record a fill, the trade still completes (the ledger is
	// a derived shadow) but we want operator visibility so the
	// drift can be reconciled. Failures are keyed by side+symbol
	// so the histogram surfaces "which instruments are silently
	// drifting".
	lotLedgerFailures             map[string]int64
	// PR-3 position-quote refresher counters. Exported via /api/metrics
	// as fundai_marketdata_position_refresh_total /
	// fundai_marketdata_position_refresh_rows / _duration_seconds_sum.
	positionRefreshPassesTotal   int64
	positionRefreshFailuresTotal int64
	positionRefreshRowsTotal     int64
	positionRefreshDurationMS    int64
	// Sprint D #1 — PM decision-input observability. These let
	// us answer "which signal blocks fired today" and "how often
	// is the portfolio bumping into a guardrail" at a glance.
	//
	// decisionInputBlocks keys by `block=NAME,present=true|false`
	// so dashboards can compute presence rate per block. Cardinality
	// is bounded by the fixed set of block names (18).
	//
	// decisionInputCalls is the denominator: total number of
	// PM decision inputs assembled (one per fund-day decision).
	//
	// decisionExposureBreaches keys by `kind=single_name|sector|
	// top3|cash_floor`. Cardinality bounded by 4.
	//
	// decisionCorrelationHighPairs counts the cumulative number
	// of high-correlation pairs surfaced. No labels — a single
	// counter is enough to spot regime changes (sudden
	// correlation spikes flag a likely systemic move).
	//
	// decisionCooldownVetos keys by `symbol=...`. Bounded by the
	// per-fund universe size (~tens per fund).
	//
	// decisionRiskBudgetThrottled keys by `reason=drawdown|
	// vol_target_zero|disabled`. Cardinality bounded by 3.
	decisionInputBlocks          map[string]int64
	decisionInputCalls           int64
	decisionExposureBreaches     map[string]int64
	decisionCorrelationHighPairs int64
	decisionCooldownVetos        map[string]int64
	decisionRiskBudgetThrottled  map[string]int64
}

var httpRequestDurationSecondsBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{
		httpRequestsTotal:             make(map[string]int64),
		httpRequestDurationMS:         make(map[string]int64),
		httpRequestCount:              make(map[string]int64),
		httpRequestDurationBuckets:    make(map[string]int64),
		httpRequestDurationSumSeconds: make(map[string]float64),
		panicTotal:                    make(map[string]int64),
		llmCallsTotal:                 make(map[string]int64),
		llmLatencyMS:                  make(map[string]int64),
		llmCallCount:                  make(map[string]int64),
		workflowTransitions:           make(map[string]int64),
		marketplaceReconciliations:    make(map[string]int64),
		marketplaceReconciliationLast: make(map[string]int64),
		hardRiskRejections:            make(map[string]int64),
		lotLedgerFailures:             make(map[string]int64),
		decisionInputBlocks:           make(map[string]int64),
		decisionExposureBreaches:      make(map[string]int64),
		decisionCooldownVetos:         make(map[string]int64),
		decisionRiskBudgetThrottled:   make(map[string]int64),
	}
}

func (m *serverMetrics) ObserveHTTP(method, path string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	key := fmt.Sprintf("method=%s,path=%s,status=%d", method, path, status)
	durationSeconds := duration.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.httpRequestsTotal[key]++
	m.httpRequestDurationMS[key] += duration.Milliseconds()
	m.httpRequestCount[key]++
	m.httpRequestDurationSumSeconds[key] += durationSeconds
	for _, bucket := range httpRequestDurationSecondsBuckets {
		if durationSeconds <= bucket {
			m.httpRequestDurationBuckets[fmt.Sprintf("%s,le=%s", key, prometheusFloat(bucket))]++
		}
	}
	m.httpRequestDurationBuckets[fmt.Sprintf("%s,le=+Inf", key)]++
}

func (m *serverMetrics) RecordPanic(path string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panicTotal[path]++
}

// RecordRefreshPass is the positionRefreshMetrics implementation. It
// records one position-quote refresh pass: the number of rows updated
// (zero on no-op passes), how long the pass took, and whether at least
// one individual row failed. Cheap counters only — we don't carry the
// per-fund cardinality because the user-visible signal is "is the
// refresher healthy at all".
func (m *serverMetrics) RecordRefreshPass(rows int, duration time.Duration, failed bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.positionRefreshPassesTotal++
	if failed {
		m.positionRefreshFailuresTotal++
	}
	if rows > 0 {
		m.positionRefreshRowsTotal += int64(rows)
	}
	m.positionRefreshDurationMS += duration.Milliseconds()
}

// ObserveDecisionInput records the Sprint D #1 PM-decision-input
// observability counters from a single decision_input_fingerprint
// emission. presentBlocks/absentBlocks are the canonical block
// names from decision.Trace.PresentBlocks() / AbsentBlocks().
// exposureBreaches lists which guardrail kinds tripped in the
// snapshot (kept short — typically 0 or 1).
// highCorrPairs is the number of high-correlation pairs surfaced.
// cooldownSymbols and riskBudgetReason are non-empty only when the
// fingerprint shows the respective signal as present and a veto
// fired (cooldown blocked a symbol) or the dynamic budget actually
// throttled (drawdown / vol-target zero).
//
// Cardinality safety: every label set above is bounded by the
// fixed signal vocabulary (18 blocks), the breach kind enum (4),
// the per-fund universe (typically <50), and the throttle reason
// enum (3). No per-fund label is added — fund-level breakdown
// stays in logs (decision_input_fingerprint slog records carry
// fund_id) to keep cardinality predictable.
func (m *serverMetrics) ObserveDecisionInput(presentBlocks, absentBlocks []string, exposureBreaches []string, highCorrPairs int, cooldownSymbols []string, riskBudgetReason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisionInputCalls++
	for _, block := range presentBlocks {
		key := fmt.Sprintf("block=%s,present=true", block)
		m.decisionInputBlocks[key]++
	}
	for _, block := range absentBlocks {
		key := fmt.Sprintf("block=%s,present=false", block)
		m.decisionInputBlocks[key]++
	}
	for _, kind := range exposureBreaches {
		if kind == "" {
			continue
		}
		key := fmt.Sprintf("kind=%s", kind)
		m.decisionExposureBreaches[key]++
	}
	if highCorrPairs > 0 {
		m.decisionCorrelationHighPairs += int64(highCorrPairs)
	}
	for _, symbol := range cooldownSymbols {
		if symbol == "" {
			continue
		}
		key := fmt.Sprintf("symbol=%s", symbol)
		m.decisionCooldownVetos[key]++
	}
	if reason := strings.TrimSpace(riskBudgetReason); reason != "" {
		key := fmt.Sprintf("reason=%s", reason)
		m.decisionRiskBudgetThrottled[key]++
	}
}

func (m *serverMetrics) ObserveLLM(provider, model, step string, status string, latency time.Duration) {
	if m == nil {
		return
	}
	key := fmt.Sprintf("provider=%s,model=%s,step=%s,status=%s", provider, model, step, status)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.llmCallsTotal[key]++
	m.llmLatencyMS[key] += latency.Milliseconds()
	m.llmCallCount[key]++
}

func (m *serverMetrics) ObserveWorkflow(fundID, state, step string) {
	if m == nil {
		return
	}
	key := fmt.Sprintf("fund=%s,state=%s,step=%s", fundID, state, step)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workflowTransitions[key]++
}

func (m *serverMetrics) ObserveMarketplaceReconciliation(status string, inspected, markedFailed, unresolved, errored int) {
	if m == nil {
		return
	}
	key := fmt.Sprintf("status=%s", status)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marketplaceReconciliations[key]++
	m.marketplaceReconciliationLast["kind=inspected"] = int64(inspected)
	m.marketplaceReconciliationLast["kind=marked_failed"] = int64(markedFailed)
	m.marketplaceReconciliationLast["kind=unresolved"] = int64(unresolved)
	m.marketplaceReconciliationLast["kind=errored"] = int64(errored)
}

func (m *serverMetrics) RecordHardRiskRejection(rule, symbol string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(rule) == "" {
		rule = "unknown"
	}
	if strings.TrimSpace(symbol) == "" {
		symbol = "unknown"
	}
	key := fmt.Sprintf("rule=%s,symbol=%s", rule, symbol)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hardRiskRejections[key]++
}

// RecordLotLedgerFailure increments the lot-ledger drift counter
// for a (side, symbol) pair. The Prometheus export surfaces the
// gauge as fundai_lot_ledger_failures_total so the operator can
// alert when a fund's attribution layer is silently going out of
// sync with the trade ledger.
func (m *serverMetrics) RecordLotLedgerFailure(side, symbol string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(side) == "" {
		side = "unknown"
	}
	if strings.TrimSpace(symbol) == "" {
		symbol = "unknown"
	}
	key := fmt.Sprintf("side=%s,symbol=%s", side, symbol)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lotLedgerFailures[key]++
}

func (m *serverMetrics) ExportPrometheus() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var lines []string
	lines = append(lines,
		"# HELP fundai_http_requests_total Total API requests.",
		"# TYPE fundai_http_requests_total counter",
	)
	for _, key := range sortedMetricKeys(m.httpRequestsTotal) {
		lines = append(lines, fmt.Sprintf("fundai_http_requests_total{%s} %d", prometheusLabels(key), m.httpRequestsTotal[key]))
	}
	lines = append(lines,
		"# HELP fundai_http_request_duration_avg_ms Average API request duration in milliseconds.",
		"# TYPE fundai_http_request_duration_avg_ms gauge",
	)
	for _, key := range sortedMetricKeys(m.httpRequestCount) {
		count := m.httpRequestCount[key]
		avg := 0.0
		if count > 0 {
			avg = float64(m.httpRequestDurationMS[key]) / float64(count)
		}
		lines = append(lines, fmt.Sprintf("fundai_http_request_duration_avg_ms{%s} %.2f", prometheusLabels(key), avg))
	}
	lines = append(lines,
		"# HELP fundai_http_request_duration_seconds API request duration histogram in seconds.",
		"# TYPE fundai_http_request_duration_seconds histogram",
	)
	for _, key := range sortedMetricKeys(m.httpRequestDurationBuckets) {
		lines = append(lines, fmt.Sprintf("fundai_http_request_duration_seconds_bucket{%s} %d", prometheusLabels(key), m.httpRequestDurationBuckets[key]))
	}
	for _, key := range sortedMetricKeys(m.httpRequestDurationSumSeconds) {
		lines = append(lines, fmt.Sprintf("fundai_http_request_duration_seconds_sum{%s} %.6f", prometheusLabels(key), m.httpRequestDurationSumSeconds[key]))
	}
	for _, key := range sortedMetricKeys(m.httpRequestCount) {
		lines = append(lines, fmt.Sprintf("fundai_http_request_duration_seconds_count{%s} %d", prometheusLabels(key), m.httpRequestCount[key]))
	}
	lines = append(lines,
		"# HELP fundai_http_panics_total Total recovered panics by path.",
		"# TYPE fundai_http_panics_total counter",
	)
	for _, key := range sortedMetricKeys(m.panicTotal) {
		lines = append(lines, fmt.Sprintf("fundai_http_panics_total{path=%q} %d", key, m.panicTotal[key]))
	}
	lines = append(lines,
		"# HELP fundai_llm_calls_total Total LLM calls.",
		"# TYPE fundai_llm_calls_total counter",
	)
	for _, key := range sortedMetricKeys(m.llmCallsTotal) {
		lines = append(lines, fmt.Sprintf("fundai_llm_calls_total{%s} %d", prometheusLabels(key), m.llmCallsTotal[key]))
	}
	lines = append(lines,
		"# HELP fundai_llm_latency_avg_ms Average LLM call latency in milliseconds.",
		"# TYPE fundai_llm_latency_avg_ms gauge",
	)
	for _, key := range sortedMetricKeys(m.llmCallCount) {
		count := m.llmCallCount[key]
		avg := 0.0
		if count > 0 {
			avg = float64(m.llmLatencyMS[key]) / float64(count)
		}
		lines = append(lines, fmt.Sprintf("fundai_llm_latency_avg_ms{%s} %.2f", prometheusLabels(key), avg))
	}
	lines = append(lines,
		"# HELP fundai_workflow_transitions_total Total workflow state transitions.",
		"# TYPE fundai_workflow_transitions_total counter",
	)
	for _, key := range sortedMetricKeys(m.workflowTransitions) {
		lines = append(lines, fmt.Sprintf("fundai_workflow_transitions_total{%s} %d", prometheusLabels(key), m.workflowTransitions[key]))
	}
	lines = append(lines,
		"# HELP fundai_marketplace_reconciliations_total Total marketplace reconciliation passes.",
		"# TYPE fundai_marketplace_reconciliations_total counter",
	)
	for _, key := range sortedMetricKeys(m.marketplaceReconciliations) {
		lines = append(lines, fmt.Sprintf("fundai_marketplace_reconciliations_total{%s} %d", prometheusLabels(key), m.marketplaceReconciliations[key]))
	}
	lines = append(lines,
		"# HELP fundai_marketplace_reconciliation_last Last marketplace reconciliation summary by kind.",
		"# TYPE fundai_marketplace_reconciliation_last gauge",
	)
	for _, key := range sortedMetricKeys(m.marketplaceReconciliationLast) {
		lines = append(lines, fmt.Sprintf("fundai_marketplace_reconciliation_last{%s} %d", prometheusLabels(key), m.marketplaceReconciliationLast[key]))
	}
	lines = append(lines,
		"# HELP fundai_hard_risk_rejections_total Total hard risk gate rejections by rule and symbol.",
		"# TYPE fundai_hard_risk_rejections_total counter",
	)
	for _, key := range sortedMetricKeys(m.hardRiskRejections) {
		lines = append(lines, fmt.Sprintf("fundai_hard_risk_rejections_total{%s} %d", prometheusLabels(key), m.hardRiskRejections[key]))
	}
	lines = append(lines,
		"# HELP fundai_lot_ledger_failures_total Total lot ledger record failures by trade side and symbol (trade still completed).",
		"# TYPE fundai_lot_ledger_failures_total counter",
	)
	for _, key := range sortedMetricKeys(m.lotLedgerFailures) {
		lines = append(lines, fmt.Sprintf("fundai_lot_ledger_failures_total{%s} %d", prometheusLabels(key), m.lotLedgerFailures[key]))
	}
	lines = append(lines,
		"# HELP fundai_decision_input_calls_total Total PM decision inputs assembled (one per fund-day decision).",
		"# TYPE fundai_decision_input_calls_total counter",
		fmt.Sprintf("fundai_decision_input_calls_total %d", m.decisionInputCalls),
		"# HELP fundai_decision_input_blocks_total Per-block presence count in PM decision inputs. Divide by fundai_decision_input_calls_total to get presence rate.",
		"# TYPE fundai_decision_input_blocks_total counter",
	)
	for _, key := range sortedMetricKeys(m.decisionInputBlocks) {
		lines = append(lines, fmt.Sprintf("fundai_decision_input_blocks_total{%s} %d", prometheusLabels(key), m.decisionInputBlocks[key]))
	}
	lines = append(lines,
		"# HELP fundai_decision_exposure_breaches_total Portfolio concentration guardrail breaches detected during PM decision input assembly.",
		"# TYPE fundai_decision_exposure_breaches_total counter",
	)
	for _, key := range sortedMetricKeys(m.decisionExposureBreaches) {
		lines = append(lines, fmt.Sprintf("fundai_decision_exposure_breaches_total{%s} %d", prometheusLabels(key), m.decisionExposureBreaches[key]))
	}
	lines = append(lines,
		"# HELP fundai_decision_correlation_high_pairs_total Cumulative number of high-correlation candidate-or-held pairs surfaced to the PM prompt.",
		"# TYPE fundai_decision_correlation_high_pairs_total counter",
		fmt.Sprintf("fundai_decision_correlation_high_pairs_total %d", m.decisionCorrelationHighPairs),
		"# HELP fundai_decision_cooldown_vetos_total Per-symbol cooldown vetos surfaced as deterministic blocks in the PM prompt.",
		"# TYPE fundai_decision_cooldown_vetos_total counter",
	)
	for _, key := range sortedMetricKeys(m.decisionCooldownVetos) {
		lines = append(lines, fmt.Sprintf("fundai_decision_cooldown_vetos_total{%s} %d", prometheusLabels(key), m.decisionCooldownVetos[key]))
	}
	lines = append(lines,
		"# HELP fundai_decision_risk_budget_throttled_total Dynamic risk-budget throttles (drawdown / vol target zero / disabled) applied to per-trade R.",
		"# TYPE fundai_decision_risk_budget_throttled_total counter",
	)
	for _, key := range sortedMetricKeys(m.decisionRiskBudgetThrottled) {
		lines = append(lines, fmt.Sprintf("fundai_decision_risk_budget_throttled_total{%s} %d", prometheusLabels(key), m.decisionRiskBudgetThrottled[key]))
	}
	lines = append(lines,
		"# HELP fundai_marketdata_position_refresh_total Total background position-quote refresher passes that have completed.",
		"# TYPE fundai_marketdata_position_refresh_total counter",
		fmt.Sprintf("fundai_marketdata_position_refresh_total %d", m.positionRefreshPassesTotal),
		"# HELP fundai_marketdata_position_refresh_failures_total Position-quote refresher passes where at least one row failed.",
		"# TYPE fundai_marketdata_position_refresh_failures_total counter",
		fmt.Sprintf("fundai_marketdata_position_refresh_failures_total %d", m.positionRefreshFailuresTotal),
		"# HELP fundai_marketdata_position_refresh_rows_total Total holding_positions rows updated by the background refresher.",
		"# TYPE fundai_marketdata_position_refresh_rows_total counter",
		fmt.Sprintf("fundai_marketdata_position_refresh_rows_total %d", m.positionRefreshRowsTotal),
		"# HELP fundai_marketdata_position_refresh_duration_ms_total Cumulative wall-clock time (ms) spent in position-quote refresh passes.",
		"# TYPE fundai_marketdata_position_refresh_duration_ms_total counter",
		fmt.Sprintf("fundai_marketdata_position_refresh_duration_ms_total %d", m.positionRefreshDurationMS),
	)
	return strings.Join(append(lines, ""), "\n")
}

func exportRuntimePrometheus(db *sql.DB, leaseManager *scheduler.LeaseManager) string {
	var lines []string
	if db != nil {
		stats := db.Stats()
		lines = append(lines,
			"# HELP fundai_db_open_connections Current number of established database connections.",
			"# TYPE fundai_db_open_connections gauge",
			fmt.Sprintf("fundai_db_open_connections %d", stats.OpenConnections),
			"# HELP fundai_db_in_use_connections Current number of in-use database connections.",
			"# TYPE fundai_db_in_use_connections gauge",
			fmt.Sprintf("fundai_db_in_use_connections %d", stats.InUse),
			"# HELP fundai_db_idle_connections Current number of idle database connections.",
			"# TYPE fundai_db_idle_connections gauge",
			fmt.Sprintf("fundai_db_idle_connections %d", stats.Idle),
			"# HELP fundai_db_max_open_connections Configured maximum number of open database connections. Zero means unlimited.",
			"# TYPE fundai_db_max_open_connections gauge",
			fmt.Sprintf("fundai_db_max_open_connections %d", stats.MaxOpenConnections),
			"# HELP fundai_db_wait_count_total Total database connection waits due to pool saturation.",
			"# TYPE fundai_db_wait_count_total counter",
			fmt.Sprintf("fundai_db_wait_count_total %d", stats.WaitCount),
			"# HELP fundai_db_wait_duration_seconds_total Total time spent waiting for database connections.",
			"# TYPE fundai_db_wait_duration_seconds_total counter",
			fmt.Sprintf("fundai_db_wait_duration_seconds_total %.6f", stats.WaitDuration.Seconds()),
		)
	}
	if leaseManager != nil {
		lines = append(lines,
			"# HELP fundai_scheduler_leader_state Whether this replica currently owns a scheduler lease (1=leader, 0=follower).",
			"# TYPE fundai_scheduler_leader_state gauge",
		)
		for _, lease := range []string{SchedulerLeaseName, MarketplaceReconcilerLeaseName} {
			value := 0
			if leaseManager.IsLeader(lease) {
				value = 1
			}
			lines = append(lines, fmt.Sprintf("fundai_scheduler_leader_state{lease=%q} %d", lease, value))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(append(lines, ""), "\n")
}

func sortedMetricKeys[K comparable](values map[string]K) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// exportMarketDataPrometheus emits per-provider health counters tracked by
// the marketdata service. Labels:
//
//   - provider: short provider name (tencent, yahoo, eastmoney, ...)
//
// Series are bounded by the number of providers (<= 15 in practice) so
// there's no cardinality risk from making each its own time-series.
func exportMarketDataPrometheus(svc *marketdata.Service) string {
	if svc == nil {
		return ""
	}
	health := svc.ProviderHealth()
	if len(health) == 0 {
		return ""
	}
	providers := make([]string, 0, len(health))
	for name := range health {
		providers = append(providers, name)
	}
	sort.Strings(providers)

	now := time.Now().UTC()
	lines := []string{
		"# HELP fundai_marketdata_provider_calls_total Number of upstream calls per provider.",
		"# TYPE fundai_marketdata_provider_calls_total counter",
	}
	for _, p := range providers {
		lines = append(lines, fmt.Sprintf("fundai_marketdata_provider_calls_total{provider=%q} %d", p, health[p].TotalCalls))
	}
	lines = append(lines,
		"# HELP fundai_marketdata_provider_failures_total Number of failed upstream calls per provider.",
		"# TYPE fundai_marketdata_provider_failures_total counter",
	)
	for _, p := range providers {
		lines = append(lines, fmt.Sprintf("fundai_marketdata_provider_failures_total{provider=%q} %d", p, health[p].TotalFailures))
	}
	lines = append(lines,
		"# HELP fundai_marketdata_provider_consecutive_failures Current run of consecutive failures per provider (resets to 0 on success).",
		"# TYPE fundai_marketdata_provider_consecutive_failures gauge",
	)
	for _, p := range providers {
		lines = append(lines, fmt.Sprintf("fundai_marketdata_provider_consecutive_failures{provider=%q} %d", p, health[p].ConsecutiveFailures))
	}
	lines = append(lines,
		"# HELP fundai_marketdata_provider_circuit_open Whether the provider's circuit breaker is currently open (1) or closed (0).",
		"# TYPE fundai_marketdata_provider_circuit_open gauge",
	)
	for _, p := range providers {
		open := 0
		stats := health[p]
		if !stats.CircuitOpenUntil.IsZero() && now.Before(stats.CircuitOpenUntil) {
			open = 1
		}
		lines = append(lines, fmt.Sprintf("fundai_marketdata_provider_circuit_open{provider=%q} %d", p, open))
	}
	lines = append(lines,
		"# HELP fundai_marketdata_provider_latency_ms_ema Exponential moving average of upstream call latency (ms).",
		"# TYPE fundai_marketdata_provider_latency_ms_ema gauge",
	)
	for _, p := range providers {
		lines = append(lines, fmt.Sprintf("fundai_marketdata_provider_latency_ms_ema{provider=%q} %d", p, health[p].EMALatencyMs))
	}
	return strings.Join(append(lines, ""), "\n")
}

func prometheusFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func prometheusLabels(serialized string) string {
	if strings.TrimSpace(serialized) == "" {
		return ""
	}
	parts := strings.Split(serialized, ",")
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		labels = append(labels, fmt.Sprintf("%s=%q", strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])))
	}
	return strings.Join(labels, ",")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"failed to encode response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstEnv(keysAndFallback ...string) string {
	if len(keysAndFallback) == 0 {
		return ""
	}
	fallback := keysAndFallback[len(keysAndFallback)-1]
	for _, key := range keysAndFallback[:len(keysAndFallback)-1] {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return fallback
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	origins := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}

// readQuoteProvidersByMarket assembles the per-market provider chain overrides
// from environment variables of the form MARKETDATA_QUOTE_PROVIDERS_{MARKET}
// where {MARKET} is one of the canonical market keys produced by
// marketdata.normalizeMarket (cnstock, usstock, hkstock, futures, crypto).
//
// Example:
//
//	MARKETDATA_QUOTE_PROVIDERS_CNSTOCK=tencent,akshare
//	MARKETDATA_QUOTE_PROVIDERS_FUTURES=akshare,yahoo
//	MARKETDATA_QUOTE_PROVIDERS_CRYPTO=coingecko,yahoo
//
// Any unset env var leaves that market to fall back to the global
// MARKETDATA_QUOTE_PROVIDERS list (or the built-in default chain when that's
// empty too). Returning nil means "no per-market override at all" so the
// market resolver can skip the lookup entirely.
func readQuoteProvidersByMarket() map[string][]string {
	markets := []string{"cnstock", "usstock", "hkstock", "futures", "crypto"}
	overrides := make(map[string][]string, len(markets))
	for _, market := range markets {
		key := "MARKETDATA_QUOTE_PROVIDERS_" + strings.ToUpper(market)
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		names := splitCSV(raw)
		if len(names) == 0 {
			continue
		}
		overrides[market] = names
	}
	if len(overrides) == 0 {
		return nil
	}
	return overrides
}

func legacyDatabaseURLFallback() string {
	host := envOr("DB_HOST", "localhost")
	port := envOr("DB_PORT", "5432")
	user := envOr("DB_USER", "fundai")
	password := envOr("DB_PASSWORD", "fundai")
	name := envOr("DB_NAME", "fundai")
	sslMode := envOr("DB_SSL_MODE", "disable")
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s", user, password, host, port, name, sslMode)
}

func isProductionEnv(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "prod", "production":
		return true
	default:
		return false
	}
}

func isInsecureJWTSecret(secret string) bool {
	normalized := strings.ToLower(strings.TrimSpace(secret))
	if normalized == "" {
		return true
	}

	insecure := map[string]struct{}{
		"dev-secret-do-not-use-in-prod":                               {},
		"change_me_in_production_please":                              {},
		"change_me_to_a_random_64_char_string":                        {},
		"change_me_to_a_random_64_char_string_min_32_chars_model_cfg": {},
		"changeme":  {},
		"change-me": {},
		"change_me": {},
		"secret":    {},
		"default":   {},
	}
	if _, ok := insecure[normalized]; ok {
		return true
	}

	return len(strings.TrimSpace(secret)) < 32
}

func isLocalDatabaseURL(databaseURL string) bool {
	normalized := strings.ToLower(strings.TrimSpace(databaseURL))
	return strings.Contains(normalized, "localhost") || strings.Contains(normalized, "127.0.0.1")
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// parseDurationEnv reads a Go-duration env var (e.g. "6h", "300ms").
// Empty / unparseable / non-positive → fallback. Returns the
// fallback unchanged when the caller wants the downstream
// package's default; this keeps the env knob optional.
func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// envBoolWithDefault is the variant used when the absence of the env var
// should be treated as a specific boolean default (envBool always returns
// false on absence, which is wrong for opt-out toggles).
func envBoolWithDefault(key string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch raw {
	case "":
		return fallback
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func databaseURLHost(databaseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(databaseURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname()))
}

func isInternalComposeDatabaseURL(databaseURL string) bool {
	return databaseURLHost(databaseURL) == "postgres"
}

func isTLSDisabledDatabaseURL(databaseURL string) bool {
	return strings.Contains(strings.ToLower(databaseURL), "sslmode=disable")
}

func allowInternalComposePlaintextDB(databaseURL string) bool {
	return os.Getenv("RUNNING_IN_CONTAINER") == "1" && envBool("ALLOW_INTERNAL_COMPOSE_DB") && isInternalComposeDatabaseURL(databaseURL)
}

func isPlaceholderProductionDatabaseURL(databaseURL string) bool {
	normalized := strings.ToLower(strings.TrimSpace(databaseURL))
	fragments := []string{
		"change_me",
		"fundai_secret",
		"://fundai:fundai@",
		"password=fundai",
	}
	for _, fragment := range fragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func validateProductionDatabaseURL(databaseURL string) error {
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		return errors.New("DATABASE_URL must be explicitly set when APP_ENV=production; legacy DB_* fallback is not allowed")
	}
	if strings.TrimSpace(databaseURL) == "" {
		return errors.New("DATABASE_URL must be set when APP_ENV=production")
	}
	if isLocalDatabaseURL(databaseURL) {
		return errors.New("DATABASE_URL must not point to localhost in production")
	}
	if isTLSDisabledDatabaseURL(databaseURL) && !allowInternalComposePlaintextDB(databaseURL) {
		return errors.New("DATABASE_URL must require TLS in production; sslmode=disable is allowed only for explicit single-host internal Compose deployments with ALLOW_INTERNAL_COMPOSE_DB=1")
	}
	if isPlaceholderProductionDatabaseURL(databaseURL) {
		return errors.New("DATABASE_URL must not use placeholder or demo credentials in production")
	}
	return nil
}

func validateConfig(cfg *Config) error {
	if isProductionEnv(cfg.Env) {
		if isInsecureJWTSecret(cfg.JWTSecret) {
			return errors.New("JWT_SECRET must be set to a strong non-default value when APP_ENV=production")
		}
		// F29: every key in the rotation ring must independently pass
		// the strength check. A weak old key is still a valid
		// signing oracle until it's removed from the ring.
		if cfg.JWTKeyring != nil {
			for _, k := range cfg.JWTKeyring.All() {
				if isInsecureJWTSecret(k.Secret) {
					return fmt.Errorf("JWT_SECRETS_JSON key %q must be strong non-default in production", k.Kid)
				}
			}
		}
		if isInsecureJWTSecret(cfg.ModelConfigAPIKeySecret) {
			return errors.New("MODEL_CONFIG_API_KEY_SECRET must be set to a strong non-default value when APP_ENV=production")
		}
		if strings.TrimSpace(cfg.JWTSecret) == strings.TrimSpace(cfg.ModelConfigAPIKeySecret) {
			return errors.New("MODEL_CONFIG_API_KEY_SECRET must differ from JWT_SECRET when APP_ENV=production")
		}
		if err := validateProductionDatabaseURL(cfg.DatabaseURL); err != nil {
			return err
		}
		if len(cfg.CORSOrigins) == 0 {
			return errors.New("CORS_ORIGINS must be set in production")
		}
		for _, origin := range cfg.CORSOrigins {
			if origin == "*" {
				return errors.New("CORS_ORIGINS must not contain '*' in production")
			}
			if strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1") {
				return fmt.Errorf("CORS_ORIGINS must not include local origin %q in production", origin)
			}
		}
	}

	if os.Getenv("RUNNING_IN_CONTAINER") == "1" && isLocalDatabaseURL(cfg.DatabaseURL) {
		return errors.New("DATABASE_URL must not point to localhost when RUNNING_IN_CONTAINER=1; use the compose hostname 'postgres' instead")
	}

	return nil
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	cfg := LoadConfig()

	// Set up structured logging.
	var logLevel slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn", "warning":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))

	if err := validateConfig(cfg); err != nil {
		slog.Error("invalid runtime configuration", "error", err, "env", cfg.Env)
		os.Exit(1)
	}

	slog.Info("starting FundAI Simulator",
		"version", version,
		"build_time", buildTime,
		"env", cfg.Env,
		"port", cfg.Port,
		"cors_origins", cfg.CORSOrigins,
	)

	// Connect to database with retries.
	db, err := connectDB(cfg)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// Run migrations.
	if err := runMigrations(db, cfg.MigrationsPath); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}

	// Initialize services.
	svc, err := initServices(db, cfg)
	if err != nil {
		slog.Error("failed to initialize services", "error", err)
		os.Exit(1)
	}
	defer svc.Stop()

	// Build HTTP router.
	handler := buildRouter(svc, cfg)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}

	// Start server in background.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Wait for shutdown signal.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig.String())
	case err := <-errCh:
		slog.Error("server error", "error", err)
	}

	// Graceful shutdown with 15-second deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	slog.Info("shutting down HTTP server")
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("forced shutdown", "error", err)
	}

	slog.Info("server stopped")
}
