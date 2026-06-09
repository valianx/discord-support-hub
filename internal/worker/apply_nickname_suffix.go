// apply_nickname_suffix.go implements the KindApplyNicknameSuffix worker handler
// (M4, FR-24, AC-5).
//
// When AgentNicknameSuffix is configured (non-empty), the handler appends the suffix
// to the agent's current Discord display name via GuildMemberNickname. When marking
// is disabled (empty suffix), the handler is a no-op — no nickname change occurs.
//
// The suffix is vendor-agnostic: it is a plain string from config and may be anything
// the operator configures (e.g. "[Support]", "(Agent)", etc.).
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// discordNick is the Discord sub-interface needed by the marking handler.
type discordNick interface {
	SetNickname(ctx context.Context, guildID, discordUserID, nickname string) error
}

// nickmarkingConfig carries dependencies for the apply_nickname_suffix handler.
type nickmarkingConfig struct {
	store   store.Store
	discord discordNick
	guildID string
	// suffix is the configured nickname suffix. Empty = marking disabled.
	suffix string
}

type applyNicknameSuffixHandler struct {
	cfg nickmarkingConfig
}

func newApplyNicknameSuffixHandler(cfg nickmarkingConfig) asynq.HandlerFunc {
	if cfg.store == nil || cfg.discord == nil {
		return stubHandler(queue.KindApplyNicknameSuffix)
	}
	// AC-5: when suffix is empty, marking is disabled — return a no-op handler.
	if cfg.suffix == "" {
		return func(ctx context.Context, task *asynq.Task) error {
			slog.DebugContext(ctx, "apply_nickname_suffix: marking disabled (suffix not configured)")
			return nil
		}
	}
	h := &applyNicknameSuffixHandler{cfg: cfg}
	return h.handle
}

func (h *applyNicknameSuffixHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.ApplyNicknameSuffixPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: decode nickname_suffix payload: %v", asynq.SkipRetry, err)
	}

	if payload.UserID == "" {
		return fmt.Errorf("%w: apply_nickname_suffix: payload missing user_id", asynq.SkipRetry)
	}

	slog.InfoContext(ctx, "apply_nickname_suffix: starting",
		"user_id", payload.UserID, "guild_id", payload.GuildID)

	// Load the user to get their discord_user_id.
	user, err := h.cfg.store.GetUserByID(ctx, payload.UserID)
	if err != nil {
		return fmt.Errorf("apply_nickname_suffix: load user %s: %w", payload.UserID, err)
	}

	if user.DiscordUserID == nil {
		// User has not yet connected Discord — skip silently; the marking will be applied
		// when the agent joins via the OAuth2 flow and the role is projected.
		slog.InfoContext(ctx, "apply_nickname_suffix: user has no discord_user_id, skipping",
			"user_id", payload.UserID)
		return nil
	}

	guildID := payload.GuildID
	if guildID == "" {
		guildID = h.cfg.guildID
	}

	// Build the nickname: use display_name (or a fallback) + configured suffix.
	// We do not read the current Discord nickname to avoid a round-trip; we set
	// displayName + suffix which is idempotent on repeated application.
	displayName := ""
	if user.DisplayName != nil {
		displayName = *user.DisplayName
	}
	suffix := h.cfg.suffix
	if payload.Suffix != "" {
		// Payload-level suffix override (for testing or per-user customisation).
		suffix = payload.Suffix
	}

	nickname := buildNickname(displayName, suffix)

	if err := h.cfg.discord.SetNickname(ctx, guildID, *user.DiscordUserID, nickname); err != nil {
		return fmt.Errorf("apply_nickname_suffix: set nickname for %s: %w", *user.DiscordUserID, err)
	}

	slog.InfoContext(ctx, "apply_nickname_suffix: completed",
		"user_id", payload.UserID, "discord_user_id", *user.DiscordUserID, "nickname", nickname)
	return nil
}

// buildNickname appends the suffix to the display name with a space separator.
// When the suffix is already present at the end, it is not duplicated (idempotent).
// Discord nicknames are limited to 32 characters (code points, not bytes); truncation
// uses rune counts so multibyte UTF-8 characters are never split (SEC-M4-003).
func buildNickname(displayName, suffix string) string {
	if displayName == "" {
		return suffix
	}
	base := displayName
	// Idempotency: don't double-apply the suffix.
	if strings.HasSuffix(base, suffix) {
		return base
	}
	candidate := base + " " + suffix
	// fix(SEC-M4-003): Discord's 32-character limit is measured in code points, not bytes.
	// Count and truncate by rune so multibyte characters (e.g. CJK, emoji) are not split.
	const discordNickMax = 32
	candidateRunes := []rune(candidate)
	if len(candidateRunes) <= discordNickMax {
		return candidate
	}
	// Truncate base runes to fit: leave room for " " + suffix runes.
	suffixRunes := []rune(" " + suffix)
	available := discordNickMax - len(suffixRunes)
	if available <= 0 {
		sr := []rune(suffix)
		if len(sr) > discordNickMax {
			sr = sr[:discordNickMax]
		}
		return string(sr)
	}
	baseRunes := []rune(base)
	if len(baseRunes) > available {
		baseRunes = baseRunes[:available]
	}
	return string(baseRunes) + " " + suffix
}
