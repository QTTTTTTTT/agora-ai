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

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/alphalesson"
	"github.com/fundai/server/internal/analystreport"
	"github.com/fundai/server/internal/debaterepo"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/brinson"
	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/advisorbilling"
	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/dbinstr"
	"github.com/fundai/server/internal/factorexposure"
	"github.com/fundai/server/internal/cnmarketstructure"
	"github.com/fundai/server/internal/fundamental"
	"github.com/fundai/server/internal/stress"
	"github.com/fundai/server/internal/fx"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/outbox"
	"github.com/fundai/server/internal/lotbackfill"
	"github.com/fundai/server/internal/mailer"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/marketimpact"
	"github.com/fundai/server/internal/marketplace"
	"github.com/fundai/server/internal/modelab"
	"github.com/fundai/server/internal/promotion"
	"github.com/fundai/server/internal/embedquota"
	"github.com/fundai/server/internal/embedquotaobs"
	"github.com/fundai/server/internal/memreembed"
	"github.com/fundai/server/internal/recall"
	"github.com/fundai/server/internal/drawdown"
	"github.com/fundai/server/internal/lockup"
	"github.com/fundai/server/internal/marketstatus"
	"github.com/fundai/server/internal/pricecollar"
	"github.com/fundai/server/internal/securitiesborrow"
	"github.com/fundai/server/internal/recon"
	"github.com/fundai/server/internal/surveillance"
	"github.com/fundai/server/internal/compliance"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/scheduler"
	"github.com/fundai/server/internal/quota"
	"github.com/fundai/server/internal/secrets"
	"github.com/fundai/server/internal/stoptrigger"
	"github.com/fundai/server/internal/subscription"
	"github.com/fundai/server/internal/userbyok"
	"github.com/fundai/server/internal/quotecache"
	"github.com/fundai/server/internal/wsfeed"
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
	// W5-3 — embedquota wiring. Zero values fall back to the
	// embedquota.DefaultConfig() baseline (200 calls/min,
	// 1M tokens/day). Operators can ratchet either down via
	// EMBED_QUOTA_RPS / EMBED_QUOTA_DAILY_TOKENS to match the
	// negotiated provider tier.
	MaxCallsPerMinute int
	TokenQuotaPerDay  int
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
			APIKey:            firstEnv("RECALL_OPENAI_API_KEY", "OPENAI_API_KEY", ""),
			BaseURL:           firstEnv("RECALL_OPENAI_BASE_URL", "OPENAI_BASE_URL", ""),
			Model:             firstEnv("RECALL_EMBED_MODEL", "text-embedding-3-small"),
			MaxCallsPerMinute: envInt("EMBED_QUOTA_RPM", 0),
			TokenQuotaPerDay:  envInt("EMBED_QUOTA_DAILY_TOKENS", 0),
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

	// Slow-query instrumentation. Wraps the lib/pq driver so any
	// query/exec that exceeds SLOW_QUERY_THRESHOLD_MS gets logged
	// at WARN level via internal/dbinstr (sanitised query + args
	// count, never the args themselves). Threshold defaults to 0
	// (disabled); typical prod setting is 200-500ms. Logged once
	// at startup so the boot log records whether it's on.
	driverName := "postgres"
	thresholdMs := envInt("SLOW_QUERY_THRESHOLD_MS", 0)
	if thresholdMs > 0 {
		instrumentedName := "postgres-slowq"
		_, regErr := dbinstr.RegisterInstrumented("postgres", instrumentedName, time.Duration(thresholdMs)*time.Millisecond)
		if regErr != nil {
			slog.Warn("failed to register slow-query driver, falling back to vanilla", "error", regErr)
		} else {
			driverName = instrumentedName
			slog.Info("slow query logging enabled", "threshold_ms", thresholdMs)
		}
	}

	maxRetries := 15
	for i := 0; i < maxRetries; i++ {
		db, err = sql.Open(driverName, cfg.DatabaseURL)
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
	BrokerSimulator        *broker.Simulator
	MarketImpactRepo       *marketimpact.Repo
	MarketImpactCache      *marketimpact.Cache
	MarketImpactAdapter    *marketimpact.SlippageAdapter
	LockupRepo             *lockup.Repo
	BorrowRepo             *securitiesborrow.Repo
	BorrowCache            *securitiesborrow.Cache
	// FactorExposureRepo is the S7 / P3-1 instrument factor
	// loading store. nil-safe: admin endpoints short-circuit to
	// 503 when unwired (e.g. tests).
	FactorExposureRepo     *factorexposure.Repo
	// StressRepo backs S7 / P3-3 stress-scenario CRUD and the
	// per-fund stress runner. nil-safe.
	StressRepo             *stress.Repo
	// BrinsonRepo backs S7 / P3-4 Brinson benchmark composition
	// CRUD and the per-fund Brinson runner. nil-safe.
	BrinsonRepo            *brinson.Repo
	// AnalystReportRepo backs the S8.1 four-analyst panel
	// runner (analystreport.Repo). nil-safe.
	AnalystReportRepo      *analystreport.Repo
	// AnalystPanelProvider returns the configured panel for a
	// fund. Wired by the dependency-injection layer per
	// deployment (LLM credentials, persona overrides, …). nil
	// → /api/funds/{fundId}/analysts/run replies 503.
	AnalystPanelProvider   AnalystPanelProvider
	// DebateRepo backs the S8.2 Bull/Bear debate transcript
	// persistence (debaterepo.Repo). nil-safe.
	DebateRepo             *debaterepo.Repo
	// DebateProvider returns the configured Bull/Bear debate
	// orchestrator for a fund. nil → /api/funds/{fundId}/debates/run replies 503.
	DebateProvider         DebateProvider
	// AgentReputationRepo backs the S8.4 per-agent reputation
	// ledger (agentreputation.Repo). nil-safe.
	AgentReputationRepo    *agentreputation.Repo
	// AgentReputationLoop is the background backfill driver
	// that turns analyst panels + debate transcripts into
	// realised-alpha outcomes. nil → admin rebuild endpoint
	// returns 503; the read endpoints continue to serve
	// whatever is in the table.
	AgentReputationLoop    *agentReputationLoop
	// AlphaLessonRepo is the S9.1 alpha-tagged memory writer +
	// reader. The reputation backfill calls it to mint
	// long-term lessons; the PM context builder pulls from it.
	AlphaLessonRepo        *alphalesson.Repo
	// WorkflowCheckpointRepo is the S9.2 per-step snapshot repo
	// behind the daily orchestrator's CheckpointStore plus the
	// admin / resume HTTP endpoints. nil-safe — when unwired the
	// orchestrator silently skips persistence and the admin
	// endpoints return empty lists.
	WorkflowCheckpointRepo *repository.WorkflowCheckpointRepo

	// ModelABRepo + ModelABReporter back the Sprint 10 model A/B
	// admin endpoints (S10.3 reports + S10.4 CRUD). Both are
	// nil-safe — when unwired the admin endpoints return 503.
	// The router and shadow dispatcher use ModelABRepo
	// independently via llmRuntime.AttachModelABResolver.
	ModelABRepo     *modelab.Repo
	ModelABReporter *modelab.Reporter
	// ModelABPromotionDraftRepo + ModelABPromotionScanLoop back the
	// Sprint 13 auto-promotion flow. The repo is read by the admin
	// list / apply / reject endpoints; the loop is the nightly
	// scanner that produces fresh drafts. Both are nil-safe — when
	// the DB isn't wired (e.g. in some integration tests) the
	// promotion routes degrade to 503 / "not configured".
	ModelABPromotionDraftRepo *modelab.DraftRepo
	ModelABPromotionScanLoop  *promotionScanLoop
	// LLMHealthRepo (Sprint 11.4) backs the admin LLM-health
	// dashboard. Nil-safe.
	LLMHealthRepo *repository.LLMHealthRepo
	// AlertEventRepo (Sprint 12.2) backs the alertmanager
	// webhook + admin acknowledgement flow. Nil-safe.
	AlertEventRepo *repository.AlertEventRepo
	// PlatformLLMProviderRepo (S13) backs the admin LLM-provider
	// CRUD + hot-reload flow. The wiring layer calls
	// LoadFromDBOrSeedFromEnv on startup to populate the router;
	// the admin handler calls Upsert/Delete + reloader to update
	// the router in-place without a process restart. Nil-safe.
	PlatformLLMProviderRepo *repository.PlatformLLMProviderRepo
	// ProviderHealthHistoryRepo (S14.A) stores 5-minute health
	// pings for the observability dashboard. Read by admin GET
	// endpoints; written by LLMHealthProbeLoop. Nil-safe.
	ProviderHealthHistoryRepo *repository.ProviderHealthHistoryRepo
	// ProviderDailyRollupRepo (S14.A) stores per-day cost / token
	// rollups. Read by admin GET endpoints; written hourly by
	// LLMCostRollupLoop. Nil-safe.
	ProviderDailyRollupRepo *repository.ProviderDailyRollupRepo
	// LLMHealthProbeLoop (S14.A) — periodic 5-minute probe of every
	// active provider. Stops via context cancellation, not Stop().
	LLMHealthProbeLoop *llmHealthProbeLoop
	// LLMCostRollupLoop (S14.A) — hourly job that re-folds usage
	// entries into per-day rollups. Stops via context cancellation.
	LLMCostRollupLoop *llmCostRollupLoop
	// FundLLMOverrideRepo (S14.B) — per-fund / per-agent provider
	// override table. Read by the LLM router on every call (via
	// the fund-override hook); written by the fund settings admin
	// endpoints. Nil-safe.
	FundLLMOverrideRepo *repository.FundLLMOverrideRepo

	// AdvisorService backs the /advisor consultation surface
	// (migration 098). Distinct from the fund/team subsystem:
	// users hit /api/advisor/* without touching any company /
	// fund / plan / trade. nil-safe: when the DB or persona
	// JSON aren't loaded the routes return 503.
	AdvisorService *advisor.Service
	// advisorFundamentalFetcher is the per-symbol fundamentals
	// source the advisor's master panel uses. Constructed at
	// boot from FUNDAMENTAL_* env knobs (same as the workflow
	// service's fetcher) but kept as a separate handle so
	// advisor traffic doesn't share the workflow's caching
	// lifecycle. nil when fundamentals are disabled.
	advisorFundamentalFetcher fundamental.Fetcher

	// advisorFundamentalHistoryFetcher is the multi-year
	// historical financial series the advisor uses for Buffett-
	// grade criteria like "ROE_10yr_avg >= 15%". nil when
	// FUNDAMENTAL_HISTORY_DISABLED=1 or no provider is wired —
	// the master agents then degrade to data_unavailable on
	// history-dependent checks.
	advisorFundamentalHistoryFetcher fundamental.HistoricalFetcher

	// CNMarketStructureProvider is the A-share intraday + 龙虎榜
	// + market regime + sector strength data source used by the
	// advisor /tactic panel and by an admin probe endpoint.
	// nil-degraded when neither akshare nor an alternative is
	// wired (CN_MARKETSTRUCTURE_DISABLED=1 or no BASE URL set).
	CNMarketStructureProvider cnmarketstructure.Provider
	// CNMarketStructureRegistry is the raw Registry the admin
	// probe handler reads HealthStats from. nil when the chain
	// degrades to a single provider or to nil.
	CNMarketStructureRegistry *cnmarketstructure.Registry

	// AdvisorReputationLoop is the Phase 5 backfill driver that
	// scans recent advisor_consultations, grades them against
	// realised price moves over 1/5/21 day horizons, and writes
	// agent_reputation_outcomes rows with master:* / tactic:*
	// agent_ids. nil-degraded when AdvisorService or
	// AgentReputationRepo isn't wired.
	AdvisorReputationLoop *advisorReputationLoop

	// DailyPicksRepo persists the SHARED-CACHE publisher-mode
	// rows in daily_picks (migration 106). Wired BEFORE
	// AdvisorService so the service constructor can pick it up
	// via WithPicksRepo. nil-degraded when migration 106 isn't
	// applied — advisor.Service.PublishConsult then returns an
	// error and the loop logs + no-ops.
	DailyPicksRepo *dailypicks.Repo
	// DailyPicksLoop is the nightly /daily-picks publisher
	// wave (Go cron). Iterates daily_pick_watchlists × preset,
	// calls advisor.Service.PublishConsult per ticker, UPSERTs
	// the result into daily_picks. nil-degraded when the repo
	// or advisor service isn't wired.
	DailyPicksLoop *dailyPicksLoop

	// AdvisorBillingGate is the Phase A per-user monthly quota
	// guardrail. Every /api/advisor/consult call runs through
	// Gate.Check before the panel runs and Gate.Consume after
	// the panel returns. Plan-derived quotas are pulled from
	// SubscriptionService.GetEffectivePlan; counters live in
	// user_advisor_monthly_usage (migration 100).
	AdvisorBillingGate *advisorbilling.Gate

	// ComplianceRepo persists the SEC disclosure-ack +
	// phrase-violation audit rows (migration 104). nil-safe:
	// degraded boots skip the per-request disclosure gate (the
	// advisor handler fails OPEN — see disclaimerOK in
	// advisor_handler.go).
	ComplianceRepo *repository.ComplianceRepo
	// ComplianceMode is parsed from COMPLIANCE_MODE env once at
	// boot. Drives whether the advisor service redacts LLM
	// outputs (Publisher) or passes them through (RIA-registered)
	// and whether the per-request disclaimer gate is enforced.
	ComplianceMode compliance.Mode

	// UserBYOKRepo backs the Phase B-2 BYOK CRUD handlers and
	// the Phase B-2 user-override hook installed on the LLM
	// router. Reads/writes user_llm_keys (migration 101) with
	// AES-GCM-encrypted plaintext keys.
	UserBYOKRepo *userbyok.Repo

	// AdvisorCreditsRepo backs the Phase C-1 credit-pack ledger
	// (user_advisor_credits + advisor_credit_orders). Plugged
	// into AdvisorBillingGate so Check/Consume cascade plan →
	// credits before returning quota exhausted. The same repo
	// is shared with the LemonSqueezy webhook handler that
	// credits balances on payment.
	AdvisorCreditsRepo *advisorbilling.CreditsRepo

	WSFeedConfig            wsFeedConfig
	WSFeedManager          *wsfeed.Manager
	WSFeedCache            *quotecache.Cache
	WSFeedBridge           *wsFeedSubscriptionBridge
	StopTriggerEngine      *stoptrigger.Engine
	StopTriggerPoller      *stopTriggerPoller
	PromotionAdapter       *promotionServiceAdapter
	PromotionResolver      *promotion.Resolver
	PromotionDecayLoop     *promotionDecayLoop
	LessonScoringLoop      *lessonScoringLoop
	MemoryArchiveLoop      *memoryArchiveLoop
	MemoryEmbedLoop        *memoryEmbedLoop
	MemReembedLoop         *memReembedLoop
	// W5-3 — shared limiter used by every embed call path
	// (memoryEmbedLoop + workflow semantic recall + memreembed
	// re-embed worker). Exposed on Services so an admin route
	// can surface its Snapshot() for observability without
	// reaching into the embed loop's internals.
	EmbedLimiter *embedquota.Limiter
	// W14-1 — per-fund embed observability side-car. Optional;
	// nil disables per-fund attribution and the QuotaEmbedder
	// falls back to W5-3 / aggregate-only behaviour. See
	// docs/PER_FUND_EMBEDQUOTA_OBSERVABILITY.md for the rollout
	// rationale and cardinality budget.
	EmbedQuotaRecorder *embedquotaobs.Recorder
	// W6-1 — re-embed queue. Consolidation callers obtain this
	// to enqueue freshly-rewritten memories; the worker behind
	// the queue (MemReembedLoop) shares the same quota-gated
	// embedder as MemoryEmbedLoop.
	MemReembedQueue      *memreembed.Queue
	CorpActionIngestLoop *corpActionIngestLoop
	Mailer               mailer.Mailer
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
	if s.MemReembedLoop != nil {
		s.MemReembedLoop.Stop()
	}
	if s.EmbedQuotaRecorder != nil {
		s.EmbedQuotaRecorder.Close()
	}
	if s.CorpActionIngestLoop != nil {
		s.CorpActionIngestLoop.Stop()
	}
	if s.StopTriggerPoller != nil {
		s.StopTriggerPoller.Stop()
	}
	// Stop the WS subscription bridge before the manager so
	// the manager doesn't see stale reconcile calls during
	// shutdown.
	if s.WSFeedBridge != nil {
		s.WSFeedBridge.Stop()
	}
	if s.WSFeedManager != nil {
		s.WSFeedManager.Stop()
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
	platformLLMProviderRepo := repository.NewPlatformLLMProviderRepo(db)
	// auditLogger is created lazily inside newAdminHandler; for the
	// S13 initial env-seed (only fires on a brand-new database) we
	// build a temporary one here so the audit row lands in the
	// admin_change_log table from the first boot. Subsequent
	// reloads use llmRuntime.auditLogger (same underlying DB).
	envSeedAudit := audit.NewDBLogger(db)
	llmRuntime, err := newLLMRuntimeWithProviderRepo(
		context.Background(),
		modelConfigService, usageTracker,
		subscriptionService, budgetService, quotaService,
		metrics, cfg.LLMDefaults,
		platformLLMProviderRepo, envSeedAudit,
	)
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

	// Sprint 10 — model-level A/B routing (S10.1) plus shadow
	// dispatcher (S10.2). The router consults a modelab.Resolver
	// before falling through to the per-user / per-agent
	// priority chain; the shadow dispatcher fans out non-primary
	// arms in parallel and persists their outputs into
	// model_ab_shadow_responses. When no experiment matches the
	// (fund, agent, role, step) tuple, both layers are no-ops
	// and the runtime behaves exactly as before.
	modelABRepo := modelab.NewRepo(db)
	modelABResolver := modelab.NewResolver(modelABRepo)
	modelABReporter := modelab.NewReporter(modelABRepo)
	llmRuntime.SetModelABRepo(modelABRepo)
	llmRuntime.AttachModelABResolver(modelABResolver)

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
	leaseManager.Register(CorpActionIngestLeaseName)

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

	// S6.1 — pre-trade market-status gate (halt / suspended /
	// price-limit / stale-quote / calendar). Wired through the
	// broker simulator's optional MarketStatusGate hook. The
	// gate falls open on any internal error so a DB hiccup never
	// turns into a trading halt.
	marketStatusRepo := marketstatus.NewRepo(db)
	marketStatusGate := newMarketStatusGate(marketStatusRepo, metrics, slogLeveledLogger{})

	// S6.2 — size-aware slippage. Calibration rows live in
	// instrument_liquidity, are cached in-memory by
	// marketimpact.Cache (refreshed every 5 min and after every
	// admin write), and are consumed via a matching.SlippageModel
	// adapter plugged into the simulator's matching engine.
	// Missing rows / missing ADV degrade gracefully to
	// asset-class defaults so the simulator never returns
	// silently-zero slippage.
	marketImpactRepo := marketimpact.NewRepo(db)
	marketImpactCache, marketImpactAdapter := newMarketImpactStack(context.Background(), marketImpactRepo, metrics)
	marketImpactEngine := newMatchingEngineWithImpact(marketImpactAdapter)

	// S6.3 — IPO / pre-IPO / restricted-share lock-up gate.
	// Sells of locked qty are rejected with ErrLockupRejected;
	// the gate falls open on internal errors (DB hiccup, missing
	// position row), matching the market-status gate's posture.
	lockupRepo := lockup.NewRepo(db)
	lockupGateImpl := newLockupGate(db, metrics, slogLeveledLogger{})

	// S6.4 — securities-borrow gate + daily accrual loop.
	// The cache is hot-path; admin writes call ApplyChange to
	// install fresh rows immediately. The accrual loop is
	// leader-gated and idempotent on (fund, instrument, day).
	borrowRepo := securitiesborrow.NewRepo(db)
	borrowCache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{
		Repo:            borrowRepo,
		RefreshInterval: 5 * time.Minute,
		OnError: func(err error) {
			slog.Warn("securitiesborrow cache refresh error", "err", err)
		},
	})
	if err := borrowCache.Start(context.Background()); err != nil {
		slog.Warn("securitiesborrow cache initial refresh failed", "err", err)
	}
	borrowGateImpl := newBorrowGate(db, borrowRepo, borrowCache, metrics, slogLeveledLogger{})

	// S6.6 — broker-side price-collar gate. Catches fat-finger /
	// bad-quote / LLM-hallucination limit prices the matcher would
	// otherwise honour (regression: 2026-06-02 301308 fill at
	// 96,226.4188 CNY/share against a true mid of ~500 CNY). The
	// gate runs LAST among the four pre-trade gates so the more
	// dramatic reject reasons (halted / lockup / borrow) keep
	// precedence. Defaults: per-asset-class thresholds (11% A-share
	// main, 21% wide board, 15% US equity, 30% crypto); no-reference
	// → warn (not reject) so a transient marketdata outage doesn't
	// halt trading.
	priceCollarGateImpl := newPriceCollarGate(marketDataService, metrics, slogLeveledLogger{}, pricecollar.EngineOptions{})

	// S12.1 — broker-side lot-size compliance gate (the 5th and
	// final pre-trade gate). Catches A-share board minimums and
	// step rules (100/200, 1/100), HK custom lots (from
	// instrument_metadata, S12.3), US fractional capability
	// (default integer-only), futures integer hands, and crypto
	// step_size (also from instrument_metadata). Sits LAST in the
	// gate chain so regulatory rejects (status / lockup / borrow)
	// and the price-collar fat-finger reject keep precedence in
	// the surfaced reason. Trigger story: 2026-06-03 audit found
	// 301308 buy 1 share (ChiNext min 100) + 688195/688205
	// misaligned partial sells.
	instrumentMetadataRepo := repository.NewInstrumentMetadataRepo(db)
	lotSizeGateImpl := newLotSizeGate(
		db, metrics, slogLeveledLogger{},
		newHKLotResolver(instrumentMetadataRepo),
		newCryptoStepResolver(instrumentMetadataRepo),
		newTickResolver(instrumentMetadataRepo, slogLeveledLogger{}),
		newOverridesResolver(instrumentMetadataRepo),
	)

	// S12-followup (2026-06-04): inject the four broker-side
	// regulatory gates into the workflow service so the PM-
	// direct-fill path (runtimeTradingEngine.executePlanAction)
	// runs the SAME gate impls broker.Simulator runs. Lot-size
	// is intentionally NOT mirrored here — pmPathLotSizeGuard
	// uses a faster in-memory check against the engine's
	// already-loaded position snapshot. Calling WithPMPathGates
	// AFTER all four impls are constructed (and idempotent on
	// subsequent calls) so a future hot-swap of any individual
	// gate stays a one-line edit in this file.
	workflowService = workflowService.WithPMPathGates(
		marketStatusGate,
		lockupGateImpl,
		borrowGateImpl,
		priceCollarGateImpl,
	)

	// S6.5 — WebSocket real-time market data. The cache sits
	// between the manager (which fans out raw ticks) and the
	// broker hot path. When WSFEED_ENABLED is false the cache
	// stays nil and the broker uses the unwrapped REST quote
	// fn — byte-identical to pre-S6.5.
	wsFeedCfg := wsFeedConfigFromEnv(os.Getenv)
	var (
		wsFeedManager *wsfeed.Manager
		wsFeedCache   *quotecache.Cache
		wsFeedBridge  *wsFeedSubscriptionBridge
		brokerQuoteFn = newMarketDataQuoteFn(marketDataService)
	)
	if wsFeedCfg.Enabled {
		wsFeedCache = newQuoteCache(wsFeedCfg)
		wsFeedManager = newWSFeedManager(wsFeedCfg, wsFeedCache, metrics)
		if err := wsFeedManager.Start(context.Background()); err != nil {
			slog.Warn("wsfeed manager start failed", "err", err)
		}
		wsFeedBridge = newWSFeedSubscriptionBridge(db, wsFeedManager, wsFeedCfg, metrics)
		wsFeedBridge.Start(context.Background())
		brokerQuoteFn = newCacheAwareQuoteFn(wsFeedCache, brokerQuoteFn, metrics)
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
		BrokerSimulator: broker.NewSimulator(
			brokerQuoteFn,
			broker.WithMarketStatusGate(marketStatusGate),
			broker.WithMatchEngine(marketImpactEngine),
			broker.WithLockupGate(lockupGateImpl),
			broker.WithBorrowGate(borrowGateImpl),
			broker.WithPriceCollarGate(priceCollarGateImpl),
			broker.WithLotSizeGate(lotSizeGateImpl),
		),
		MarketImpactRepo:    marketImpactRepo,
		MarketImpactCache:   marketImpactCache,
		MarketImpactAdapter: marketImpactAdapter,
		LockupRepo:          lockupRepo,
		BorrowRepo:          borrowRepo,
		BorrowCache:         borrowCache,
		FactorExposureRepo:  factorexposure.NewRepo(db),
		StressRepo:          stress.NewRepo(db),
		BrinsonRepo:         brinson.NewRepo(db),
		AnalystReportRepo:   analystreport.NewRepo(db),
		DebateRepo:          debaterepo.NewRepo(db),
		AgentReputationRepo: agentreputation.NewRepo(db),
		ComplianceRepo:      repository.NewComplianceRepo(db),
		ComplianceMode:      compliance.ParseMode(os.Getenv("COMPLIANCE_MODE")),
		AlphaLessonRepo:     alphalesson.NewRepo(db),
		WSFeedConfig:        wsFeedCfg,
		WSFeedManager:       wsFeedManager,
		WSFeedCache:         wsFeedCache,
		WSFeedBridge:        wsFeedBridge,
		Mailer:                mailerInstance,
		SubscriptionHandler: api.NewSubscriptionHandler(
			newSubscriptionServiceAdapter(subscriptionService),
			newUsageTrackerAdapter(usageTracker),
			newModelConfigServiceAdapter(modelConfigService, llmRuntime),
			llmRuntime,
			newWalletServiceAdapter(db),
		),
	}
	// P0-3: stop-trigger engine. Constructed AFTER the BrokerSimulator
	// is created so we can pass it as the venue. Idempotent on a nil
	// simulator — falls through to nil and downstream callers skip
	// quote-tick fan-out.
	services.StopTriggerEngine = newStopTriggerEngine(services.BrokerSimulator)

	// S8.1 — install the default analyst panel provider. The
	// provider is a closure so we can inject svc-scoped knobs
	// (LLM client, persona overrides, …) without changing the
	// handler API. The default in this sprint runs all four
	// analysts on their deterministic fallback paths; S8.3 will
	// swap nil → real llm.LLMClient once CompleteWithSchema lands.
	services.AnalystPanelProvider = newDefaultAnalystPanelProvider(services)

	// Advisor (/advisor consultation surface, migration 098).
	// Builds its own fundamental.Fetcher rather than borrowing
	// the workflowService's so cache lifecycle stays scoped to
	// the advisor — different TTL strategies can be applied
	// later without breaking fund analytics. nil-safe: when env
	// disables fundamentals the master prompts honestly report
	// data_unavailable instead of fabricating numbers.
	services.advisorFundamentalFetcher = buildFundamentalFetcherFromEnv()
	services.advisorFundamentalHistoryFetcher = buildAdvisorFundamentalHistoryFetcherFromEnv()
	// Daily-picks publisher cache repo (migration 106). Wired
	// BEFORE buildAdvisorService so the service constructor can
	// pick it up via the WithPicksRepo option. nil-safe — when
	// the migration hasn't been applied the repo is still
	// constructable (it'll just return SQL errors at read /
	// write time, which surface as a 5xx the loop logs and
	// retries).
	if db != nil {
		services.DailyPicksRepo = dailypicks.NewRepo(db)
	}
	// CN market structure (akshare 龙虎榜 / 涨停池 / 市场活跃度) —
	// powers the /advisor tactic panel in Phase 4 and the admin
	// probe endpoint registered with the admin handler.
	services.CNMarketStructureRegistry, services.CNMarketStructureProvider = buildCNMarketStructureProvider()
	services.AdvisorService = buildAdvisorService(services)

	// S8.2 — install the default Bull/Bear debate provider.
	// Uses the same nil-LLM path as the panel in S8.1 so the
	// advocates fall back to their deterministic skeletons.
	services.DebateProvider = newDefaultDebateProvider(services)

	// S8.4 — install the agent reputation backfill loop.
	//
	// W16-2 audit: the realised-return source is now wired to the
	// shared OHLC fetcher (same chain the rest of the runtime uses
	// for indicators / regime / ranking) and falls back to the no-op
	// only when the fetcher is unbuildable (OHLC_DISABLED=1 or no
	// providers configured). Pre-fix, every fund silently produced
	// zero outcomes which left the alpha-aware-memory pipeline
	// (AgentTrackRecord prompt block) permanently empty.
	if services.AgentReputationRepo != nil && db != nil {
		repFundRepo := repository.NewFundRepo(db)
		fundLister := func(ctx context.Context) ([]string, error) {
			funds, err := repFundRepo.ListActive(ctx)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(funds))
			for _, f := range funds {
				ids = append(ids, f.ID)
			}
			return ids, nil
		}
		var returnsFn = nullRealisedReturn
		if reputationOHLC := buildOHLCFetcherFromEnv(); reputationOHLC != nil {
			lookup := func(ctx context.Context, fundID string) (string, string, bool) {
				fund, err := repFundRepo.GetByID(ctx, fundID)
				if err != nil || fund == nil {
					return "", "", false
				}
				profile := decodeFundMarketProfile(fund.Config)
				return profile.Market, profile.BenchmarkSymbol, true
			}
			returnsFn = realisedReturnFromOHLC(reputationOHLC, lookup)
		}
		services.AgentReputationLoop = newAgentReputationLoop(
			services.AgentReputationRepo,
			newAnalystPanelSource(services.AnalystReportRepo),
			newDebateTranscriptSource(services.DebateRepo),
			returnsFn,
			agentReputationLoopOptions{
				FundLister:    fundLister,
				LessonWriter:  services.AlphaLessonRepo,
			},
		)
	}

	// Phase 5 — advisor reputation loop. Independent of the
	// per-fund loop above: scans advisor_consultations,
	// grades each (master, tactic) report against realised
	// price moves, writes master:* / tactic:* rows into
	// agent_reputation_outcomes (fund_id IS NULL per
	// migration 099), and refreshes agent_reputation_stats.
	// nil-safe when AdvisorService or AgentReputationRepo are
	// absent.
	if services.AgentReputationRepo != nil && services.AdvisorService != nil {
		var advReturnsFn AdvisorRealisedReturnFn = nullAdvisorRealisedReturn
		if advisorOHLC := buildOHLCFetcherFromEnv(); advisorOHLC != nil {
			advReturnsFn = advisorRealisedReturnFromOHLC(advisorOHLC)
		}
		services.AdvisorReputationLoop = newAdvisorReputationLoop(
			services.AgentReputationRepo,
			services.AdvisorService.Repo(),
			advReturnsFn,
			advisorReputationLoopOptions{},
		)
	}

	// Daily-picks publisher loop. Wakes on a fine-grained
	// CheckInterval, runs each active watchlist's wave after its
	// scheduled instant (e.g. @daily_after_us_close = 16:30 ET).
	// nil-degraded when advisor service or repo aren't wired —
	// the /daily-picks list endpoint still serves whatever
	// historical rows are in the DB.
	if services.AdvisorService != nil && services.DailyPicksRepo != nil {
		services.DailyPicksLoop = newDailyPicksLoop(
			services.AdvisorService,
			services.DailyPicksRepo,
			db,                    // for the wave-cost summary query over usage_entries
			services.UsageTracker, // force-flush so the summary sees rows the wave just wrote
			services.Metrics,      // B2 dailypicks_publish_duration_seconds histogram
			dailyPicksLoopOptions{},
		)
	}

	// Phase A — advisor billing gate.
	// Phase C upgrade — also plug in the credit-pack repo so
	// the gate cascades plan → credits before returning quota
	// exhausted. Both repos share the same DB handle.
	//
	// Mounted on the wired Services so handleConsult can call
	// Gate.Check/Consume around every panel run, and the new
	// GET /api/advisor/billing/summary handler can read the
	// per-user monthly state.
	//
	// nil-safe in two directions:
	//   - db nil → leave gate nil; handleConsult treats nil as
	//     "skip gating" so degraded boots (test main) still work.
	//   - subscription nil → same; the gate dereferences plans
	//     internally so it can't be wired without one.
	if db != nil && subscriptionService != nil {
		services.AdvisorCreditsRepo = advisorbilling.NewCreditsRepo(db)
		services.AdvisorBillingGate = advisorbilling.NewGate(
			db, subscriptionService,
			advisorbilling.WithCreditsRepo(services.AdvisorCreditsRepo),
		)
	}

	// P1-5: order replay. Re-seed the simulator from open trade rows
	// persisted before the last shutdown. Runs synchronously so the
	// HTTP server cannot accept a Cancel/Replace against an order the
	// simulator hasn't seen yet. Best-effort: a per-row projection
	// failure is logged and skipped (see order_replay.go); only a
	// bona-fide DB error returns and is logged below.
	if services.BrokerSimulator != nil && db != nil {
		if _, err := replayOpenOrders(context.Background(), services.BrokerSimulator, repository.NewTradeRepo(db), slog.Default()); err != nil {
			slog.Default().Warn("order replay failed at boot — open orders may not be addressable until next restart",
				"err", err.Error())
		}
	}
	marketplaceAdapter := newMarketplaceServiceAdapter(db, modelConfigService, subscriptionService, llmRuntime)
	auctionAdapter := newMarketplaceAuctionAdapter(marketplaceAdapter)
	// Card D: a single abTestServiceAdapter implements three
	// interfaces — the legacy ABTestService and the two new
	// shadow-agent / operational-attribution surfaces. Sharing
	// the instance keeps the auth + db deps in one place.
	abTestAdapter := newABTestServiceAdapter(db)
	// Card K-1: if AB_SHADOW_LLM_ENABLED=1, route the AB shadow
	// B-variant decisions through the real LLM (per-trade veto +
	// end-of-run learning recap). Falls back to deterministic
	// when the flag is unset OR the runtime client is missing
	// (e.g., system has no LLM keys configured). The AB analyze
	// path is the only consumer of this decider today; wiring
	// here keeps the rest of the adapter unaware of which path
	// is active.
	if envBool("AB_SHADOW_LLM_ENABLED") && llmRuntime != nil && llmRuntime.client != nil {
		// K-5: pass the serverMetrics so the decider can publish
		// `fundai_ab_shadow_llm_calls_total{outcome=...}`. Pass
		// directly — `metrics` is the same struct already wired
		// into the rest of the server. Nil-safe at the recorder
		// boundary so a metrics-free build (e.g. integration
		// harness) still works.
		abTestAdapter = abTestAdapter.WithLLMShadowDecider(llmRuntime.client, metrics)
	}
	services.FundHandler = api.NewFundHandler(
		newFundServiceAdapter(db, workflowService),
		newTeamServiceAdapter(db, usageTracker, modelConfigService, subscriptionService, llmRuntime).WithActivityBus(workflowService.activityBus),
		newPlanServiceAdapter(db, workflowService, llmRuntime),
		newTradeServiceAdapter(db).WithMarketData(marketDataService),
		workflowService,
		newMemoryServiceAdapter(db),
		newDecisionTraceServiceAdapter(db, marketDataService, llmRuntime),
		newMarketServiceAdapter(db, marketDataService, llmRuntime),
		abTestAdapter,
		marketplaceAdapter,
	).WithReflectionService(newReflectionServiceAdapter(db)).
		WithAgentSkillService(newAgentSkillServiceAdapter(db)).
		WithAuctionService(auctionAdapter).
		WithBacktestService(buildBacktestService(db, llmRuntime)).
		WithCorpActionService(newCorpActionServiceAdapter(services)).
		WithBenchmarkService(newBenchmarkServiceAdapter(services)).
		WithHoldingsSeriesService(newHoldingsSeriesServiceAdapter(services)).
		WithABShadowAgentService(abTestAdapter).
		WithABOperationalAttributionService(abTestAdapter).
		WithFundAssistService(newFundAssistAdapter(llmRuntime.client)).
		WithFactorLabService(newFactorLabAdapter()).
		WithPaperTradingService(newPaperTradingAdapter(db)).
		WithCNIntradayService(newCNIntradayAdapter()).
		WithComplianceService(newComplianceAdapter(services.ComplianceRepo, services.ComplianceMode))
	// Phase 3A-5: a SINGLE attribution adapter feeds both the HTTP
	// surface (GET /api/funds/:id/strategy-attribution) and the
	// daily-review hook (runDailyAttribution inside the memory
	// system). Sharing the instance keeps the data consistent and
	// halves the per-process repo footprint.
	attributionAdapter := newAttributionServiceAdapter(db)
	services.FundHandler = services.FundHandler.WithAttributionService(attributionAdapter)
	workflowService = workflowService.WithAttributionService(attributionAdapter.Service())

	// Sprint 9.1 — alpha-aware memory. The per-fund runtime
	// produced by workflowService.newRuntime forwards these
	// repos into the runtimePMAgent so the PM prompt's
	// AgentTrackRecord block is populated. Both are nil-safe;
	// when either is unwired (very old test paths) the block
	// is simply omitted.
	workflowService = workflowService.WithAlphaAwareMemory(services.AgentReputationRepo, services.AlphaLessonRepo)

	// Sprint 9.2 — workflow checkpoints. The orchestrator
	// upserts a per-step row on every runStep call so the
	// resume / admin-UI endpoints can show the timeline and
	// drive resume actions. nil repo (no DB) disables the
	// persistence; the in-process state path is unaffected.
	workflowCheckpointRepo := repository.NewWorkflowCheckpointRepo(db)
	workflowService = workflowService.WithWorkflowCheckpointRepo(workflowCheckpointRepo)
	services.WorkflowCheckpointRepo = workflowCheckpointRepo

	// Sprint 10.3 — expose modelab repo + reporter to the admin
	// handler so the report / CRUD endpoints can read and mutate
	// experiments. Both are nil-safe; when the modelab tables
	// are unmigrated the endpoints surface 5xx from the repo
	// layer, which the admin UI degrades on.
	services.ModelABRepo = modelABRepo
	services.ModelABReporter = modelABReporter
	// Sprint 13 — model A/B auto-promotion scanner. The draft
	// repo and the nightly loop are both wired here so the loop
	// is started by the same goroutine that starts the other
	// background jobs further down.
	services.ModelABPromotionDraftRepo = modelab.NewDraftRepo(db)
	services.ModelABPromotionScanLoop = newPromotionScanLoop(
		modelABReporter,
		modelABRepo,
		services.ModelABPromotionDraftRepo,
		promotionScanLoopOptions{
			Interval: 24 * time.Hour,
		},
	)
	// Sprint 11.4 — admin LLM-health dashboard. Lives next to
	// ModelABRepo because both are admin-only and read-only;
	// nil-safe at the handler boundary.
	services.LLMHealthRepo = repository.NewLLMHealthRepo(db)
	// Sprint 12.2 — alertmanager webhook + admin ack flow.
	services.AlertEventRepo = repository.NewAlertEventRepo(db)
	// S13 — platform LLM provider table (managed via admin UI,
	// hot-reloaded into the router on every Upsert/Delete).
	services.PlatformLLMProviderRepo = platformLLMProviderRepo

	// S14.A — provider observability: 5-minute health probe + hourly
	// cost rollups. Both loops are no-ops when the repos are nil
	// (i.e. when the underlying DB is missing). The loops use
	// context.Background() to outlive request scopes; they'll exit
	// naturally on process termination because that context is
	// only cancelled then. Restart-safety is provided by:
	//   * probe loop: each tick re-lists from DB, no in-memory state
	//   * rollup loop: RecomputeWindow is idempotent on (provider, model, day)
	providerHealthHistoryRepo := repository.NewProviderHealthHistoryRepo(db)
	providerDailyRollupRepo := repository.NewProviderDailyRollupRepo(db)
	services.ProviderHealthHistoryRepo = providerHealthHistoryRepo
	services.ProviderDailyRollupRepo = providerDailyRollupRepo
	healthLoop := newLLMHealthProbeLoop(platformLLMProviderRepo, providerHealthHistoryRepo, slog.Default())
	healthLoop.Start(context.Background())
	services.LLMHealthProbeLoop = healthLoop
	rollupLoop := newLLMCostRollupLoop(providerDailyRollupRepo, slog.Default())
	rollupLoop.Start(context.Background())
	services.LLMCostRollupLoop = rollupLoop

	// S14.B — fund_llm_overrides. Owned by the fund settings page;
	// hot-resolved on every LLM call. The hook lives in llmRuntime
	// so it can dereference (provider, label) into the platform_llm_providers
	// row that holds the encrypted API key. Nil-safe: missing repo
	// means the hook is disabled and the router falls through to
	// the pre-S14 priority chain.
	fundLLMOverrideRepo := repository.NewFundLLMOverrideRepo(db)
	services.FundLLMOverrideRepo = fundLLMOverrideRepo
	llmRuntime.SetFundLLMOverrideRepo(fundLLMOverrideRepo)

	// Phase B-2/3 — user BYOK key repo + router hook.
	//
	// The repo is constructed unconditionally when the DB is
	// available; the hook only routes when a user has an
	// active row, so an empty user_llm_keys table is a no-op.
	// We install the hook even when no users have BYOK keys
	// yet so the wiring exists for the moment the first key
	// lands.
	services.UserBYOKRepo = userbyok.NewRepo(db)
	llmRuntime.SetUserBYOKRepo(services.UserBYOKRepo)

	// Sprint 9.3 — social sentiment ingestion. The registry
	// reads per-platform env flags; when no provider is enabled
	// the call returns nil and the workflow's sentiment block
	// stays news-only, matching pre-9.3 behaviour.
	if socialRegistry := buildSocialRegistryFromEnv(slog.Default()); socialRegistry != nil {
		workflowService = workflowService.WithSocialRegistry(socialRegistry)
	}

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
		// S6.5: WS-feed cache overlay. When a symbol has a
		// fresh WS snapshot we prefer it over the REST quote.
		positionRefresher.SetWSCache(newQuoteCacheLookup(wsFeedCache))
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

		// P1-1: corp-action daily ingest. Walks every active fund's
		// open positions twice a day, drives the provider chain
		// (Eastmoney / Yahoo / ...) to fetch dividends + splits, and
		// applies them via the same pipeline as the corpactionsync
		// CLI. Skipped when DB is absent (smoke tests etc.); leader-
		// gated in multi-replica deployments.
		corpActionIngest := newCorpActionIngestLoop(db)
		corpActionIngest.SetLeaderChecker(leaseManager)
		// Card G: feed the loop's per-tick / per-event observations
		// into the shared serverMetrics. This is what powers the
		// "fundai_corp_action_ingest_*" Prometheus series and the
		// "now - last_success > 7d" alert. nil-safe — the loop
		// uses a noop recorder if metrics is nil.
		corpActionIngest.SetMetrics(metrics)
		corpActionIngest.Start()
		services.CorpActionIngestLoop = corpActionIngest

		// P0-3: stop-trigger poller. Walks every pending stop /
		// stop_limit / trailing_stop on the in-process broker
		// simulator at a fixed cadence, fetches a quote per
		// unique instrument, and forwards into the trigger
		// engine. Trailing stops ratchet, breached stops fire.
		// Skipped silently when the simulator or quote pipeline
		// isn't wired (e.g. unit-test boots without market
		// data).
		if services.StopTriggerEngine != nil && services.BrokerSimulator != nil && marketDataService != nil {
			pollerInterval := envDuration("STOP_TRIGGER_INTERVAL", 5*time.Second)
			stopPoller := newStopTriggerPoller(
				services.StopTriggerEngine,
				services.BrokerSimulator,
				newMarketDataQuoteFn(marketDataService),
				pollerInterval,
				slog.Default(),
			)
			if stopPoller != nil {
				stopPoller.Start()
				services.StopTriggerPoller = stopPoller
			}
		}

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
			// W5-3 — wrap the bare OpenAI client with the
			// embedquota limiter so every embed call goes through
			// rate + quota gating. The two callsites
			// (memoryEmbedLoop + WithSemanticRecall) receive the
			// decorated handle so they share one daily ledger.
			quotaCfg := embedquota.DefaultConfig()
			if cfg.RecallEmbed.MaxCallsPerMinute > 0 {
				quotaCfg.MaxCallsPerMinute = cfg.RecallEmbed.MaxCallsPerMinute
			}
			if cfg.RecallEmbed.TokenQuotaPerDay > 0 {
				quotaCfg.TokenQuotaPerDay = cfg.RecallEmbed.TokenQuotaPerDay
			}
			limiter := embedquota.New(quotaCfg)
			services.EmbedLimiter = limiter
			// W14-1 — opt-in per-fund observability side-car.
			// Off by default: production roll-out is gated on the
			// EMBED_QUOTA_OBS_ENABLED flag so we can land the wire
			// in main but only flip it on for one cluster at a
			// time. ADR: docs/PER_FUND_EMBEDQUOTA_OBSERVABILITY.md.
			var recorder *embedquotaobs.Recorder
			if strings.EqualFold(strings.TrimSpace(os.Getenv("EMBED_QUOTA_OBS_ENABLED")), "true") {
				recorder = embedquotaobs.New(embedquotaobs.Config{})
				services.EmbedQuotaRecorder = recorder
				slog.Info("embed quota observability enabled",
					"max_funds", embedquotaobs.Config{}.Normalised().MaxFunds,
					"retain_for", embedquotaobs.Config{}.Normalised().RetainFor.String(),
				)
			}
			gated := recall.NewQuotaEmbedderWithRecorder(embedder, limiter, recorder)
			memoryEmbed := newMemoryEmbedLoop(db, gated)
			memoryEmbed.SetLeaderChecker(leaseManager)
			memoryEmbed.Start()
			services.MemoryEmbedLoop = memoryEmbed
			recallSvc := recall.New(db)
			workflowService.WithSemanticRecall(recallSvc, gated)

			// W6-1 — bring up the re-embed queue + worker. The
			// queue is exposed on services.MemReembedQueue so
			// future consolidation callers can Enqueue without
			// reaching into the loop's internals; the worker
			// shares the same quota-gated embedder so the daily
			// token ledger is one source of truth across all
			// embed paths.
			reembedQueue := memreembed.NewQueue(memreembed.DefaultConfig())
			reembedWriter := newMemReembedWriter(db, embedder.Model())
			reembedLoop := newMemReembedLoop(reembedQueue, gated, reembedWriter)
			reembedLoop.SetLeaderChecker(leaseManager)
			reembedLoop.Start()
			services.MemReembedQueue = reembedQueue
			services.MemReembedLoop = reembedLoop

			slog.Info("memory embed loop enabled",
				"model", embedder.Model(),
				"quota_rpm", quotaCfg.MaxCallsPerMinute,
				"quota_daily_tokens", quotaCfg.TokenQuotaPerDay,
				"reembed_queue", "enabled",
			)
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

