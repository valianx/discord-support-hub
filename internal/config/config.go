// Package config loads all runtime settings from environment variables with sane defaults.
// All sensitive values (tokens, keys) are read from env only — never hardcoded (NFR-6, NFR-10).
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds every runtime setting the service needs.
// All fields are populated from environment variables; see .env.example for the full key list.
type Config struct {
	// HTTP server settings.
	HTTPAddr string // default ":8080"

	// PostgreSQL connection.
	PostgresDSN string // required

	// Valkey / Redis connection.
	ValkeyAddr     string // default "localhost:6379"
	ValkeyPassword string // default ""
	ValkeyDB       int    // default 0

	// Discord bot settings (NFR-6: bot token from env only, never persisted).
	DiscordBotToken    string // required
	DiscordGuildID     string // required
	DiscordAgentRoleID string // required; the Agent role all agents receive
	DiscordCategoryID  string // default category for new channels

	// Discord OAuth2 settings.
	DiscordOAuthClientID     string
	DiscordOAuthClientSecret string // NFR-6: secret from env only
	DiscordOAuthRedirectURL  string

	// CORS: comma-separated list of allowed origins (never "*" with credentials).
	CORSAllowedOrigins []string

	// AES-256-GCM encryption key for OAuth2 tokens at rest (NFR-6).
	// Must be exactly 32 bytes when base64-decoded.
	EncryptionKey string // required for OAuth2 token storage

	// Agent nickname suffix (FR-24): optional configurable suffix, off by default.
	// Empty string = feature disabled.
	AgentNicknameSuffix string // default ""

	// Log level: debug, info, warn, error. Default: info.
	LogLevel string

	// asynq server concurrency (workers per queue process).
	WorkerConcurrency int // default 10
}

// Load reads all settings from environment variables and applies defaults.
// Returns an error only for values that are syntactically invalid (e.g. bad int).
// Callers that require specific values (e.g. DiscordBotToken) should validate after loading.
func Load() (*Config, error) {
	cfg := &Config{
		HTTPAddr:                 getEnv("HTTP_ADDR", ":8080"),
		PostgresDSN:              getEnv("POSTGRES_DSN", ""),
		ValkeyAddr:               getEnv("VALKEY_ADDR", "localhost:6379"),
		ValkeyPassword:           getEnv("VALKEY_PASSWORD", ""),
		DiscordBotToken:          getEnv("DISCORD_BOT_TOKEN", ""),
		DiscordGuildID:           getEnv("DISCORD_GUILD_ID", ""),
		DiscordAgentRoleID:       getEnv("DISCORD_AGENT_ROLE_ID", ""),
		DiscordCategoryID:        getEnv("DISCORD_CATEGORY_ID", ""),
		DiscordOAuthClientID:     getEnv("DISCORD_OAUTH_CLIENT_ID", ""),
		DiscordOAuthClientSecret: getEnv("DISCORD_OAUTH_CLIENT_SECRET", ""),
		DiscordOAuthRedirectURL:  getEnv("DISCORD_OAUTH_REDIRECT_URL", ""),
		EncryptionKey:            getEnv("ENCRYPTION_KEY", ""),
		AgentNicknameSuffix:      getEnv("AGENT_NICKNAME_SUFFIX", ""),
		LogLevel:                 getEnv("LOG_LEVEL", "info"),
	}

	var err error

	cfg.ValkeyDB, err = getEnvInt("VALKEY_DB", 0)
	if err != nil {
		return nil, fmt.Errorf("config: VALKEY_DB: %w", err)
	}

	cfg.WorkerConcurrency, err = getEnvInt("WORKER_CONCURRENCY", 10)
	if err != nil {
		return nil, fmt.Errorf("config: WORKER_CONCURRENCY: %w", err)
	}

	rawOrigins := getEnv("CORS_ALLOWED_ORIGINS", "")
	if rawOrigins != "" {
		for _, o := range strings.Split(rawOrigins, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins, trimmed)
			}
		}
	}

	return cfg, nil
}

// RequireDiscordToken returns an error when the bot token is absent.
// Called at boot by cmd/api and cmd/worker; not called by cmd/migrate.
func (c *Config) RequireDiscordToken() error {
	if c.DiscordBotToken == "" {
		return fmt.Errorf("config: DISCORD_BOT_TOKEN is required but not set")
	}
	return nil
}

// RequirePostgresDSN returns an error when the DSN is absent.
func (c *Config) RequirePostgresDSN() error {
	if c.PostgresDSN == "" {
		return fmt.Errorf("config: POSTGRES_DSN is required but not set")
	}
	return nil
}

// RequireEncryptionKey returns an error when the AES-256-GCM key is absent.
func (c *Config) RequireEncryptionKey() error {
	if c.EncryptionKey == "" {
		return fmt.Errorf("config: ENCRYPTION_KEY is required but not set")
	}
	return nil
}

// ValidateEncryptionKey returns an error when ENCRYPTION_KEY is absent or does not
// decode to exactly 32 bytes (AES-256). Call this at startup before first use so
// misconfiguration fails loudly rather than on the first encrypt/decrypt call (NFR-6).
func (c *Config) ValidateEncryptionKey() error {
	if c.EncryptionKey == "" {
		return fmt.Errorf("config: ENCRYPTION_KEY is required but not set")
	}
	decoded, err := base64.StdEncoding.DecodeString(c.EncryptionKey)
	if err != nil {
		return fmt.Errorf("config: ENCRYPTION_KEY is not valid base64: %w", err)
	}
	if len(decoded) != 32 {
		return fmt.Errorf("config: ENCRYPTION_KEY must decode to exactly 32 bytes (AES-256), got %d", len(decoded))
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("expected integer, got %q", v)
	}
	return n, nil
}
