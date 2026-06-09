// Package discord provides a thin interface over discordgo for all Discord API calls.
// The interface enables mocking in tests (NFR-8 pluggable seam).
// In M0 only the constructor is implemented; actual API calls land in M2+.
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
	// TODO(M2): replace with a lightweight REST check.
	Ping(ctx context.Context) error
}

// Session is the discordgo-backed implementation of Client.
// The session is opened once at boot from the bot token (injected from env, NFR-6).
// No API calls are made in M0; real channel/member/role operations land in M2+.
type Session struct {
	session *discordgo.Session
	guildID string
}

// New creates a Session, opens a WebSocket connection to Discord, and verifies
// the bot token is accepted. The caller must call Close when done.
// The bot token is passed in, never read from env inside this constructor (NFR-6).
func New(botToken, guildID string) (*Session, error) {
	if botToken == "" {
		return nil, fmt.Errorf("discord: bot token is required")
	}

	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return nil, fmt.Errorf("discord: create session: %w", err)
	}

	// In M0 we do not register event handlers or open the WebSocket gateway —
	// the ws connection is not needed until M2 when real events must be handled.
	// Keeping the session closed avoids unnecessary intents setup in the skeleton.
	// TODO(M2): open the session and register event handlers.

	slog.Info("discord: session created", "guild_id", guildID)
	return &Session{session: dg, guildID: guildID}, nil
}

// Close releases the underlying discordgo session.
func (s *Session) Close() error {
	return s.session.Close()
}

// Ping implements Client. In M0 it is a no-op that always succeeds.
// TODO(M2): implement a real REST check (e.g. get gateway).
func (s *Session) Ping(_ context.Context) error {
	return nil
}
