package subscription

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
	"github.com/google/uuid"
)

// UserModelConfig 用户的模型配置
type UserModelConfig struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	AgentID         *string   `json:"agent_id,omitempty"`
	ConfigType      string    `json:"config_type"`
	Tier            *string   `json:"tier,omitempty"`
	Provider        string    `json:"provider"`
	ModelName       string    `json:"model_name"`
	BaseURL         *string   `json:"base_url,omitempty"`
	APIKeyEncrypted *string   `json:"-"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ConnectionTestResult 连接测试结果
type ConnectionTestResult struct {
	Success bool   `json:"success"`
	Latency int    `json:"latency_ms"`
	Message string `json:"message"`
	ModelID string `json:"model_id,omitempty"`
}

type RuntimeModelConfig struct {
	UserID     string
	AgentID    *string
	ConfigType string
	Tier       *llm.ModelTier
	Provider   llm.Provider
	ModelName  string
	BaseURL    string
	APIKey     string
	IsActive   bool
}

// ModelConfigService 模型配置服务
type ModelConfigService struct {
	db *sql.DB
}

func NewModelConfigService(db *sql.DB) *ModelConfigService {
	return &ModelConfigService{db: db}
}

func (s *ModelConfigService) SaveConfig(ctx context.Context, config *UserModelConfig) error {
	if config.UserID == "" {
		return fmt.Errorf("user_id is required")
	}
	if config.ConfigType != "tier_override" && config.ConfigType != "custom_endpoint" && config.ConfigType != "agent_default" {
		return fmt.Errorf("config_type must be 'tier_override', 'custom_endpoint' or 'agent_default'")
	}
	if config.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	if config.ModelName == "" {
		return fmt.Errorf("model_name is required")
	}
	if config.ConfigType == "tier_override" && (config.Tier == nil || strings.TrimSpace(*config.Tier) == "") {
		return fmt.Errorf("tier is required for tier_override config")
	}
	if config.ConfigType == "agent_default" {
		if config.AgentID == nil || strings.TrimSpace(*config.AgentID) == "" {
			return fmt.Errorf("agent_id is required for agent_default config")
		}
		config.Tier = nil
	}
	if err := validateProviderBoundary(config.Provider, config.BaseURL); err != nil {
		return err
	}

	if config.APIKeyEncrypted != nil {
		encrypted, err := normalizeAPIKeyForStorage(*config.APIKeyEncrypted)
		if err != nil {
			return err
		}
		config.APIKeyEncrypted = &encrypted
	}

	now := time.Now()
	if config.ID == "" {
		config.ID = uuid.New().String()
		config.CreatedAt = now
	}
	config.UpdatedAt = now
	if !config.IsActive {
		config.IsActive = true
	}

	if config.ID != "" {
		var existingID string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM user_model_configs WHERE id = $1 AND user_id = $2`, config.ID, config.UserID).Scan(&existingID)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("check model config existence: %w", err)
		}
		if err == sql.ErrNoRows {
			query := `
				INSERT INTO user_model_configs (id, user_id, agent_id, config_type, tier, provider,
				                                model_name, base_url, api_key_encrypted, is_active,
				                                created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			`
			_, err = s.db.ExecContext(ctx, query,
				config.ID, config.UserID, nullableStringValue(config.AgentID), config.ConfigType, config.Tier,
				config.Provider, config.ModelName, config.BaseURL, config.APIKeyEncrypted,
				config.IsActive, config.CreatedAt, config.UpdatedAt,
			)
			if err != nil {
				return fmt.Errorf("insert model config: %w", err)
			}
		} else {
			query := `
				UPDATE user_model_configs
				SET agent_id = $1, config_type = $2, tier = $3, provider = $4, model_name = $5,
				    base_url = $6, api_key_encrypted = COALESCE($7, api_key_encrypted),
				    is_active = $8, updated_at = $9
				WHERE id = $10 AND user_id = $11
			`
			result, err := s.db.ExecContext(ctx, query,
				nullableStringValue(config.AgentID), config.ConfigType, config.Tier, config.Provider, config.ModelName,
				config.BaseURL, config.APIKeyEncrypted, config.IsActive, config.UpdatedAt,
				config.ID, config.UserID,
			)
			if err != nil {
				return fmt.Errorf("update model config: %w", err)
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected: %w", err)
			}
			if rows == 0 {
				return fmt.Errorf("model config not found: %s", config.ID)
			}
		}
	}

	switch {
	case config.ConfigType == "tier_override" && config.IsActive:
		deactivateQuery := `
			UPDATE user_model_configs
			SET is_active = false, updated_at = $1
			WHERE user_id = $2 AND tier = $3 AND config_type = 'tier_override'
			  AND id != $4 AND is_active = true
		`
		_, err := s.db.ExecContext(ctx, deactivateQuery, now, config.UserID, config.Tier, config.ID)
		if err != nil {
			return fmt.Errorf("deactivate other configs: %w", err)
		}
	case config.ConfigType == "agent_default" && config.IsActive:
		deactivateQuery := `
			UPDATE user_model_configs
			SET is_active = false, updated_at = $1
			WHERE user_id = $2 AND agent_id = $3 AND config_type = 'agent_default'
			  AND id != $4 AND is_active = true
		`
		_, err := s.db.ExecContext(ctx, deactivateQuery, now, config.UserID, nullableStringValue(config.AgentID), config.ID)
		if err != nil {
			return fmt.Errorf("deactivate other agent configs: %w", err)
		}
	}

	return nil
}

