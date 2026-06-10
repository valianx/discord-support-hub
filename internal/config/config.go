// Package config loads all runtime settings from environment variables with sane defaults.
// All sensitive values (tokens, keys) are read from env only — never hardcoded (NFR-6, NFR-10).
package config

import (
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

	// CORS: comma-separated list of allowed origins (never "*" with credentials).
	CORSAllowedOrigins []string

	// Agent nickname suffix (FR-24): optional configurable suffix, off by default.
	// Empty string = feature disabled.
	AgentNicknameSuffix string // default ""

	// Log level: debug, info, warn, error. Default: info.
	LogLevel string

	// asynq server concurrency (workers per queue process).
	WorkerConcurrency int // default 10

	// ReconcileSweepCron is the cron expression for the scheduled full-guild reconcile sweep
	// (M5, AC-5). Uses cron syntax; default is every 5 minutes ("*/5 * * * *").
	// Set to "" to disable the scheduled sweep (useful for testing).
	ReconcileSweepCron string // default "*/5 * * * *"

	// SMTP relay settings for the notify queue (AC-M6-5, AC-M6-6).
	// All fields are config-by-env; credentials are never persisted or logged.
	// Validation occurs when a send is first attempted (not at boot).
	SMTPHost     string // SMTP_HOST; e.g. "smtp.mailgun.org"
	SMTPPort     int    // SMTP_PORT; default 587
	SMTPUsername string // SMTP_USERNAME
	SMTPPassword string // SMTP_PASSWORD — never logged
	SMTPFrom     string // SMTP_FROM; e.g. "support@example.com"

	// WelcomeMessage is the configurable text posted to the #bienvenida channel (AC-M6-7).
	// No hard-coded brand. Falls back to a generic default when empty.
	WelcomeMessage     string // WELCOME_MESSAGE
	WelcomeChannelName string // WELCOME_CHANNEL_NAME; default "bienvenida"
}

// Load reads all settings from environment variables and applies defaults.
// Returns an error only for values that are syntactically invalid (e.g. bad int).
// Callers that require specific values (e.g. DiscordBotToken) should validate after loading.
func Load() (*Config, error) {
	cfg := &Config{
		HTTPAddr:           getEnv("HTTP_ADDR", ":8080"),
		PostgresDSN:        getEnv("POSTGRES_DSN", ""),
		ValkeyAddr:         getEnv("VALKEY_ADDR", "localhost:6379"),
		ValkeyPassword:     getEnv("VALKEY_PASSWORD", ""),
		DiscordBotToken:    getEnv("DISCORD_BOT_TOKEN", ""),
		DiscordGuildID:     getEnv("DISCORD_GUILD_ID", ""),
		DiscordAgentRoleID: getEnv("DISCORD_AGENT_ROLE_ID", ""),
		DiscordCategoryID:  getEnv("DISCORD_CATEGORY_ID", ""),
		AgentNicknameSuffix: getEnv("AGENT_NICKNAME_SUFFIX", ""),
		LogLevel:            getEnv("LOG_LEVEL", "info"),
		ReconcileSweepCron:  getEnv("RECONCILE_SWEEP_CRON", "*/5 * * * *"),
		SMTPHost:            getEnv("SMTP_HOST", ""),
		SMTPUsername:        getEnv("SMTP_USERNAME", ""),
		SMTPPassword:        getEnv("SMTP_PASSWORD", ""),
		SMTPFrom:            getEnv("SMTP_FROM", ""),
		WelcomeMessage:      getEnv("WELCOME_MESSAGE", ""),
		WelcomeChannelName:  getEnv("WELCOME_CHANNEL_NAME", "bienvenida"),
	}

	var err error

	cfg.SMTPPort, err = getEnvInt("SMTP_PORT", 587)
	if err != nil {
		return nil, fmt.Errorf("config: SMTP_PORT: %w", err)
	}

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

// RequireAgentRoleID returns an error when DISCORD_AGENT_ROLE_ID is absent or equals
// DISCORD_GUILD_ID. The Agent role must be a real, distinct role — using the guild id
// would grant @everyone VIEW_CHANNEL on every space category, breaking multi-tenant
// isolation (NFR-5).
func (c *Config) RequireAgentRoleID() error {
	if c.DiscordAgentRoleID == "" {
		return fmt.Errorf("config: DISCORD_AGENT_ROLE_ID is required but not set")
	}
	if c.DiscordGuildID != "" && c.DiscordAgentRoleID == c.DiscordGuildID {
		return fmt.Errorf("config: DISCORD_AGENT_ROLE_ID must not equal DISCORD_GUILD_ID — " +
			"the guild id is the @everyone role; using it would make every channel world-readable (NFR-5)")
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
