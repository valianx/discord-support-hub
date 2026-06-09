package config_test

import (
	"os"
	"testing"

	"github.com/valianx/discord-support-hub/internal/config"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear any env vars that might leak from the test environment.
	clearEnv(t, "HTTP_ADDR", "VALKEY_ADDR", "LOG_LEVEL", "WORKER_CONCURRENCY",
		"VALKEY_DB", "CORS_ALLOWED_ORIGINS", "AGENT_NICKNAME_SUFFIX")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr: want :8080, got %q", cfg.HTTPAddr)
	}
	if cfg.ValkeyAddr != "localhost:6379" {
		t.Errorf("ValkeyAddr: want localhost:6379, got %q", cfg.ValkeyAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel: want info, got %q", cfg.LogLevel)
	}
	if cfg.WorkerConcurrency != 10 {
		t.Errorf("WorkerConcurrency: want 10, got %d", cfg.WorkerConcurrency)
	}
	if cfg.ValkeyDB != 0 {
		t.Errorf("ValkeyDB: want 0, got %d", cfg.ValkeyDB)
	}
	if len(cfg.CORSAllowedOrigins) != 0 {
		t.Errorf("CORSAllowedOrigins: want empty, got %v", cfg.CORSAllowedOrigins)
	}
	// Agent nickname suffix must default to empty (feature off by default, FR-24).
	if cfg.AgentNicknameSuffix != "" {
		t.Errorf("AgentNicknameSuffix: want empty (off by default), got %q", cfg.AgentNicknameSuffix)
	}
}

func TestLoad_CORSParsing(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://a.example.com, https://b.example.com ,https://c.example.com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if len(cfg.CORSAllowedOrigins) != 3 {
		t.Errorf("CORSAllowedOrigins: want 3 items, got %d: %v", len(cfg.CORSAllowedOrigins), cfg.CORSAllowedOrigins)
	}
}

func TestLoad_InvalidWorkerConcurrency(t *testing.T) {
	t.Setenv("WORKER_CONCURRENCY", "not-a-number")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() should have returned an error for invalid WORKER_CONCURRENCY")
	}
}

func TestRequireDiscordToken(t *testing.T) {
	clearEnv(t, "DISCORD_BOT_TOKEN")

	cfg, _ := config.Load()
	if err := cfg.RequireDiscordToken(); err == nil {
		t.Error("RequireDiscordToken() should return error when token is empty")
	}

	t.Setenv("DISCORD_BOT_TOKEN", "test-token")
	cfg2, _ := config.Load()
	if err := cfg2.RequireDiscordToken(); err != nil {
		t.Errorf("RequireDiscordToken() unexpected error: %v", err)
	}
}

// TestRequireAgentRoleID verifies the boot-time guard that DISCORD_AGENT_ROLE_ID
// must be present and distinct from DISCORD_GUILD_ID (fix NFR-5).
func TestRequireAgentRoleID(t *testing.T) {
	t.Run("missing agent role id returns error", func(t *testing.T) {
		clearEnv(t, "DISCORD_AGENT_ROLE_ID", "DISCORD_GUILD_ID")
		cfg, _ := config.Load()
		if err := cfg.RequireAgentRoleID(); err == nil {
			t.Error("RequireAgentRoleID() should return error when DISCORD_AGENT_ROLE_ID is empty")
		}
	})

	t.Run("agent role id equals guild id returns error (would be @everyone)", func(t *testing.T) {
		t.Setenv("DISCORD_GUILD_ID", "guild-123")
		t.Setenv("DISCORD_AGENT_ROLE_ID", "guild-123") // same as guild id = @everyone
		cfg, _ := config.Load()
		if err := cfg.RequireAgentRoleID(); err == nil {
			t.Error("RequireAgentRoleID() should return error when AgentRoleID equals GuildID (@everyone)")
		}
	})

	t.Run("distinct agent role id is valid", func(t *testing.T) {
		t.Setenv("DISCORD_GUILD_ID", "guild-123")
		t.Setenv("DISCORD_AGENT_ROLE_ID", "agent-role-456") // distinct
		cfg, _ := config.Load()
		if err := cfg.RequireAgentRoleID(); err != nil {
			t.Errorf("RequireAgentRoleID() unexpected error for valid config: %v", err)
		}
	})
}

func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old := os.Getenv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if old != "" {
				os.Setenv(k, old)
			}
		})
	}
}