func (s *ModelConfigService) GetUserConfigs(ctx context.Context, userID string) ([]*UserModelConfig, error) {
	query := `
		SELECT id, user_id, agent_id, config_type, tier, provider, model_name,
		       base_url, api_key_encrypted, is_active, created_at, updated_at
		FROM user_model_configs
		WHERE user_id = $1
		ORDER BY agent_id ASC NULLS FIRST, tier ASC NULLS LAST, created_at DESC
	`
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("query user model configs: %w", err)
	}
	defer rows.Close()

	configs := make([]*UserModelConfig, 0)
	for rows.Next() {
		c := &UserModelConfig{}
		var agentID sql.NullString
		if err := rows.Scan(
			&c.ID, &c.UserID, &agentID, &c.ConfigType, &c.Tier, &c.Provider,
			&c.ModelName, &c.BaseURL, &c.APIKeyEncrypted, &c.IsActive,
			&c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan model config: %w", err)
		}
		if agentID.Valid {
			value := strings.TrimSpace(agentID.String)
			c.AgentID = &value
		}
		configs = append(configs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate config rows: %w", err)
	}

	return configs, nil
}

func (s *ModelConfigService) DeleteConfig(ctx context.Context, userID, configID string) error {
	query := `DELETE FROM user_model_configs WHERE id = $1 AND user_id = $2`
	result, err := s.db.ExecContext(ctx, query, configID, userID)
	if err != nil {
		return fmt.Errorf("delete model config: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("model config not found: %s", configID)
	}
	return nil
}

func (s *ModelConfigService) ListRuntimeConfigs(ctx context.Context) ([]*RuntimeModelConfig, error) {
	query := `
		SELECT user_id, agent_id, config_type, tier, provider, model_name, base_url, api_key_encrypted, is_active
		FROM user_model_configs
		WHERE is_active = true
		ORDER BY user_id ASC, agent_id ASC NULLS FIRST, tier ASC NULLS LAST, created_at ASC
	`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query runtime model configs: %w", err)
	}
	defer rows.Close()

	configs := make([]*RuntimeModelConfig, 0)
	for rows.Next() {
		var (
			userID       string
			agentID      sql.NullString
			configType   string
			tierValue    sql.NullString
			provider     string
			modelName    string
			baseURL      sql.NullString
			apiKeyStored sql.NullString
			isActive     bool
		)
		if err := rows.Scan(&userID, &agentID, &configType, &tierValue, &provider, &modelName, &baseURL, &apiKeyStored, &isActive); err != nil {
			return nil, fmt.Errorf("scan runtime model config: %w", err)
		}

		runtimeConfig := &RuntimeModelConfig{
			UserID:     userID,
			ConfigType: configType,
			Provider:   llm.Provider(provider),
			ModelName:  modelName,
			BaseURL:    strings.TrimSpace(baseURL.String),
			IsActive:   isActive,
		}
		if agentID.Valid {
			value := strings.TrimSpace(agentID.String)
			runtimeConfig.AgentID = &value
		}
		if tierValue.Valid && strings.TrimSpace(tierValue.String) != "" {
			tier := llm.ModelTier(strings.TrimSpace(tierValue.String))
			runtimeConfig.Tier = &tier
		}
		if apiKeyStored.Valid && strings.TrimSpace(apiKeyStored.String) != "" {
			apiKey, err := resolveAPIKeyValue(apiKeyStored.String)
			if err != nil {
				return nil, fmt.Errorf("resolve api key for runtime config %s/%s: %w", userID, modelName, err)
			}
			runtimeConfig.APIKey = apiKey
		}
		if runtimeConfig.BaseURL == "" {
			resolvedBaseURL, err := resolveProviderBaseURL(provider, nil)
			if err != nil && (configType == "custom_endpoint" || configType == "agent_default") {
				return nil, fmt.Errorf("resolve base url for runtime config %s/%s: %w", userID, modelName, err)
			}
			runtimeConfig.BaseURL = resolvedBaseURL
		}
		configs = append(configs, runtimeConfig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runtime model configs: %w", err)
	}
	return configs, nil
}

func nullableStringValue(value *string) any {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

func (s *ModelConfigService) TestConnection(ctx context.Context, config *UserModelConfig) (*ConnectionTestResult, error) {
	result := &ConnectionTestResult{ModelID: config.ModelName}

	baseURL, err := resolveProviderBaseURL(config.Provider, config.BaseURL)
	if err != nil {
		result.Message = err.Error()
		return result, nil
	}

	apiKey, err := resolveAPIKeyForConnectionTest(config)
	if err != nil {
		result.Message = err.Error()
		return result, nil
	}
	if apiKey == "" {
		result.Message = "api_key is required"
		return result, nil
	}

	url, reqBody, headers, err := buildConnectionTestRequest(config.Provider, strings.TrimRight(baseURL, "/"), config.ModelName, apiKey)
	if err != nil {
		result.Message = fmt.Sprintf("build request: %v", err)
		return result, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		result.Message = fmt.Sprintf("create request: %v", err)
		return result, nil
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	result.Latency = int(time.Since(start).Milliseconds())
	if err != nil {
		result.Message = fmt.Sprintf("request failed: %v", err)
		return result, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		result.Success = true
		result.Message = "connection successful"
		return result, nil
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	result.Message = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))
	return result, nil
}

func EncryptAPIKey(key, secret string) (string, error) {
	block, err := aes.NewCipher(deriveEncryptionKey(secret))
	if err != nil {
		return "", fmt.Errorf("new cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("new gcm: %w", err)
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := aesGCM.Seal(nonce, nonce, []byte(key), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func normalizeAPIKeyForStorage(apiKey string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", fmt.Errorf("api_key is required")
	}
	if _, err := decryptAPIKeyFromStorage(apiKey); err == nil {
		return apiKey, nil
	}
	return encryptAPIKeyForStorage(apiKey)
}

func encryptAPIKeyForStorage(apiKey string) (string, error) {
	secret, err := getAPIKeyEncryptionSecret()
	if err != nil {
		return "", err
	}

	encrypted, err := EncryptAPIKey(apiKey, secret)
	if err != nil {
		return "", fmt.Errorf("encrypt api key for storage: %w", err)
	}
	return encrypted, nil
}

func buildConnectionTestRequest(provider, baseURL, modelName, apiKey string) (string, []byte, map[string]string, error) {
	headers := map[string]string{
		"Content-Type": "application/json",
	}

	switch resolveProviderProtocol(provider, baseURL) {
	case providerProtocolClaude:
		body, err := json.Marshal(map[string]any{
			"model":      modelName,
			"max_tokens": 5,
			"messages": []map[string]string{
				{"role": "user", "content": "Hello"},
			},
		})
		if err != nil {
			return "", nil, nil, fmt.Errorf("marshal claude request: %w", err)
		}
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
		return baseURL + "/messages", body, headers, nil
	case providerProtocolGemini:
		body, err := json.Marshal(map[string]any{
			"contents": []map[string]any{
				{"role": "user", "parts": []map[string]string{{"text": "Hello"}}},
			},
			"generationConfig": map[string]any{"maxOutputTokens": 5},
		})
		if err != nil {
			return "", nil, nil, fmt.Errorf("marshal gemini request: %w", err)
		}
		headers["Authorization"] = "Bearer " + apiKey
		return buildGeminiGenerateContentURL(baseURL, modelName), body, headers, nil
	default:
		body, err := json.Marshal(map[string]any{
			"model":      modelName,
			"max_tokens": 5,
			"messages": []map[string]string{
				{"role": "user", "content": "Hello"},
			},
		})
		if err != nil {
			return "", nil, nil, fmt.Errorf("marshal openai-compatible request: %w", err)
		}
		headers["Authorization"] = "Bearer " + apiKey
		return baseURL + "/chat/completions", body, headers, nil
	}
}

const (
	providerProtocolOpenAICompatible = "openai-compatible"
	providerProtocolClaude           = "claude"
	providerProtocolGemini           = "gemini"
)

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func validateProviderBoundary(provider string, baseURL *string) error {
	_, err := resolveProviderBaseURL(provider, baseURL)
	return err
}

func resolveProviderBaseURL(provider string, baseURL *string) (string, error) {
	trimmedBaseURL := ""
	if baseURL != nil {
		trimmedBaseURL = strings.TrimSpace(*baseURL)
	}
	if trimmedBaseURL != "" {
		return trimmedBaseURL, nil
	}

	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return firstNonEmptyString(strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")), "https://api.openai.com/v1"), nil
	case "claude", "anthropic":
		return firstNonEmptyString(strings.TrimSpace(os.Getenv("CLAUDE_BASE_URL")), strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")), "https://api.anthropic.com/v1"), nil
	case "deepseek":
		return firstNonEmptyString(strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL")), "https://api.deepseek.com/v1"), nil
	case "qwen":
		return firstNonEmptyString(strings.TrimSpace(os.Getenv("QWEN_BASE_URL")), "https://dashscope.aliyuncs.com/compatible-mode/v1"), nil
	case "gemini", "google":
		return firstNonEmptyString(strings.TrimSpace(os.Getenv("GEMINI_BASE_URL")), strings.TrimSpace(os.Getenv("GOOGLE_BASE_URL")), "https://generativelanguage.googleapis.com/v1beta"), nil
	case "custom":
		return "", fmt.Errorf("base_url is required for custom provider")
	default:
		return "", fmt.Errorf("invalid provider: %s", provider)
	}
}

func resolveProviderProtocol(provider, baseURL string) string {
	normalizedProvider := strings.ToLower(strings.TrimSpace(provider))
	normalizedBaseURL := strings.ToLower(strings.TrimSpace(baseURL))
	if normalizedProvider == "claude" || normalizedProvider == "anthropic" || strings.Contains(normalizedBaseURL, "anthropic.com") {
		return providerProtocolClaude
	}
	if normalizedProvider == "gemini" || normalizedProvider == "google" || strings.Contains(normalizedBaseURL, "generativelanguage.googleapis.com") || strings.Contains(normalizedBaseURL, "/v1beta") {
		return providerProtocolGemini
	}
	return providerProtocolOpenAICompatible
}

func buildGeminiGenerateContentURL(baseURL, modelName string) string {
	trimmedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	lowerBaseURL := strings.ToLower(trimmedBaseURL)
	if strings.HasSuffix(lowerBaseURL, "/v1beta") {
		return trimmedBaseURL + "/models/" + url.PathEscape(modelName) + ":generateContent"
	}
	return trimmedBaseURL + "/v1beta/models/" + url.PathEscape(modelName) + ":generateContent"
}

func resolveAPIKeyForConnectionTest(config *UserModelConfig) (string, error) {
	if config.APIKeyEncrypted == nil {
		return "", nil
	}
	return resolveAPIKeyValue(*config.APIKeyEncrypted)
}

func resolveAPIKeyValue(apiKey string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", nil
	}
	decrypted, err := decryptAPIKeyFromStorage(apiKey)
	if err == nil {
		return decrypted, nil
	}
	return apiKey, nil
}

func getAPIKeyEncryptionSecret() (string, error) {
	secret := strings.TrimSpace(os.Getenv("MODEL_CONFIG_API_KEY_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("API_KEY_ENCRYPTION_SECRET"))
	}
	if secret == "" {
		return "", fmt.Errorf("api key encryption secret is not configured; set MODEL_CONFIG_API_KEY_SECRET or API_KEY_ENCRYPTION_SECRET")
	}
	return secret, nil
}

func deriveEncryptionKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func decryptAPIKeyFromStorage(encrypted string) (string, error) {
	secret, err := getAPIKeyEncryptionSecret()
	if err != nil {
		return "", err
	}
	decrypted, err := DecryptAPIKey(encrypted, secret)
	if err != nil {
		return "", fmt.Errorf("decrypt api key from storage: %w", err)
	}
	return decrypted, nil
}

func DecryptAPIKey(encrypted, secret string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(deriveEncryptionKey(secret))
	if err != nil {
		return "", fmt.Errorf("new cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("new gcm: %w", err)
	}
	nonceSize := aesGCM.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}