// HTTP router assembly (`buildRouter` + `pathAliasMiddleware`) lives in
// router.go — see that file for the route table. This split keeps main.go
// focused on the boot lifecycle: config → DB → services → serve → graceful
// shutdown.

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
		_, _ = w.Write([]byte(exportEmbedQuotaPrometheus(svc.EmbedLimiter)))
		_, _ = w.Write([]byte(exportEmbedQuotaPerFundPrometheus(svc.EmbedQuotaRecorder)))
		_, _ = w.Write([]byte(exportSSEMuxPrometheus()))
		_, _ = w.Write([]byte(exportMemReembedPrometheus(svc.MemReembedQueue)))
		// LLM resolver chain — llm_resolution_source_total{layer}.
		// Lazily-registered counter from internal/llm; emits a
		// row per layer that has fired since boot. See
		// internal/llm/resolve_trace.go for the layer enum.
		_, _ = w.Write([]byte(llm.ExportResolverPrometheus()))
	}
}

// exportMemReembedPrometheus renders W7-1 gauges/counters for the
// memory re-embed queue (W3-18 + W6-1). Emits a sentinel
// `status="disabled"` series when the queue is nil so the
// dashboard panel can label "re-embed disabled" without inspecting
// a feature flag. Mirrors the shape of exportEmbedQuotaPrometheus
// so an operator alerting on one already knows the convention.
//
//   pending           — gauge: requests waiting for the worker.
//                       Trends up if the worker is starved (provider
//                       rate-limit, leader gone) or if consolidation
//                       is producing faster than the worker can
//                       consume.
//   embedded_total    — counter: successfully re-embedded rows.
//   retried_total     — counter: per-attempt retries (retries
//                       per request can exceed 1).
//   dead_letter_total — counter: requests that hit MaxRetries and
//                       were dropped. Spike here means the provider
//                       is sustained-failing.
//   status            — gauge: 1=enabled (queue wired), 0=disabled.
func exportMemReembedPrometheus(queue *memreembed.Queue) string {
	var b strings.Builder
	if queue == nil {
		b.WriteString("# HELP fundai_memreembed_status Memory re-embed queue status (1=enabled, 0=disabled).\n")
		b.WriteString("# TYPE fundai_memreembed_status gauge\n")
		b.WriteString("fundai_memreembed_status 0\n")
		return b.String()
	}
	stats := queue.Stats()
	b.WriteString("# HELP fundai_memreembed_pending Re-embed requests waiting to be processed.\n")
	b.WriteString("# TYPE fundai_memreembed_pending gauge\n")
	fmt.Fprintf(&b, "fundai_memreembed_pending %d\n", stats.Pending)
	b.WriteString("# HELP fundai_memreembed_embedded_total Total memories successfully re-embedded since process start.\n")
	b.WriteString("# TYPE fundai_memreembed_embedded_total counter\n")
	fmt.Fprintf(&b, "fundai_memreembed_embedded_total %d\n", stats.Embedded)
	b.WriteString("# HELP fundai_memreembed_retried_total Total individual re-embed retry attempts since process start.\n")
	b.WriteString("# TYPE fundai_memreembed_retried_total counter\n")
	fmt.Fprintf(&b, "fundai_memreembed_retried_total %d\n", stats.Retried)
	b.WriteString("# HELP fundai_memreembed_dead_letter_total Total re-embed requests that exhausted retries and were dropped.\n")
	b.WriteString("# TYPE fundai_memreembed_dead_letter_total counter\n")
	fmt.Fprintf(&b, "fundai_memreembed_dead_letter_total %d\n", stats.DeadLetter)
	b.WriteString("# HELP fundai_memreembed_status Memory re-embed queue status (1=enabled, 0=disabled).\n")
	b.WriteString("# TYPE fundai_memreembed_status gauge\n")
	b.WriteString("fundai_memreembed_status 1\n")
	if !stats.LastErrTime.IsZero() {
		b.WriteString("# HELP fundai_memreembed_last_error_unix Unix-seconds timestamp of the most recent re-embed error (0 if never).\n")
		b.WriteString("# TYPE fundai_memreembed_last_error_unix gauge\n")
		fmt.Fprintf(&b, "fundai_memreembed_last_error_unix %d\n", stats.LastErrTime.Unix())
	}
	return b.String()
}

