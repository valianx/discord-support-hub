// Package discord provides a thin interface over discordgo for all Discord API calls.
// The interface enables mocking in tests (NFR-8 pluggable seam).
// M0: constructor only.
// M1: role assignment/removal methods for agent projection (GuildMemberRoleAdd/Remove).
// M2+: channel create, overwrites, member add.
package discord

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

// Client is the Discord API abstraction used by the worker and reconciler.
// All methods that make Discord API calls are defined here so the implementation
// can be swapped or mocked in tests.
type Client interface {
	// Ping verifies the bot token is valid by opening and closing a session.
	Ping(ctx context.Context) error

	// AssignAgentRole grants the Agent role (guildRoleID) to the given Discord user.
	// Used by the project_agent_role worker handler (M1, §6.1).
	AssignAgentRole(ctx context.Context, guildID, discordUserID, agentRoleID string) error

	// RemoveAgentRole revokes the Agent role from the given Discord user.
	// Used when an agent is removed from the roster (M1, §6.1).
	RemoveAgentRole(ctx context.Context, guildID, discordUserID, agentRoleID string) error
}

// Session is the discordgo-backed implementation of Client.
// The session is opened once at boot from the bot token (injected from env, NFR-6).
type Session struct {
	session *discordgo.Session
	guildID string
}

// New creates a Session from the bot token.
// The caller must call Close when done.
// The bot token is passed in, never read from env inside this constructor (NFR-6).
func New(botToken, guildID string) (*Session, error) {
	if botToken == "" {
		return nil, fmt.Errorf("discord: bot token is required")
	}

	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return nil, fmt.Errorf("discord: create session: %w", err)
	}

	// In M0/M1 we do not open the WebSocket gateway — real Discord calls use REST only.
	// TODO(M2): open the session and register event handlers when gateway events are needed.

	slog.Info("discord: session created", "guild_id", guildID)
	return &Session{session: dg, guildID: guildID}, nil
}

// Close releases the underlying discordgo session.
func (s *Session) Close() error {
	return s.session.Close()
}

// Ping implements Client. In M0/M1 it is a no-op that always succeeds.
// TODO(M2): implement a real REST check (e.g. get gateway or /users/@me).
func (s *Session) Ping(_ context.Context) error {
	return nil
}

// AssignAgentRole grants the Agent role to a Discord user via GuildMemberRoleAdd.
// MANAGE_ROLES is reserved to the bot (NFR-13).
// discordgo's built-in per-route rate limiter handles retries within this process;
// the distributed token bucket (M2) will gate cross-process budget.
func (s *Session) AssignAgentRole(_ context.Context, guildID, discordUserID, agentRoleID string) error {
	if err := s.session.GuildMemberRoleAdd(guildID, discordUserID, agentRoleID); err != nil {
		return fmt.Errorf("discord: assign agent role to user %s: %w", discordUserID, err)
	}
	return nil
}

// RemoveAgentRole revokes the Agent role from a Discord user via GuildMemberRoleRemove.
// MANAGE_ROLES is reserved to the bot (NFR-13).
func (s *Session) RemoveAgentRole(_ context.Context, guildID, discordUserID, agentRoleID string) error {
	if err := s.session.GuildMemberRoleRemove(guildID, discordUserID, agentRoleID); err != nil {
		return fmt.Errorf("discord: remove agent role from user %s: %w", discordUserID, err)
	}
	return nil
}