// exportEmbedQuotaPrometheus renders W6-2 gauges for the
// embedquota.Limiter. Emits zero-status (`status="unavailable"`)
// when the limiter is nil so the dashboard panel can label
// "embed quota disabled" without inspecting a feature flag.
func exportEmbedQuotaPrometheus(limiter *embedquota.Limiter) string {
	h := limiter.HealthSnapshot()
	var b strings.Builder
	b.WriteString("# HELP fundai_embed_quota_tokens_today_used Tokens consumed by the embed worker today (UTC).\n")
	b.WriteString("# TYPE fundai_embed_quota_tokens_today_used gauge\n")
	fmt.Fprintf(&b, "fundai_embed_quota_tokens_today_used %d\n", h.TokensTodayUsed)
	b.WriteString("# HELP fundai_embed_quota_tokens_daily_max Configured maximum tokens per UTC day.\n")
	b.WriteString("# TYPE fundai_embed_quota_tokens_daily_max gauge\n")
	fmt.Fprintf(&b, "fundai_embed_quota_tokens_daily_max %d\n", h.TokensDailyMax)
	b.WriteString("# HELP fundai_embed_quota_calls_last_minute Embed calls in the last 60 seconds.\n")
	b.WriteString("# TYPE fundai_embed_quota_calls_last_minute gauge\n")
	fmt.Fprintf(&b, "fundai_embed_quota_calls_last_minute %d\n", h.CallsLastMinute)
	b.WriteString("# HELP fundai_embed_quota_calls_per_minute_max Configured maximum embed calls per minute.\n")
	b.WriteString("# TYPE fundai_embed_quota_calls_per_minute_max gauge\n")
	fmt.Fprintf(&b, "fundai_embed_quota_calls_per_minute_max %d\n", h.CallsPerMinuteMax)
	b.WriteString("# HELP fundai_embed_quota_status Limiter status (1=ok, 2=throttled, 3=near_limit, 4=exhausted, 0=unavailable).\n")
	b.WriteString("# TYPE fundai_embed_quota_status gauge\n")
	fmt.Fprintf(&b, "fundai_embed_quota_status %d\n", encodeEmbedQuotaStatus(h.Status))
	// W8-1 — backpressure event counters. Distinct from `status`
	// (point-in-time) so an alert can fire on rate() over a
	// rolling window. Throttled = rate-limit hit, request was
	// asked to wait. Exhausted = daily token cap hit, request
	// was rejected outright.
	b.WriteString("# HELP fundai_embed_quota_throttled_total Acquire calls that hit the per-minute rate limit since process start.\n")
	b.WriteString("# TYPE fundai_embed_quota_throttled_total counter\n")
	fmt.Fprintf(&b, "fundai_embed_quota_throttled_total %d\n", h.ThrottledTotal)
	b.WriteString("# HELP fundai_embed_quota_exhausted_total Acquire calls rejected because the daily token quota was exhausted.\n")
	b.WriteString("# TYPE fundai_embed_quota_exhausted_total counter\n")
	fmt.Fprintf(&b, "fundai_embed_quota_exhausted_total %d\n", h.ExhaustedTotal)
	// W9-1 — Acquire wait-time histogram. Counters tell us
	// "how often we throttled"; this tells us "how bad it got",
	// which is what an SLO/alert needs (e.g. p99 < 1s).
	hist := limiter.WaitHistogram()
	b.WriteString("# HELP fundai_embed_quota_acquire_wait_seconds Distribution of recommended wait durations returned by Acquire.\n")
	b.WriteString("# TYPE fundai_embed_quota_acquire_wait_seconds histogram\n")
	for _, bucket := range hist.Buckets {
		fmt.Fprintf(&b, "fundai_embed_quota_acquire_wait_seconds_bucket{le=\"%s\"} %d\n",
			formatPromBucketLe(bucket.LeSeconds), bucket.Count)
	}
	fmt.Fprintf(&b, "fundai_embed_quota_acquire_wait_seconds_bucket{le=\"+Inf\"} %d\n", hist.Count)
	fmt.Fprintf(&b, "fundai_embed_quota_acquire_wait_seconds_sum %s\n", formatPromFloat(hist.SumSeconds))
	fmt.Fprintf(&b, "fundai_embed_quota_acquire_wait_seconds_count %d\n", hist.Count)
	// W10-1 — RecordUsage token-volume histogram, paired with
	// the wait histogram. Together they answer "did throttling
	// come from more calls or fatter calls?" — which the
	// individual Counters can't disambiguate.
	tokens := limiter.TokenHistogram()
	b.WriteString("# HELP fundai_embed_quota_call_tokens Distribution of tokens consumed per RecordUsage observation.\n")
	b.WriteString("# TYPE fundai_embed_quota_call_tokens histogram\n")
	for _, bucket := range tokens.Buckets {
		fmt.Fprintf(&b, "fundai_embed_quota_call_tokens_bucket{le=\"%s\"} %d\n",
			formatPromBucketLe(bucket.Le), bucket.Count)
	}
	fmt.Fprintf(&b, "fundai_embed_quota_call_tokens_bucket{le=\"+Inf\"} %d\n", tokens.Count)
	fmt.Fprintf(&b, "fundai_embed_quota_call_tokens_sum %d\n", tokens.Sum)
	fmt.Fprintf(&b, "fundai_embed_quota_call_tokens_count %d\n", tokens.Count)
	return b.String()
}

// exportEmbedQuotaPerFundPrometheus renders the W14-2 per-fund
// fan-out of the embed quota metrics. Coexists with the
// aggregate exportEmbedQuotaPrometheus — both are emitted on
// /metrics so SLO queries that want process-totals can keep
// using the unlabeled series, while per-fund dashboards can
// pivot on the new fund_id label.
//
// CARDINALITY
// -----------
// Each fund contributes:
//   2 counters    (throttled_total, exhausted_total)
//   1 gauge       (tokens_today_used)
//   waitBuckets+3 histogram series (each bucket + +Inf + sum + count)
//   tokenBuckets+3 histogram series
//
// embedquotaobs.Recorder caps the active set to MaxFunds (default
// 200). Overflow funds are coalesced into the OverflowFundID
// bucket — emitted as a single series so dashboards can alarm on
// "we exceeded the per-fund cardinality budget" without losing
// signal.
//
// DISABLED PATH
// -------------
// recorder=nil emits a single status sentinel so the dashboard
// panel can render "per-fund observability disabled" without
// inspecting EMBED_QUOTA_OBS_ENABLED in two places. This mirrors
// the convention used by exportMemReembedPrometheus.
func exportEmbedQuotaPerFundPrometheus(recorder *embedquotaobs.Recorder) string {
	var b strings.Builder
	if recorder == nil {
		b.WriteString("# HELP fundai_embed_quota_per_fund_status Per-fund embed quota observability status (1=enabled, 0=disabled).\n")
		b.WriteString("# TYPE fundai_embed_quota_per_fund_status gauge\n")
		b.WriteString("fundai_embed_quota_per_fund_status 0\n")
		return b.String()
	}
	b.WriteString("# HELP fundai_embed_quota_per_fund_status Per-fund embed quota observability status (1=enabled, 0=disabled).\n")
	b.WriteString("# TYPE fundai_embed_quota_per_fund_status gauge\n")
	b.WriteString("fundai_embed_quota_per_fund_status 1\n")

	// We emit each metric family once with all funds packed in,
	// so Prometheus can group # HELP / # TYPE correctly. The
	// extra prelude pass costs O(snapshots) but keeps the
	// exposition spec-compliant.
	snaps := recorder.Snapshot()

	b.WriteString("# HELP fundai_embed_quota_per_fund_throttled_total Acquire calls that hit the per-minute rate limit, per fund.\n")
	b.WriteString("# TYPE fundai_embed_quota_per_fund_throttled_total counter\n")
	for _, s := range snaps {
		fmt.Fprintf(&b, "fundai_embed_quota_per_fund_throttled_total{fund_id=%q} %d\n", s.FundID, s.ThrottledTotal)
	}

	b.WriteString("# HELP fundai_embed_quota_per_fund_exhausted_total Acquire calls rejected by the daily token quota, per fund.\n")
	b.WriteString("# TYPE fundai_embed_quota_per_fund_exhausted_total counter\n")
	for _, s := range snaps {
		fmt.Fprintf(&b, "fundai_embed_quota_per_fund_exhausted_total{fund_id=%q} %d\n", s.FundID, s.ExhaustedTotal)
	}

	b.WriteString("# HELP fundai_embed_quota_per_fund_tokens_today_used Tokens consumed today (UTC), per fund.\n")
	b.WriteString("# TYPE fundai_embed_quota_per_fund_tokens_today_used gauge\n")
	for _, s := range snaps {
		fmt.Fprintf(&b, "fundai_embed_quota_per_fund_tokens_today_used{fund_id=%q} %d\n", s.FundID, s.TokensTodayUsed)
	}

	b.WriteString("# HELP fundai_embed_quota_per_fund_acquire_wait_seconds Distribution of Acquire wait durations, per fund.\n")
	b.WriteString("# TYPE fundai_embed_quota_per_fund_acquire_wait_seconds histogram\n")
	for _, s := range snaps {
		writePerFundHistogram(&b, "fundai_embed_quota_per_fund_acquire_wait_seconds", s.FundID, s.WaitBuckets, s.WaitCount, s.WaitSumSeconds, formatPromFloat)
	}

	b.WriteString("# HELP fundai_embed_quota_per_fund_call_tokens Distribution of tokens consumed per RecordUsage observation, per fund.\n")
	b.WriteString("# TYPE fundai_embed_quota_per_fund_call_tokens histogram\n")
	for _, s := range snaps {
		writePerFundHistogram(&b, "fundai_embed_quota_per_fund_call_tokens", s.FundID, s.TokenBuckets, s.TokenCount, float64(s.TokenSum), func(f float64) string {
			// Token sum is integer-valued — emit it as such so
			// recording rules / max() queries don't see a fake
			// floating-point tail.
			return strconv.FormatInt(int64(f), 10)
		})
	}

	return b.String()
}

// writePerFundHistogram emits the canonical histogram exposition
// (buckets + +Inf + sum + count) for one fund. Pulled out to
// keep the exporter readable: two histograms (wait + tokens)
// share identical machinery, only the formatter for `_sum`
// differs.
func writePerFundHistogram(
	b *strings.Builder,
	metric string,
	fundID string,
	buckets []embedquotaobs.BucketCount,
	count uint64,
	sumValue float64,
	formatSum func(float64) string,
) {
	for _, bucket := range buckets {
		fmt.Fprintf(b, "%s_bucket{fund_id=%q,le=\"%s\"} %d\n",
			metric, fundID, formatPromBucketLe(bucket.Le), bucket.Count)
	}
	fmt.Fprintf(b, "%s_bucket{fund_id=%q,le=\"+Inf\"} %d\n", metric, fundID, count)
	fmt.Fprintf(b, "%s_sum{fund_id=%q} %s\n", metric, fundID, formatSum(sumValue))
	fmt.Fprintf(b, "%s_count{fund_id=%q} %d\n", metric, fundID, count)
}

// formatPromBucketLe renders a bucket boundary in the trim form
// Prometheus exposition expects (no exponential notation, no
// trailing zeros). 0.001 stays "0.001"; 1 stays "1"; 600 stays
// "600". strconv.FormatFloat with -1 precision is the canonical
// Go way to do this without the regex hacks Prometheus client
// libs sometimes ship.
func formatPromBucketLe(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// formatPromFloat renders a sum value. We always want enough
// precision to preserve sub-millisecond observations
// (-1 precision again does the right thing).
func formatPromFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func encodeEmbedQuotaStatus(s embedquota.Status) int {
	switch s {
	case embedquota.StatusOK:
		return 1
	case embedquota.StatusThrottled:
		return 2
	case embedquota.StatusNearLimit:
		return 3
	case embedquota.StatusExhausted:
		return 4
	default:
		return 0
	}
}

// exportSSEMuxPrometheus renders W6-2 counters for the SSE
// multiplex endpoint. Lives in cmd/server (not the api package)
// so the import direction stays one-way (cmd → internal).
func exportSSEMuxPrometheus() string {
	m := api.SnapshotMuxStreamMetrics()
	var b strings.Builder
	b.WriteString("# HELP fundai_workflow_sse_mux_active_connections Currently open multiplex SSE connections.\n")
	b.WriteString("# TYPE fundai_workflow_sse_mux_active_connections gauge\n")
	fmt.Fprintf(&b, "fundai_workflow_sse_mux_active_connections %d\n", m.ActiveConnections)
	b.WriteString("# HELP fundai_workflow_sse_mux_connections_total Total multiplex SSE connections opened since process start.\n")
	b.WriteString("# TYPE fundai_workflow_sse_mux_connections_total counter\n")
	fmt.Fprintf(&b, "fundai_workflow_sse_mux_connections_total %d\n", m.ConnectionsTotal)
	b.WriteString("# HELP fundai_workflow_sse_mux_subscriptions_total Total fund subscriptions across all multiplex SSE connections.\n")
	b.WriteString("# TYPE fundai_workflow_sse_mux_subscriptions_total counter\n")
	fmt.Fprintf(&b, "fundai_workflow_sse_mux_subscriptions_total %d\n", m.SubscriptionsTotal)
	b.WriteString("# HELP fundai_workflow_sse_mux_forbidden_frames_total Number of forbidden-fund error frames emitted by the multiplex SSE endpoint.\n")
	b.WriteString("# TYPE fundai_workflow_sse_mux_forbidden_frames_total counter\n")
	fmt.Fprintf(&b, "fundai_workflow_sse_mux_forbidden_frames_total %d\n", m.ForbiddenFramesTotal)
	return b.String()
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
		// P0-6: if the user has 2FA enabled, do NOT mint a session
		// here. Instead hand back a short-lived challenge token the
		// frontend forwards to /api/auth/2fa/challenge alongside
		// the user's TOTP code. We deliberately tolerate a nil
		// totp handler (TOTP_ENCRYPTION_KEY not set) and a nil
		// repo (legacy / dev installs without the table) — both
		// cases fall through to the regular session mint below.
		if svc != nil && svc.DB != nil {
			repo := repository.NewUserTOTPRepo(svc.DB)
			row, lookupErr := repo.GetByUserID(r.Context(), user.ID)
			if lookupErr == nil && row != nil && row.IsEnabled() {
				challenge, expiresAt, mintErr := issueTwoFAChallenge(user.ID, cfg)
				if mintErr != nil {
					slog.Error("failed to issue 2fa challenge", "request_id", requestID, "user_id", user.ID, "error", mintErr)
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to issue 2fa challenge", "request_id": requestID})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"requires_2fa": true,
					"challenge":    challenge,
					"expires_at":   expiresAt.UTC().Format(time.RFC3339),
					"request_id":   requestID,
				})
				return
			}
			// A5: super_admin is the highest-blast-radius role —
			// they can rotate platform secrets, mint refunds,
			// approve any user role change. Password-only is not
			// good enough. If TOTP is missing or only half-enrolled
			// (row exists but IsEnabled()==false), force them
			// through the enrollment flow before issuing a session.
			//
			// We only attempt this when the deployment has TOTP
			// fully wired (TOTP_ENCRYPTION_KEY set). On dev installs
			// without the key the totp handler refuses to register
			// its routes, so forcing enrollment would lock the
			// admin out of the platform — fall through to the
			// regular session in that degenerate case.
			if isSuperAdminUser(user) && totpFeatureWired() && !totpRowFullyEnabled(row) {
				grant, expiresAt, mintErr := issueTwoFAEnrollmentGrant(user.ID, cfg)
				if mintErr != nil {
					slog.Error("failed to issue 2fa enrollment grant", "request_id", requestID, "user_id", user.ID, "error", mintErr)
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to issue 2fa enrollment grant", "request_id": requestID})
					return
				}
				slog.Info("2fa.enroll_grant_issued",
					"request_id", requestID,
					"user_id", user.ID,
					"role", user.Role,
				)
				writeJSON(w, http.StatusOK, map[string]any{
					"requires_2fa_enrollment": true,
					"enrollment_grant":        grant,
					"expires_at":              expiresAt.UTC().Format(time.RFC3339),
					"request_id":              requestID,
				})
				return
			}
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
		// Templatized path collapses opaque IDs (UUID / nanoid /
		// cuid / numeric ids) to {id} placeholders. The histogram
		// + counter metrics use this label to keep cardinality
		// bounded and make P95/P99 statistically meaningful.
		// Raw path is still emitted on the structured log line so
		// debugging individual requests stays possible.
		routeLabel := templatizeAPIPath(r.URL.Path)
		if metrics != nil {
			metrics.ObserveHTTP(r.Method, routeLabel, recorder.status, duration)
		}
		slog.Info("request",
			"request_id", requestID,
			"trace_id", traceID,
			"span_id", spanID,
			"method", r.Method,
			"path", r.URL.Path,
			"route", routeLabel,
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
		// Migration 111 — Paper-trading Publisher's-Exclusion
		// surface. The /track-record family is intentionally
		// anonymous: SEC §202(a)(11)(D) only carves out
		// "publication of impersonal information", so any
		// auth-gated divergence would re-create the personalised-
		// advice risk the exclusion is supposed to avoid. The
		// handler returns identical data for every viewer.
		if strings.HasPrefix(path, "/api/papertrading/public/") {
			return true
		}
		switch path {
		case "/api/health", "/api/version", "/api/metrics", "/api/auth/register", "/api/auth/login", "/api/auth/logout", "/api/auth/session",
			"/api/auth/forgot-password", "/api/auth/reset-password", "/api/auth/wechat-login",
			"/api/auth/2fa/challenge",
			// A5: super_admin forced-2FA enrollment. The user has
			// completed password auth but does NOT yet have a
			// session — only a short-lived enrollment grant. The
			// handlers themselves parse + validate the grant
			// in-body, so session middleware would just turn into
			// a 401 wall they could never get past.
			"/api/auth/2fa/enroll-start", "/api/auth/2fa/enroll-complete",
			// Sprint 12.2 — alertmanager webhook receiver. We let
			// it bypass the JWT middleware because alertmanager
			// authenticates with a shared bearer secret
			// (FUNDAI_ALERT_WEBHOOK_SECRET) rather than a
			// user JWT. The handler itself enforces that secret
			// via subtle.ConstantTimeCompare; the worst-case for
			// adding the route here is the same posture as the
			// /api/metrics endpoint which is also unprotected by
			// design.
			"/api/admin/alerts/webhook",
			// SEC Marketing Rule requires disclosures to be
			// served BEFORE the user has authenticated (so the
			// ComplianceAckModal can render on the welcome /
			// login page and the unauthenticated landing visitor
			// sees the "NOT a registered investment adviser"
			// notice). The endpoint returns text only — no PII —
			// so anonymous access is safe.
			"/api/compliance/disclosure":
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

// slogLeveledLogger adapts log/slog to the leveledLogger interface
// the FX loop expects (P1-4). Centralised here so any future loop
// using leveledLogger reuses the same adapter.
type slogLeveledLogger struct{}

func (slogLeveledLogger) Info(msg string, kv ...any) { slog.Info(msg, kv...) }
func (slogLeveledLogger) Warn(msg string, kv ...any) { slog.Warn(msg, kv...) }

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
	// P1-1 cash ledger write failures. Same model as
	// lotLedgerFailures: trade flow stays unchanged on failure,
	// but a counter surfaces drift so a reconciliation job can
	// re-run the missed entries. Keyed by entry_type so
	// dashboards can answer "is the commission leg silently
	// failing?" without scrolling through trade ids.
	cashLedgerWriteFailures       map[string]int64
	// P1-2 — funding-request lifecycle counters. Keyed by event
	// = approved | rejected | cancelled | created so the ops
	// dashboard can show "how many deposits got approved this
	// week" without joining audit logs. Failures are reported
	// through the regular http_5xx counters; this map only
	// records terminal-state transitions.
	fundingRequestEvents          map[string]int64
	// fxEvents counts FX-related lifecycle hits (P1-4):
	//   upsert_manual / upsert_override — operator wrote a rate
	//   fetch_ok / fetch_error          — scheduler tried Yahoo
	//   convert_stale                   — NAV / cash summary
	//                                     hit a missing rate
	// Exported as fundai_fx_events_total{event="..."}.
	fxEvents                      map[string]int64
	// reconEvents counts reconciliation lifecycle hits (P1-3):
	//   ingest_ok / ingest_duplicate / ingest_error — broker
	//                                                 statement ingest path
	//   run_ok / run_failed                          — daily diff
	//                                                 outcome
	//   break_<break_type>                           — bumped per break
	//                                                 emitted by the engine
	//   resolve_<status>                             — operator resolved
	//                                                 a break
	// Exported as fundai_recon_events_total{event="..."}.
	reconEvents                   map[string]int64
	// surveillanceEvents counts trade-surveillance lifecycle hits
	// (P1-7):
	//   run_ok / run_failed             — scan run outcome
	//   event_<rule_code>               — bumped per persisted event
	//   severity_<severity>             — secondary cardinality view
	//   review_<status>                 — operator review action
	//   insert_error                    — DB write failed for one event
	//                                     (snapshot loaded but persist
	//                                     failed; engine output kept
	//                                     for the run row)
	// Exported as fundai_surveillance_events_total{event="..."}.
	surveillanceEvents            map[string]int64
	// drawdownEvents counts soft-circuit-breaker lifecycle hits
	// (P3-5):
	//   check_ok / check_failed                — scheduler /
	//                                            on-demand evaluation
	//   breach_tier_<tier>                     — engine emitted a
	//                                            breach event for this tier
	//   action_<action>                        — distribution by
	//                                            action (trim_proportional / flatten / defensive_only)
	//   review_<status>                        — operator approve /
	//                                            dismiss / re-open
	//   auto_executed                          — auto_execute path
	//                                            submitted orders
	//   policy_upsert / policy_delete          — admin tier edits
	// Exported as fundai_drawdown_events_total{event="..."}.
	drawdownEvents                map[string]int64
	// marketStatusEvents counts every pre-trade gate decision
	// emitted by the market-status engine (S6.1):
	//   allow                                — order passed all rules
	//   reject_<rule>                        — short-circuit reject by halt / suspended /
	//                                          price_limit / market_closed /
	//                                          half_day_closed
	//   warn_<rule>                          — advisory (e.g. stale_quote)
	//   lookup_failed / calendar_lookup_failed / persist_failed / evaluate_failed —
	//                                          internal hiccups; gate falls open
	//   admin_*                              — operator UI mutations (halt,
	//                                          unhalt, set_limits, calendar)
	// Exported as fundai_marketstatus_events_total{event="..."}.
	marketStatusEvents            map[string]int64
	// marketImpactEvents counts size-aware slippage estimator
	// usage (S6.2):
	//   estimate                              — every FillPrice probe
	//   used_defaults                         — engine fell back to asset-class defaults (no row)
	//   used_adv_fallback                     — calibration row present but ADV missing
	//   bucket_<asset_class>_<bucket>         — adverse-bps bucket histogram
	//   admin_upsert / admin_delete           — operator UI mutations
	//   cache_refresh_ok / cache_refresh_err  — periodic loader outcome
	// Exported as fundai_marketimpact_events_total{event="..."}.
	marketImpactEvents            map[string]int64
	// lockupEvents counts S6.3 IPO lock-up gate hits:
	//   check_allow / check_reject_locked / check_reject_no_position
	//   check_allow_non_sell / check_allow_no_lockup / check_no_repo
	//   gate_lookup_failed / position_lookup_failed
	//   admin_create / admin_update / admin_release / admin_delete
	// Exported as fundai_lockup_events_total{event="..."}.
	lockupEvents                  map[string]int64
	// borrowEvents counts S6.4 securities-borrow gate + daily
	// accrual loop hits:
	//   check_allow_short / check_allow_no_borrow / check_allow_non_sell
	//   check_reject_unavailable / check_reject_insufficient
	//   check_reject_below_min / check_reject_above_max
	//   no_calibration / position_lookup_failed / audit_log_failed
	//   accrual_booked / accrual_skipped_* / book_failed
	//   scan_failed / scan_row_failed / run_completed
	//   admin_upsert_rate / admin_delete_rate
	// Exported as fundai_borrow_events_total{event="..."}.
	borrowEvents                  map[string]int64
	// lotSizeEvents counts broker-side lot-size gate hits (S12.1).
	// Trigger story: 2026-06-03 audit found 301308 buy 1 share +
	// 688195/688205 misaligned partial sells that slipped past the
	// upstream NormalizeBuyQty / NormalizeSellQty because no
	// broker-side terminal gate existed. Events:
	//   allow                       — happy path (qty aligned)
	//   reject_a_share              — A-share buy below MinLot / step
	//   reject_hk_equity            — HK board-lot violation
	//   reject_us_equity            — US fractional without capability
	//   reject_futures              — futures fractional hand
	//   reject_crypto               — crypto step or min-notional miss
	//   reject_unknown_side         — probe Side neither buy nor sell
	//   evaluate_failed             — spec/position source error (fail-open)
	// Exported as fundai_lotsize_events_total{event="..."}.
	lotSizeEvents                 map[string]int64
	// priceCollarEvents counts broker-side fat-finger / bad-quote
	// limit-price gate hits. Trigger story: 2026-06-02 301308 fill
	// at 96,226.4188 CNY/share (true mid ~500). Events:
	//   allow                       — happy path (limit inside collar)
	//   reject_price_collar         — limit too far from reference
	//   warn_price_collar_no_reference / reject_price_collar_no_reference
	//                                 — reference quote missing / stale
	//   evaluate_failed             — engine internal failure (fail-open)
	// Exported as fundai_pricecollar_events_total{event="..."}.
	priceCollarEvents             map[string]int64
	// wsFeedEvents counts S6.5 WebSocket real-time market
	// data plumbing events:
	//   tick_applied / quote_cache_hit / quote_miss_fallback_ok
	//   quote_stale_fallback_ok / quote_stale_served_on_error
	//   quote_miss_fallback_err
	//   state_connected / state_reconnecting / state_disconnected
	//   reconcile_ok / reconcile_added / reconcile_removed
	//   reconcile_query_err / reconcile_subscribe_err / reconcile_unsubscribe_err
	//   manager_error / admin_force_reconnect
	// Exported as fundai_wsfeed_events_total{event="..."}.
	wsFeedEvents                  map[string]int64
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
	// Card G — corp-action ingest observability. The 12h ingest
	// loop has historically been a black box (slog only) and a
	// silent provider regression — Eastmoney WAF block, Yahoo
	// schema drift — looks identical to "no events today" until
	// users complain about a missed split. Surfacing these counters
	// turns that into a Grafana alert.
	//
	//   - corpActionIngestTicks keys by `status=ok|skipped_no_holdings|
	//     skipped_not_leader`. One increment per runOnce call.
	//   - corpActionIngestProviderErrors keys by
	//     `market=a_share|us_equity|hk_equity,outcome=transient|fatal`.
	//     transient = the retry path classified the err as worth
	//     retrying (EOF, reset). fatal = immediate up-stack giveup.
	//   - corpActionIngestRetries keys by the same labels but only
	//     fires when a retry was issued; outcome=succeeded means the
	//     retry produced data, exhausted means it didn't.
	//   - corpActionIngestEvents keys by `action=split|cash_dividend|
	//     combined,phase=upserted|upsert_error`. Phase is the
	//     distinguisher (counter, not gauge) so a dashboard can
	//     compute success rate cheaply.
	//   - corpActionIngestApply keys by
	//     `outcome=applied|missing|error`. missing = position
	//     vanished between collect and apply; error = applier
	//     returned a non-ErrPositionMissing failure.
	//   - corpActionIngestLastTickUnix is the Unix seconds of the
	//     last tick (any leader run, success or skip). A "now -
	//     last > 24h" alert catches the case where the loop
	//     goroutine has stopped firing (e.g., panic-recovered into
	//     a stuck state).
	//   - corpActionIngestLastSuccessUnix is the Unix seconds of
	//     the last tick that produced ≥1 ingested event OR
	//     deliberately skipped because no holdings were active.
	//     "now - last_success > 7d" catches the slower regression
	//     where the provider stops returning anything.
	corpActionIngestTicks            map[string]int64
	corpActionIngestProviderErrors   map[string]int64
	corpActionIngestRetries          map[string]int64
	corpActionIngestEvents           map[string]int64
	corpActionIngestApply            map[string]int64
	corpActionIngestLastTickUnix     int64
	corpActionIngestLastSuccessUnix  int64

	// Card K-5 — AB shadow LLM cost / health.
	//
	// `abShadowLLMCalls` is partitioned by `outcome` so we can
	// see at a glance:
	//   - "decided_by_llm" / "recap_decided_by_llm"      — happy path,
	//     used the model's response.
	//   - "fallback_llm_error" / "recap_fallback_llm_error" — upstream
	//     timeout/refusal/network error; the synthetic decider
	//     rescued the run.
	//   - "fallback_parse_error" / "recap_fallback_parse_error" — the
	//     model spoke but wasn't a valid JSON shape.
	//   - "fallback_budget_cap"                          — per-run
	//     `AB_SHADOW_LLM_MAX_CALLS` was exceeded; we stopped paying
	//     the model and used the synthetic decider for the rest.
	//
	// Two operator-facing numbers fall out of this:
	//   sum(rate(...{outcome="decided_by_llm"}[5m])) → live LLM
	//   spend in "calls per second", which combined with model
	//   pricing tells you the burn.
	//   sum(...{outcome=~"fallback_.*"}) / sum(...) → fallback rate;
	//   if it's > 5% the model or budget needs tuning.
	//
	// We keep this on `serverMetrics` (not on llmBSideDecider) so
	// every run shares one counter — the decider is rebuilt per
	// analyze invocation and would lose state otherwise.
	abShadowLLMCalls map[string]int64

	// Sprint 11.4 — decision-source counter. One counter row per
	// PM decision keyed by source (llm_pm / llm_three_stage /
	// fallback_after_llm_error / fallback_empty_plan /
	// fallback_no_llm / legacy) and, for fallback rows, the
	// errorclass.Category and provider. Powers the
	// `fundai_pm_decision_total{source,category,provider}`
	// series the admin LLM-health board (S11.4) reads and the
	// SRE alert template uses to fire when fallback rate exceeds
	// 5% of all PM decisions over the trailing hour.
	decisionSourceTotal map[string]int64

	// B2 — three business metrics for the optimisation push.
	// Each is intentionally kept on `serverMetrics` (not
	// internal/metrics) so the existing /api/metrics endpoint and
	// hand-rolled exporter pick them up automatically.
	//
	//   dailyPicksPublishBuckets — histogram, labelled by preset.
	//   Bucketed in seconds via dailyPicksPublishBucketsSeconds
	//   below. The interesting tail is "did today's publish
	//   take 4 minutes again?" so the bucket schedule is
	//   weighted toward minute-scale rather than the second-scale
	//   HTTP histogram.
	//
	//   complianceFilterBlocked — counter, labelled by
	//   (pattern, layer). pattern is `compliance.Violation.Pattern`
	//   (e.g. "guaranteed_return", "breakout_imminent"); layer is
	//   "advisor" for the scanner-driven path, "geo" for the
	//   middleware fail-close path, and one bucket per future
	//   surface. Lets us alert on a sudden spike of a single
	//   regex hit ("a master prompt regressed and is now
	//   emitting forbidden phrases on every call") without
	//   trawling audit-log rows.
	//
	//   subscriptionMRR — gauge in USD. Refreshed on a slow
	//   ticker (default 1h) from a SUM(price_usd) query over
	//   active subscriptions. One series only (no labels for v1
	//   because we deliberately don't want to expose per-plan
	//   counts on the public /metrics surface).
	dailyPicksPublishBuckets  map[string]int64
	dailyPicksPublishSumSecs  map[string]float64
	dailyPicksPublishCount    map[string]int64
	complianceFilterBlocked   map[string]int64
	subscriptionMRRUSD        float64
}

// dailyPicksPublishBucketsSeconds is the histogram bucket schedule
// for the per-preset daily-picks publish duration. Anchored at
// 30s (a healthy publish over the cheapest preset), 1m, 2m, 5m
// (the SRE alert threshold), 10m (the "something is on fire"
// threshold), 30m (the "queued behind a stuck preset" tail).
var dailyPicksPublishBucketsSeconds = []float64{30, 60, 120, 300, 600, 1800}

var httpRequestDurationSecondsBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// httpRequestQuantiles is the canonical set of latency quantiles
// computed at /api/metrics export time. We surface P50 (median —
// "what does typical traffic look like"), P95 ("what does the
// long tail of the slowest 5% look like"), and P99 ("does the
// tail blow up"). Adding more quantiles is cheap; the cost is in
// the resulting metric cardinality (one additional gauge series
// per quantile per route × method × status).
var httpRequestQuantiles = []float64{0.5, 0.95, 0.99}

// bucketCountsForKey extracts the cumulative bucket counts for one
// (method, route, status) key out of the flat httpRequestDurationBuckets
// map. The map is keyed "<key>,le=<bucket>" and shares the bucket
// schedule with httpRequestDurationSecondsBuckets — we walk that
// schedule plus the implicit +Inf bucket and look up each entry.
// Returns a slice with len(httpRequestDurationSecondsBuckets)+1
// entries: [bucket0_count, bucket1_count, ..., +Inf_count].
func bucketCountsForKey(all map[string]int64, key string) []int64 {
	out := make([]int64, len(httpRequestDurationSecondsBuckets)+1)
	for i, bound := range httpRequestDurationSecondsBuckets {
		out[i] = all[fmt.Sprintf("%s,le=%s", key, prometheusFloat(bound))]
	}
	out[len(out)-1] = all[fmt.Sprintf("%s,le=+Inf", key)]
	return out
}

// histogramQuantile estimates a quantile from a Prometheus-style
// cumulative-bucket histogram via linear interpolation, mirroring
// the standard histogram_quantile() PromQL function.
//
// boundaries: the upper bounds for each finite bucket (ascending).
// counts:     cumulative observations falling into each bucket;
//             counts[len(boundaries)] is the +Inf bucket.
// total:      the total observation count (equals counts[+Inf]).
// q:          the target quantile in (0, 1).
//
// Behaviour:
//   - q ≤ 0: returns 0.
//   - q ≥ 1: returns the largest finite bucket boundary (capped).
//   - When the rank lands inside finite bucket i, interpolates
//     linearly between boundaries[i-1] (or 0) and boundaries[i].
//   - When the rank lands in the +Inf bucket, returns the largest
//     finite boundary (caller should know the histogram is
//     under-resolved at the tail).
func histogramQuantile(boundaries []float64, counts []int64, total int64, q float64) float64 {
	if total <= 0 || q <= 0 || len(boundaries) == 0 {
		return 0
	}
	if q >= 1 {
		return boundaries[len(boundaries)-1]
	}
	rank := q * float64(total)
	prevCount := int64(0)
	prevBound := 0.0
	for i, ub := range boundaries {
		c := counts[i]
		if float64(c) >= rank {
			// Rank lands in this bucket. Interpolate between
			// prevBound..ub by the fractional position of (rank -
			// prevCount) within (c - prevCount).
			delta := float64(c - prevCount)
			if delta <= 0 {
				return ub
			}
			frac := (rank - float64(prevCount)) / delta
			return prevBound + (ub-prevBound)*frac
		}
		prevCount = c
		prevBound = ub
	}
	// Fell through to +Inf bucket; cap at the largest finite bound.
	return boundaries[len(boundaries)-1]
}

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
		cashLedgerWriteFailures:       make(map[string]int64),
		fundingRequestEvents:          make(map[string]int64),
		fxEvents:                      make(map[string]int64),
		reconEvents:                   make(map[string]int64),
		surveillanceEvents:            make(map[string]int64),
		drawdownEvents:                make(map[string]int64),
		marketStatusEvents:            make(map[string]int64),
		marketImpactEvents:            make(map[string]int64),
		lockupEvents:                  make(map[string]int64),
		borrowEvents:                  make(map[string]int64),
		priceCollarEvents:             make(map[string]int64),
		lotSizeEvents:                 make(map[string]int64),
		wsFeedEvents:                  make(map[string]int64),
		decisionInputBlocks:           make(map[string]int64),
		decisionExposureBreaches:      make(map[string]int64),
		decisionCooldownVetos:         make(map[string]int64),
		decisionRiskBudgetThrottled:   make(map[string]int64),
		corpActionIngestTicks:           make(map[string]int64),
		corpActionIngestProviderErrors:  make(map[string]int64),
		corpActionIngestRetries:         make(map[string]int64),
		corpActionIngestEvents:          make(map[string]int64),
		corpActionIngestApply:           make(map[string]int64),
		abShadowLLMCalls:                make(map[string]int64),
		decisionSourceTotal:             make(map[string]int64),
		dailyPicksPublishBuckets:        make(map[string]int64),
		dailyPicksPublishSumSecs:        make(map[string]float64),
		dailyPicksPublishCount:          make(map[string]int64),
		complianceFilterBlocked:         make(map[string]int64),
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

// ObservePMDecisionSource (Sprint 11.4) records one PM decision keyed
// by its provenance tag. category and provider are empty strings for
// the LLM-success rows (llm_pm, llm_three_stage) and populated from
// the errorclass.Detail for fallback rows. The series cardinality is
// bounded: 6 sources × 11 categories × ~5 providers = ~330 lines.
func (m *serverMetrics) ObservePMDecisionSource(source, category, provider string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(source) == "" {
		return
	}
	key := fmt.Sprintf("source=%s,category=%s,provider=%s",
		strings.TrimSpace(source),
		strings.TrimSpace(category),
		strings.TrimSpace(provider),
	)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisionSourceTotal[key]++
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

// RecordCorpActionTick increments the per-tick run counter. Status
// is one of:
//   - "ok"                    — runOnce reached the end of the
//     fetch+apply path (regardless of how many events flowed —
//     "no events today" is still ok).
//   - "skipped_not_leader"    — replica skipped the tick because
//     the lease is held elsewhere; expected on N-1 of N replicas.
//   - "skipped_no_holdings"   — no active funds with non-zero
//     positions, so there's nothing to ask upstream about.
func (m *serverMetrics) RecordCorpActionTick(status string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(status) == "" {
		status = "unknown"
	}
	now := time.Now().Unix()
	key := fmt.Sprintf("status=%s", status)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.corpActionIngestTicks[key]++
	m.corpActionIngestLastTickUnix = now
	if status == "ok" || status == "skipped_no_holdings" {
		// "ok" includes the case where we ran but the upstream
		// returned 0 events (entirely possible — most days have no
		// splits/dividends). We still treat that as a successful
		// observation: the loop reached out and got a response.
		m.corpActionIngestLastSuccessUnix = now
	}
}

// RecordCorpActionProviderError counts a failed provider fetch.
// outcome is "transient" for errors the retry helper considered
// worth retrying (EOF, connection reset, broken pipe) and "fatal"
// for everything else (4xx, malformed JSON, etc.). Distinguishing
// the two on the dashboard lets operators tell "the upstream is
// flaky" from "we have a real bug".
func (m *serverMetrics) RecordCorpActionProviderError(market, outcome string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(market) == "" {
		market = "unknown"
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = "fatal"
	}
	key := fmt.Sprintf("market=%s,outcome=%s", market, outcome)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.corpActionIngestProviderErrors[key]++
}

// RecordCorpActionRetry counts a retry attempt issued by the
// provider fetch wrapper. outcome is one of:
//   - "succeeded"  — the retry returned data.
//   - "exhausted"  — the retry budget ran out (1 attempt for now,
//     so this just means "retried once and it still failed").
// market labels which provider lane the retry happened on so a
// "Yahoo flaky / Eastmoney solid" pattern is visible in Grafana.
func (m *serverMetrics) RecordCorpActionRetry(market, outcome string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(market) == "" {
		market = "unknown"
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = "unknown"
	}
	key := fmt.Sprintf("market=%s,outcome=%s", market, outcome)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.corpActionIngestRetries[key]++
}

// RecordCorpActionEvent counts an event-level ingest result. action
// is the corporate-action type (split / cash_dividend / combined /
// other vendor-specific tags); phase is "upserted" or "upsert_error".
// One call per event per tick.
func (m *serverMetrics) RecordCorpActionEvent(action, phase string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(action) == "" {
		action = "unknown"
	}
	if strings.TrimSpace(phase) == "" {
		phase = "unknown"
	}
	key := fmt.Sprintf("action=%s,phase=%s", action, phase)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.corpActionIngestEvents[key]++
}

// RecordCorpActionApply counts how a single (event, fund) apply
// attempt resolved. outcome is "applied" / "missing" / "error".
// missing covers the corpaction.ErrPositionMissing race (position
// zeroed between collect and apply) which is silent in logs;
// surfacing it as a counter lets dashboards spot rates without
// log-grepping.
func (m *serverMetrics) RecordCorpActionApply(outcome string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = "unknown"
	}
	key := fmt.Sprintf("outcome=%s", outcome)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.corpActionIngestApply[key]++
}

// RecordABShadowLLMCall (Card K-5) increments the per-outcome
// counter that operators watch when AB_SHADOW_LLM_ENABLED=1 is
// turned on in production. The decider calls this exactly once
// per LLM attempt (whether the LLM was actually contacted or
// the request was short-circuited by the budget cap), so the
// `outcome` label tells the full story:
//
//   - "decided_by_llm" / "recap_decided_by_llm" — happy path.
//   - "fallback_llm_error" / "recap_fallback_llm_error" — the
//     model errored, network/timeout/refusal; synthetic rescue
//     kept the run alive.
//   - "fallback_parse_error" / "recap_fallback_parse_error" —
//     model spoke but the JSON shape was off.
//   - "fallback_budget_cap" — exceeded `AB_SHADOW_LLM_MAX_CALLS`
//     for this analyze run; we stopped paying for the rest.
//
// Nil-safe so the deterministic path in tests can call it
// without wiring a real metrics struct.
func (m *serverMetrics) RecordABShadowLLMCall(outcome string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = "unknown"
	}
	key := fmt.Sprintf("outcome=%s", outcome)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abShadowLLMCalls[key]++
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

// RecordCashLedgerWriteFailure increments the cash-ledger drift
// counter for a single entry_type. Surfaced through Prometheus
// as fundai_cash_ledger_write_failures_total — paired with a
// reconciliation alert (sum of cash_ledger != funds.current_capital)
// it tells operators which leg is silently failing.
func (m *serverMetrics) RecordCashLedgerWriteFailure(entryType string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(entryType) == "" {
		entryType = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cashLedgerWriteFailures[entryType]++
}

// RecordFundingRequestEvent bumps the per-state funding lifecycle
// counter (P1-2). event ∈ {created, cancelled, approved, rejected}.
// Surfaced through Prometheus as
// fundai_funding_request_events_total{event="..."} so dashboards
// can answer "how many deposits hit approved last week" without
// running the admin filter UI.
func (m *serverMetrics) RecordFundingRequestEvent(event string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(event) == "" {
		event = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fundingRequestEvents[event]++
}

// RecordFXEvent bumps the per-event FX counter (P1-4). event ∈
// {upsert_manual, upsert_override, fetch_ok, fetch_error,
//  convert_stale}. Surfaced through Prometheus as
// fundai_fx_events_total{event="..."} so dashboards can answer
// "is the daily fetch loop healthy" + "is NAV using stale rates"
// without grepping logs.
func (m *serverMetrics) RecordFXEvent(event string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(event) == "" {
		event = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fxEvents[event]++
}

// RecordReconEvent bumps the per-event recon counter (P1-3). event ∈
// {ingest_ok, ingest_duplicate, ingest_error, run_ok, run_failed,
//  break_<break_type>, resolve_<status>, scheduled_skip}.
// Surfaced through Prometheus as
// fundai_recon_events_total{event="..."} so dashboards can answer
// "did last night's recon land?" + "how many breaks are still
// open?" without scraping the DB.
func (m *serverMetrics) RecordReconEvent(event string) {
	if m == nil {
		return
	}
	if strings.TrimSpace(event) == "" {
		event = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconEvents[event]++
}

// RecordSurveillanceEvent bumps the per-event surveillance counter
// (P1-7). event ∈
//
//	{run_ok, run_failed, event_<rule_code>, severity_<severity>,
//	 review_<status>, insert_error, scheduled_skip}
//
// Exported through Prometheus as
// fundai_surveillance_events_total{event="..."} so dashboards can
// answer "how many wash-trade events fired this week?" or "are any
// criticals still open?" without scraping the DB.
func (m *serverMetrics) RecordSurveillanceEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.surveillanceEvents == nil {
		m.surveillanceEvents = make(map[string]int64)
	}
	m.surveillanceEvents[event]++
}

// RecordDrawdownEvent bumps the per-event drawdown counter (P3-5).
// event ∈
//
//	{check_ok, check_failed, breach_tier_<n>, action_<action>,
//	 review_<status>, auto_executed, policy_upsert, policy_delete,
//	 scheduled_skip}
//
// Exported via Prometheus as
// fundai_drawdown_events_total{event="..."} so dashboards can
// answer "how many funds breached tier 2 today?" or "is the loop
// running?" without scraping the DB.
func (m *serverMetrics) RecordDrawdownEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drawdownEvents == nil {
		m.drawdownEvents = make(map[string]int64)
	}
	m.drawdownEvents[event]++
}

// RecordMarketStatusEvent bumps the per-event market-status
// gate counter (S6.1). event is one of:
//
//	{allow, reject_<rule>, warn_<rule>, lookup_failed,
//	 calendar_lookup_failed, evaluate_failed, persist_failed,
//	 admin_halt, admin_unhalt, admin_set_limits,
//	 admin_calendar_upsert, admin_calendar_delete}
//
// Exported as fundai_marketstatus_events_total{event="..."}.
func (m *serverMetrics) RecordMarketStatusEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.marketStatusEvents == nil {
		m.marketStatusEvents = make(map[string]int64)
	}
	m.marketStatusEvents[event]++
}

// RecordLotSizeEvent bumps the per-event broker lot-size gate
// counter (S12.1). event is one of:
//
//	{allow, reject_a_share, reject_hk_equity, reject_us_equity,
//	 reject_futures, reject_crypto, reject_unknown_side,
//	 evaluate_failed}
//
// Exported as fundai_lotsize_events_total{event="..."}.
func (m *serverMetrics) RecordLotSizeEvent(event string) {
	if m == nil {
		return
	}
	event = strings.ToLower(strings.TrimSpace(event))
	if event == "" {
		event = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lotSizeEvents == nil {
		m.lotSizeEvents = make(map[string]int64)
	}
	m.lotSizeEvents[event]++
}

// RecordPriceCollarEvent bumps the per-event broker price-collar
// gate counter. event is one of:
//
//	{allow, reject_price_collar, warn_price_collar_no_reference,
//	 reject_price_collar_no_reference, evaluate_failed}
//
// Exported as fundai_pricecollar_events_total{event="..."}.
func (m *serverMetrics) RecordPriceCollarEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.priceCollarEvents == nil {
		m.priceCollarEvents = make(map[string]int64)
	}
	m.priceCollarEvents[event]++
}

// RecordMarketImpactEvent bumps the per-event counter for the
// size-aware slippage estimator (S6.2). Exported as
// fundai_marketimpact_events_total{event="..."}.
func (m *serverMetrics) RecordMarketImpactEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.marketImpactEvents == nil {
		m.marketImpactEvents = make(map[string]int64)
	}
	m.marketImpactEvents[event]++
}

// RecordLockupEvent bumps the per-event counter for the
// IPO lock-up gate (S6.3). Exported as
// fundai_lockup_events_total{event="..."}.
func (m *serverMetrics) RecordLockupEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockupEvents == nil {
		m.lockupEvents = make(map[string]int64)
	}
	m.lockupEvents[event]++
}

// RecordBorrowEvent bumps the per-event counter for the
// securities-borrow gate + daily accrual loop (S6.4). Exported
// as fundai_borrow_events_total{event="..."}.
func (m *serverMetrics) RecordBorrowEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.borrowEvents == nil {
		m.borrowEvents = make(map[string]int64)
	}
	m.borrowEvents[event]++
}

// RecordWSFeedEvent bumps the per-event counter for the
// WebSocket real-time market data plumbing (S6.5). Exported as
// fundai_wsfeed_events_total{event="..."}.
func (m *serverMetrics) RecordWSFeedEvent(event string) {
	if m == nil {
		return
	}
	event = strings.TrimSpace(event)
	if event == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wsFeedEvents == nil {
		m.wsFeedEvents = make(map[string]int64)
	}
	m.wsFeedEvents[event]++
}

// ObserveDailyPicksPublish records one preset-publish run.
// preset is the daily-picks preset key (e.g. "growth", "value");
// d is the wall-clock duration of the publish call. Safe to call
// with m == nil or preset == "".
func (m *serverMetrics) ObserveDailyPicksPublish(preset string, d time.Duration) {
	if m == nil {
		return
	}
	preset = strings.TrimSpace(preset)
	if preset == "" {
		preset = "unknown"
	}
	secs := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dailyPicksPublishSumSecs[preset] += secs
	m.dailyPicksPublishCount[preset]++
	for _, bucket := range dailyPicksPublishBucketsSeconds {
		if secs <= bucket {
			m.dailyPicksPublishBuckets[fmt.Sprintf("preset=%s,le=%s", preset, prometheusFloat(bucket))]++
		}
	}
	m.dailyPicksPublishBuckets[fmt.Sprintf("preset=%s,le=+Inf", preset)]++
}

// RecordComplianceFilterBlock bumps the counter for one redacted
// phrase. pattern matches compliance.Violation.Pattern (e.g.
// "guaranteed_return"); layer identifies the source surface
// ("advisor", "geo", "daily_picks"). Both labels are required —
// empty strings collapse to "unknown" so we never silently drop
// metrics rows on a logging-only path.
func (m *serverMetrics) RecordComplianceFilterBlock(pattern, layer string) {
	if m == nil {
		return
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = "unknown"
	}
	layer = strings.TrimSpace(layer)
	if layer == "" {
		layer = "unknown"
	}
	key := fmt.Sprintf("pattern=%s,layer=%s", pattern, layer)
	m.mu.Lock()
	m.complianceFilterBlocked[key]++
	m.mu.Unlock()
}

// SetSubscriptionMRR stores the most recently computed monthly
// recurring revenue in USD. Updated by an external refresher loop
// (see subscriptionMRRLoop) so the metric reflects the latest
// committed snapshot rather than the wall-clock of the scrape.
// Negative values are silently dropped — an MRR < 0 is always
// a computation bug, not a legitimate signal.
func (m *serverMetrics) SetSubscriptionMRR(usd float64) {
	if m == nil || usd < 0 {
		return
	}
	m.mu.Lock()
	m.subscriptionMRRUSD = usd
	m.mu.Unlock()
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
	// Computed quantile gauges. Self-contained derivation from the
	// bucket counters above via linear interpolation between bucket
	// boundaries — cheap, deterministic, and lets operators alert
	// on tail latency without standing up a Prometheus stack just
	// to call histogram_quantile(). The accuracy is bounded by the
	// bucket granularity: a 250 ms / 500 ms bucket pair caps the
	// P95 estimate's resolution at 250 ms in that range.
	lines = append(lines,
		"# HELP fundai_http_request_duration_seconds_quantile Self-derived P50/P95/P99 latency in seconds (bucket interpolation).",
		"# TYPE fundai_http_request_duration_seconds_quantile gauge",
	)
	for _, key := range sortedMetricKeys(m.httpRequestCount) {
		count := m.httpRequestCount[key]
		if count == 0 {
			continue
		}
		buckets := bucketCountsForKey(m.httpRequestDurationBuckets, key)
		for _, q := range httpRequestQuantiles {
			val := histogramQuantile(httpRequestDurationSecondsBuckets, buckets, count, q)
			lines = append(lines, fmt.Sprintf(
				"fundai_http_request_duration_seconds_quantile{%s,quantile=\"%s\"} %.6f",
				prometheusLabels(key), prometheusFloat(q), val,
			))
		}
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
		"# HELP fundai_cash_ledger_write_failures_total Total cash ledger row insert failures by entry_type (trade still completed; reconcile via re-run).",
		"# TYPE fundai_cash_ledger_write_failures_total counter",
	)
	for _, key := range sortedMetricKeys(m.cashLedgerWriteFailures) {
		lines = append(lines, fmt.Sprintf("fundai_cash_ledger_write_failures_total{entry_type=%q} %d", key, m.cashLedgerWriteFailures[key]))
	}
	lines = append(lines,
		"# HELP fundai_funding_request_events_total Funding-request lifecycle events by terminal state (created/cancelled/approved/rejected). Divide approved by created over the same window for the approval rate.",
		"# TYPE fundai_funding_request_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.fundingRequestEvents) {
		lines = append(lines, fmt.Sprintf("fundai_funding_request_events_total{event=%q} %d", key, m.fundingRequestEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_fx_events_total FX (foreign-exchange) lifecycle events by phase (P1-4). upsert_manual/override = operator wrote a rate; fetch_ok/error = scheduler attempted Yahoo; convert_stale = NAV / cash summary hit a missing rate. ",
		"# TYPE fundai_fx_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.fxEvents) {
		lines = append(lines, fmt.Sprintf("fundai_fx_events_total{event=%q} %d", key, m.fxEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_recon_events_total Total reconciliation lifecycle events (ingest, run, break, resolve).",
		"# TYPE fundai_recon_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.reconEvents) {
		lines = append(lines, fmt.Sprintf("fundai_recon_events_total{event=%q} %d", key, m.reconEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_surveillance_events_total Total trade surveillance lifecycle events (run, event_<rule>, severity, review).",
		"# TYPE fundai_surveillance_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.surveillanceEvents) {
		lines = append(lines, fmt.Sprintf("fundai_surveillance_events_total{event=%q} %d", key, m.surveillanceEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_drawdown_events_total Total drawdown soft-circuit-breaker events (check, breach, action, review, policy edits).",
		"# TYPE fundai_drawdown_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.drawdownEvents) {
		lines = append(lines, fmt.Sprintf("fundai_drawdown_events_total{event=%q} %d", key, m.drawdownEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_marketstatus_events_total Total market-status pre-trade gate events (allow/reject/warn/admin/internal-failures).",
		"# TYPE fundai_marketstatus_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.marketStatusEvents) {
		lines = append(lines, fmt.Sprintf("fundai_marketstatus_events_total{event=%q} %d", key, m.marketStatusEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_marketimpact_events_total Total size-aware slippage estimator events (estimate, used_defaults, used_adv_fallback, bucket_*, admin_*, cache_refresh_*).",
		"# TYPE fundai_marketimpact_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.marketImpactEvents) {
		lines = append(lines, fmt.Sprintf("fundai_marketimpact_events_total{event=%q} %d", key, m.marketImpactEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_lockup_events_total Total IPO / private-placement / restricted-share lock-up gate events.",
		"# TYPE fundai_lockup_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.lockupEvents) {
		lines = append(lines, fmt.Sprintf("fundai_lockup_events_total{event=%q} %d", key, m.lockupEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_borrow_events_total Total securities-borrow gate + daily accrual events.",
		"# TYPE fundai_borrow_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.borrowEvents) {
		lines = append(lines, fmt.Sprintf("fundai_borrow_events_total{event=%q} %d", key, m.borrowEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_pricecollar_events_total Total broker price-collar gate decisions (fat-finger / bad-quote limit price defence).",
		"# TYPE fundai_pricecollar_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.priceCollarEvents) {
		lines = append(lines, fmt.Sprintf("fundai_pricecollar_events_total{event=%q} %d", key, m.priceCollarEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_lotsize_events_total Total broker lot-size gate decisions (S12.1 market microstructure safety net).",
		"# TYPE fundai_lotsize_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.lotSizeEvents) {
		lines = append(lines, fmt.Sprintf("fundai_lotsize_events_total{event=%q} %d", key, m.lotSizeEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_wsfeed_events_total Total WebSocket real-time market-data plumbing events (S6.5).",
		"# TYPE fundai_wsfeed_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.wsFeedEvents) {
		lines = append(lines, fmt.Sprintf("fundai_wsfeed_events_total{event=%q} %d", key, m.wsFeedEvents[key]))
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
	// Card G — corp-action ingest health. These counters/gauges
	// power the "is the daily ingest still running?" alerting on
	// the operator dashboard. See docs/PROMETHEUS_QUERIES.md for
	// the canonical alert expressions.
	lines = append(lines,
		"# HELP fundai_corp_action_ingest_ticks_total Total runs of the 12h corp-action ingest loop, partitioned by status (ok / skipped_not_leader / skipped_no_holdings).",
		"# TYPE fundai_corp_action_ingest_ticks_total counter",
	)
	for _, key := range sortedMetricKeys(m.corpActionIngestTicks) {
		lines = append(lines, fmt.Sprintf("fundai_corp_action_ingest_ticks_total{%s} %d", prometheusLabels(key), m.corpActionIngestTicks[key]))
	}
	lines = append(lines,
		"# HELP fundai_corp_action_ingest_provider_errors_total Provider fetch failures during corp-action ingest by market and outcome (transient / fatal).",
		"# TYPE fundai_corp_action_ingest_provider_errors_total counter",
	)
	for _, key := range sortedMetricKeys(m.corpActionIngestProviderErrors) {
		lines = append(lines, fmt.Sprintf("fundai_corp_action_ingest_provider_errors_total{%s} %d", prometheusLabels(key), m.corpActionIngestProviderErrors[key]))
	}
	lines = append(lines,
		"# HELP fundai_corp_action_ingest_retries_total Provider fetch retries issued during corp-action ingest by market and outcome (succeeded / exhausted).",
		"# TYPE fundai_corp_action_ingest_retries_total counter",
	)
	for _, key := range sortedMetricKeys(m.corpActionIngestRetries) {
		lines = append(lines, fmt.Sprintf("fundai_corp_action_ingest_retries_total{%s} %d", prometheusLabels(key), m.corpActionIngestRetries[key]))
	}
	lines = append(lines,
		"# HELP fundai_corp_action_ingest_events_total Per-event corp-action ingest results by action type and phase (upserted / upsert_error).",
		"# TYPE fundai_corp_action_ingest_events_total counter",
	)
	for _, key := range sortedMetricKeys(m.corpActionIngestEvents) {
		lines = append(lines, fmt.Sprintf("fundai_corp_action_ingest_events_total{%s} %d", prometheusLabels(key), m.corpActionIngestEvents[key]))
	}
	lines = append(lines,
		"# HELP fundai_corp_action_ingest_apply_total Per-(event,fund) apply outcomes during corp-action ingest (applied / missing / error).",
		"# TYPE fundai_corp_action_ingest_apply_total counter",
	)
	for _, key := range sortedMetricKeys(m.corpActionIngestApply) {
		lines = append(lines, fmt.Sprintf("fundai_corp_action_ingest_apply_total{%s} %d", prometheusLabels(key), m.corpActionIngestApply[key]))
	}
	lines = append(lines,
		"# HELP fundai_corp_action_ingest_last_tick_unix Unix seconds of the most recent corp-action ingest tick (any outcome). 0 = the loop has not run yet.",
		"# TYPE fundai_corp_action_ingest_last_tick_unix gauge",
		fmt.Sprintf("fundai_corp_action_ingest_last_tick_unix %d", m.corpActionIngestLastTickUnix),
		"# HELP fundai_corp_action_ingest_last_success_unix Unix seconds of the most recent successful corp-action ingest tick (ok or skipped_no_holdings). 0 = no successful run yet.",
		"# TYPE fundai_corp_action_ingest_last_success_unix gauge",
		fmt.Sprintf("fundai_corp_action_ingest_last_success_unix %d", m.corpActionIngestLastSuccessUnix),
	)
	// Card K-5 — AB shadow LLM call accounting. Single counter,
	// partitioned by outcome so operators can build a "burn vs
	// fallback rate" panel from one metric.
	lines = append(lines,
		"# HELP fundai_ab_shadow_llm_calls_total LLM-shadow B-side decision attempts during AB analyze, partitioned by outcome (decided_by_llm / fallback_llm_error / fallback_parse_error / fallback_budget_cap / recap_decided_by_llm / recap_fallback_llm_error / recap_fallback_parse_error). Each AnalyzeTest run with AB_SHADOW_LLM_ENABLED=1 will increment this counter once per trade and once for the recap; the per-trade outcomes drive cost and the fallback_* outcomes drive reliability alerts.",
		"# TYPE fundai_ab_shadow_llm_calls_total counter",
	)
	for _, key := range sortedMetricKeys(m.abShadowLLMCalls) {
		lines = append(lines, fmt.Sprintf("fundai_ab_shadow_llm_calls_total{%s} %d", prometheusLabels(key), m.abShadowLLMCalls[key]))
	}
	// Sprint 11.4 — decision-source counter. Labels:
	//   source     llm_pm / llm_three_stage / fallback_* / legacy
	//   category   errorclass.Category (only set on fallback_*)
	//   provider   openai / claude / gemini / "" (only set on fallback_*
	//              where the request reached the provider before failing)
	//
	// Operator queries:
	//   sum(rate(fundai_pm_decision_total{source=~"fallback_.*"}[5m])) /
	//     sum(rate(fundai_pm_decision_total[5m]))
	//   → fallback rate; alert when > 0.05 sustained 30m.
	//
	//   sum by (category)(rate(fundai_pm_decision_total{source="fallback_after_llm_error"}[1h]))
	//   → top failure causes for an operator briefing.
	lines = append(lines,
		"# HELP fundai_pm_decision_total PM decisions partitioned by provenance source and (for fallback rows) errorclass category and provider.",
		"# TYPE fundai_pm_decision_total counter",
	)
	for _, key := range sortedMetricKeys(m.decisionSourceTotal) {
		lines = append(lines, fmt.Sprintf("fundai_pm_decision_total{%s} %d", prometheusLabels(key), m.decisionSourceTotal[key]))
	}

	// B2 — daily-picks publish duration histogram. Bucket counts +
	// _sum + _count match the standard Prometheus histogram shape so
	// histogram_quantile() works out of the box.
	lines = append(lines,
		"# HELP dailypicks_publish_duration_seconds Per-preset daily-picks publish duration in seconds.",
		"# TYPE dailypicks_publish_duration_seconds histogram",
	)
	for _, key := range sortedMetricKeys(m.dailyPicksPublishBuckets) {
		lines = append(lines, fmt.Sprintf("dailypicks_publish_duration_seconds_bucket{%s} %d", prometheusLabels(key), m.dailyPicksPublishBuckets[key]))
	}
	for _, key := range sortedMetricKeys(m.dailyPicksPublishSumSecs) {
		lines = append(lines, fmt.Sprintf("dailypicks_publish_duration_seconds_sum{preset=%q} %.6f", key, m.dailyPicksPublishSumSecs[key]))
	}
	for _, key := range sortedMetricKeys(m.dailyPicksPublishCount) {
		lines = append(lines, fmt.Sprintf("dailypicks_publish_duration_seconds_count{preset=%q} %d", key, m.dailyPicksPublishCount[key]))
	}

	// B2 — compliance filter block counter, partitioned by (pattern,
	// layer). Powers "which forbidden phrase is the platform
	// catching the most this week?" dashboards plus a fast SRE
	// alert ("a single pattern just spiked 10× — a master prompt
	// is now leaking forbidden phrases on every call").
	lines = append(lines,
		"# HELP compliance_filter_blocked_total Compliance phrases redacted, by regex pattern and surface layer.",
		"# TYPE compliance_filter_blocked_total counter",
	)
	for _, key := range sortedMetricKeys(m.complianceFilterBlocked) {
		lines = append(lines, fmt.Sprintf("compliance_filter_blocked_total{%s} %d", prometheusLabels(key), m.complianceFilterBlocked[key]))
	}

	// B2 — subscription MRR (USD). Single series, refreshed by the
	// subscriptionMRRLoop goroutine. Exposed as a snapshot gauge so
	// downstream dashboards can graph MRR-over-time without joining
	// against the subscriptions table directly.
	lines = append(lines,
		"# HELP subscription_mrr_usd Monthly recurring revenue in USD (snapshot, refreshed on a slow ticker).",
		"# TYPE subscription_mrr_usd gauge",
		fmt.Sprintf("subscription_mrr_usd %.2f", m.subscriptionMRRUSD),
	)

	return strings.Join(append(lines, ""), "\n")
}

func exportRuntimePrometheus(db *sql.DB, leaseManager *scheduler.LeaseManager) string {
	var lines []string
	if db != nil {
		stats := db.Stats()
		// Pool saturation gauge (%): 100 × InUse / MaxOpen. When
		// MaxOpen is 0 the pool is unlimited and the ratio is
		// undefined — emit -1 so dashboards can mask the panel.
		// Operators alert on this gauge approaching 90%, which is
		// the canonical "your service is about to hang" signal
		// before the wait_count counter starts climbing.
		utilization := -1.0
		if stats.MaxOpenConnections > 0 {
			utilization = 100.0 * float64(stats.InUse) / float64(stats.MaxOpenConnections)
		}
		// Wait-duration average (seconds per wait). Helps separate
		// "many small waits" from "one giant stall" — both share
		// the same wait_count number but mean very different things
		// for tail latency. -1 when wait_count is 0.
		waitAvg := -1.0
		if stats.WaitCount > 0 {
			waitAvg = stats.WaitDuration.Seconds() / float64(stats.WaitCount)
		}
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
			"# HELP fundai_db_pool_utilization_pct Pool utilization in percent (100 * in_use / max_open). -1 when max_open is unlimited.",
			"# TYPE fundai_db_pool_utilization_pct gauge",
			fmt.Sprintf("fundai_db_pool_utilization_pct %.2f", utilization),
			"# HELP fundai_db_wait_count_total Total database connection waits due to pool saturation.",
			"# TYPE fundai_db_wait_count_total counter",
			fmt.Sprintf("fundai_db_wait_count_total %d", stats.WaitCount),
			"# HELP fundai_db_wait_duration_seconds_total Total time spent waiting for database connections.",
			"# TYPE fundai_db_wait_duration_seconds_total counter",
			fmt.Sprintf("fundai_db_wait_duration_seconds_total %.6f", stats.WaitDuration.Seconds()),
			"# HELP fundai_db_wait_avg_seconds Average wait time per acquisition that had to block on the pool. -1 when no waits have occurred.",
			"# TYPE fundai_db_wait_avg_seconds gauge",
			fmt.Sprintf("fundai_db_wait_avg_seconds %.6f", waitAvg),
			"# HELP fundai_db_max_idle_closed_total Total connections closed due to MaxIdleConns enforcement. Spikes mean the pool is over-trimming idle conns.",
			"# TYPE fundai_db_max_idle_closed_total counter",
			fmt.Sprintf("fundai_db_max_idle_closed_total %d", stats.MaxIdleClosed),
			"# HELP fundai_db_max_idle_time_closed_total Total connections closed due to ConnMaxIdleTime expiry.",
			"# TYPE fundai_db_max_idle_time_closed_total counter",
			fmt.Sprintf("fundai_db_max_idle_time_closed_total %d", stats.MaxIdleTimeClosed),
			"# HELP fundai_db_max_lifetime_closed_total Total connections closed due to ConnMaxLifetime expiry.",
			"# TYPE fundai_db_max_lifetime_closed_total counter",
			fmt.Sprintf("fundai_db_max_lifetime_closed_total %d", stats.MaxLifetimeClosed),
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

	// Daily FX-rate fetch loop (P1-4). Runs in-process with the
	// rest of the server because the supported pair list is small
	// (6 pairs / 6h) and standing up a separate worker for it
	// would dwarf the work it actually performs. Leader election
	// is currently single-replica trust — when we promote to
	// multi-replica deployments, gate the loop on the same
	// schedulerLeader flag the workflow scheduler uses.
	{
		fxRepo := fx.NewRepo(db)
		fxProvider := fx.NewYahooProvider(fx.YahooProviderOptions{})
		fxLoopHandle := newFXLoop(fxRepo, fxProvider, svc.Metrics, slogLeveledLogger{}, fxLoopOptions{})
		go func() {
			fxLoopHandle.Run(context.Background())
		}()
	}

	// Daily reconciliation loop (P1-3). Diffs internal positions /
	// cash / trades against a (mock) broker statement and writes
	// reconciliation_runs + reconciliation_breaks. The mock
	// provider produces a perfect-mirror statement so a healthy
	// platform yields zero breaks; it's the scaffolding that lets
	// the dashboard and the hash-chained audit trail exist BEFORE
	// real broker statement loaders land. When the first real
	// loader lands (CSV / FIX), it replaces the mock provider
	// here without touching the rest of the loop.
	{
		reconRepo := recon.NewRepo(db)
		snapshotBuilder := newReconSnapshotBuilder(db)
		fundRepo := repository.NewFundRepo(db)
		reconLoopHandle := newReconLoop(reconRepo, snapshotBuilder, svc.Metrics, slogLeveledLogger{}, reconLoopOptions{
			FundLister: func(ctx context.Context) ([]string, error) {
				funds, err := fundRepo.ListActive(ctx)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(funds))
				for _, f := range funds {
					ids = append(ids, f.ID)
				}
				return ids, nil
			},
		})
		go func() {
			reconLoopHandle.Run(context.Background())
		}()
	}

	// S8.4 — kick off the per-agent reputation backfill loop.
	// Runs once per 24h on every active fund. No-op when the
	// repo / loop weren't wired (e.g. tests).
	if svc.AgentReputationLoop != nil {
		go func() {
			svc.AgentReputationLoop.Run(context.Background())
		}()
	}

	// Migration 112 — transactional outbox flusher. Drains
	// outbox_events via SELECT … FOR UPDATE SKIP LOCKED so
	// multi-replica deploys are safe. v1 handler is the bundled
	// LoggingHandler which just records the event in slog; real
	// downstream sinks (Kafka, S3, public provenance feed) are
	// chained in via MultiHandler in a follow-up PR. The flusher
	// runs unconditionally — an empty queue is the cheap idle path.
	{
		flusher := outbox.NewFlusher(
			db,
			outbox.LoggingHandler(slog.Default()),
			outbox.FlusherOptions{
				PollInterval:   10 * time.Second,
				BatchSize:      64,
				HandlerTimeout: 30 * time.Second,
				Logger:         slog.Default(),
			},
		)
		go func() {
			_ = flusher.Run(context.Background())
		}()
	}

	// Phase 5 — advisor reputation backfill loop. Runs once per
	// 24h, scoped to advisor_consultations (fund-less). No-op
	// when AdvisorService or AgentReputationRepo weren't wired.
	if svc.AdvisorReputationLoop != nil {
		go func() {
			svc.AdvisorReputationLoop.Run(context.Background())
		}()
	}
	if svc.DailyPicksLoop != nil {
		go func() {
			svc.DailyPicksLoop.Run(context.Background())
		}()
	}

	// Sprint 13 — model A/B promotion scanner. Same nightly cadence
	// as the reputation loop; nil-safe when the modelab repo or the
	// reporter weren't wired.
	if svc.ModelABPromotionScanLoop != nil {
		go func() {
			svc.ModelABPromotionScanLoop.Run(context.Background())
		}()
	}

	// B2 — subscription MRR refresher. Hourly COUNT(*) GROUP BY
	// plan_tier so the subscription_mrr_usd gauge tracks the
	// live revenue snapshot for the SRE / ops dashboards. nil-safe
	// when DB or metrics aren't wired (test binaries).
	if mrrLoop := newSubscriptionMRRLoop(db, svc.Metrics, subscriptionMRRLoopOptions{}); mrrLoop != nil {
		go mrrLoop.Run(context.Background())
	}

	// P1-7 — trade surveillance scheduler. Hourly intraday scan
	// of every active fund's day-of trades. The same fundRepo is
	// re-used; we don't re-construct it because the wiring above
	// already declares it inside that block.
	{
		surveillanceRepo := surveillance.NewRepo(db)
		surveillanceBuilder := newSurveillanceSnapshotBuilder(db)
		surveillanceFundRepo := repository.NewFundRepo(db)
		surveillanceLoopHandle := newSurveillanceLoop(surveillanceRepo, surveillanceBuilder, svc.Metrics, slogLeveledLogger{}, surveillanceLoopOptions{
			FundLister: func(ctx context.Context) ([]string, error) {
				funds, err := surveillanceFundRepo.ListActive(ctx)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(funds))
				for _, f := range funds {
					ids = append(ids, f.ID)
				}
				return ids, nil
			},
		})
		go func() {
			surveillanceLoopHandle.Run(context.Background())
		}()
	}

	// P3-5 — drawdown soft circuit breaker. 5min scan of every
	// active fund's drawdown vs configured tier policy. Re-uses
	// the fund_repo; auto-execute path is left unwired (nil
	// handler) until the order pipeline integration lands. With
	// nil handler the loop persists every breach as 'proposed'
	// for operator review — this is the safe default for a soft
	// breaker that proposes but does not execute.
	{
		ddRepo := drawdown.NewRepo(db)
		ddBuilder := newDrawdownSnapshotBuilder(db, ddRepo)
		ddFundRepo := repository.NewFundRepo(db)
		ddLoopHandle := newDrawdownLoop(ddRepo, ddBuilder, svc.Metrics, slogLeveledLogger{}, drawdownLoopOptions{
			FundLister: func(ctx context.Context) ([]string, error) {
				funds, err := ddFundRepo.ListActive(ctx)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(funds))
				for _, f := range funds {
					ids = append(ids, f.ID)
				}
				return ids, nil
			},
		})
		go func() {
			ddLoopHandle.Run(context.Background())
		}()
	}

	// S6.4 borrow accrual loop. Runs once per day at 23:55 UTC
	// (HourOfDay=23 + tick interval 1h → fires inside the 23rd
	// hour window). Leader-gated so multi-instance deployments
	// don't double-book.
	if svc.BorrowRepo != nil {
		accrualLoop := newBorrowAccrualLoop(borrowAccrualConfig{
			DB:         svc.DB,
			BorrowRepo: svc.BorrowRepo,
			Cache:      svc.BorrowCache,
			CashRepo:   repository.NewCashLedgerRepo(svc.DB),
			Metrics:    svc.Metrics,
			Logger:     slog.Default(),
			Interval:   1 * time.Hour,
			HourOfDay:  23,
			DayCount:   365,
		})
		accrualLoop.Start(context.Background())
	}

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
