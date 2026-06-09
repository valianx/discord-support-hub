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

	// CreateChannelDenied creates a Discord text channel with an @everyone deny-VIEW_CHANNEL
	// overwrite in the initial PermissionOverwrites so the channel is invisible from the
	// instant it exists (fail-closed, NFR-4, §4.4).
	// Returns the created channel's Discord ID.
	// The everyoneRoleID is the guild's @everyone role (same as guildID in Discord).
	CreateChannelDenied(ctx context.Context, guildID, name, categoryID, everyoneRoleID string) (channelID string, err error)

	// ApplyCategoryAgentAllow sets a role-level VIEW_CHANNEL allow on a category for the
	// Agent role. This is the category-level grant that lets all agents see all spaces
	// with a single overwrite (§6.1). Called after CreateChannelDenied succeeds (§4.4).
	ApplyCategoryAgentAllow(ctx context.Context, categoryID, agentRoleID string) error

	// SetChannelPermissionDeny removes any permission overwrite for targetID on channelID
	// by calling ChannelPermissionSet with allow=0 deny=VIEW_CHANNEL.
	// Used by the fail-closed path to re-assert the @everyone deny after a partial failure.
	SetChannelPermissionDeny(ctx context.Context, channelID, targetID string, targetType discordgo.PermissionOverwriteType) error

	// SetCollaboratorOverwrite applies a per-user permission overwrite on channelID for
	// discordUserID, granting VIEW_CHANNEL + SEND_MESSAGES (PermissionOverwriteTypeMember).
	// This is the ONLY access grant for a collaborator — no role, no category access.
	// Isolation invariant: each collaborator's access is bounded to exactly the spaces
	// they hold an overwrite on (NFR-5, §6.2).
	SetCollaboratorOverwrite(ctx context.Context, channelID, discordUserID string) error

	// DeleteCollaboratorOverwrite removes the per-user permission overwrite for discordUserID
	// on channelID (PermissionOverwriteTypeMember). Used by both the channel-scope expulsion
	// path and the reconciler when revoking an unbacked overwrite (§6.3, §4.2).
	DeleteCollaboratorOverwrite(ctx context.Context, channelID, discordUserID string) error

	// AddGuildMember adds a user to the guild using their guilds.join OAuth2 access token
	// (GuildMemberAdd). The accessToken is the per-user guilds.join token stored encrypted
	// in oauth_tokens (§6.2, §7). No roles are applied at join; collaborator access is
	// granted via SetCollaboratorOverwrite separately.
	// Returns nil (no error) when the user is already a guild member.
	AddGuildMember(ctx context.Context, guildID, discordUserID, accessToken string) error

	// RemoveGuildMember removes a user from the guild entirely (GuildMemberRemove).
	// Used for server-scope expulsion (§6.3).
	RemoveGuildMember(ctx context.Context, guildID, discordUserID string) error

	// GetChannelOverwrites returns the list of permission overwrites on channelID.
	// Used by the reconciler to detect unbacked overwrites (§4.2).
	GetChannelOverwrites(ctx context.Context, channelID string) ([]*discordgo.PermissionOverwrite, error)
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

// CreateChannelDenied creates a text channel with an @everyone deny-VIEW_CHANNEL overwrite
// embedded in the initial PermissionOverwrites field of GuildChannelCreateComplex.
//
// This is the fail-closed invariant (NFR-4, §4.4): the channel is born invisible —
// there is NO window between channel creation and the @everyone deny being applied.
// The deny is part of the create payload, not a subsequent API call.
//
// everyoneRoleID is the guild's @everyone role, which always equals the guildID
// in Discord's permission model.
func (s *Session) CreateChannelDenied(
	_ context.Context,
	guildID, name, categoryID, everyoneRoleID string,
) (string, error) {
	data := discordgo.GuildChannelCreateData{
		Name:     name,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: categoryID,
		// The @everyone deny-VIEW_CHANNEL overwrite is included in the initial payload
		// so the channel is invisible the instant Discord creates it (NFR-4).
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:   everyoneRoleID,
				Type: discordgo.PermissionOverwriteTypeRole,
				Deny: discordgo.PermissionViewChannel,
			},
		},
	}

	ch, err := s.session.GuildChannelCreateComplex(guildID, data)
	if err != nil {
		return "", fmt.Errorf("discord: create channel %q denied: %w", name, err)
	}
	return ch.ID, nil
}

// ApplyCategoryAgentAllow sets a role-level VIEW_CHANNEL allow on the given category
// for the Agent role. With this single overwrite, every Agent can see all channels
// nested under the category — no per-channel work needed (§6.1).
func (s *Session) ApplyCategoryAgentAllow(_ context.Context, categoryID, agentRoleID string) error {
	if err := s.session.ChannelPermissionSet(
		categoryID,
		agentRoleID,
		discordgo.PermissionOverwriteTypeRole,
		discordgo.PermissionViewChannel, // allow
		0,                               // deny
	); err != nil {
		return fmt.Errorf("discord: apply category agent allow on %s: %w", categoryID, err)
	}
	return nil
}

// SetCollaboratorOverwrite applies a per-user permission overwrite granting
// VIEW_CHANNEL + SEND_MESSAGES to discordUserID on channelID (PermissionOverwriteTypeMember).
// This is the only access grant for a collaborator — no role is assigned (NFR-5, §6.2).
func (s *Session) SetCollaboratorOverwrite(_ context.Context, channelID, discordUserID string) error {
	allow := int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages)
	if err := s.session.ChannelPermissionSet(
		channelID,
		discordUserID,
		discordgo.PermissionOverwriteTypeMember,
		allow,
		0, // deny: none
	); err != nil {
		return fmt.Errorf("discord: set collaborator overwrite on channel %s for user %s: %w",
			channelID, discordUserID, err)
	}
	return nil
}

// DeleteCollaboratorOverwrite removes the per-user overwrite for discordUserID on channelID.
// Used for channel-scope expulsion and reconciler revocation of unbacked overwrites (§6.3, §4.2).
func (s *Session) DeleteCollaboratorOverwrite(_ context.Context, channelID, discordUserID string) error {
	if err := s.session.ChannelPermissionDelete(channelID, discordUserID); err != nil {
		return fmt.Errorf("discord: delete collaborator overwrite on channel %s for user %s: %w",
			channelID, discordUserID, err)
	}
	return nil
}

// AddGuildMember adds a user to the guild via OAuth2 guilds.join.
// accessToken is the per-user OAuth2 access token stored encrypted in oauth_tokens.
// Returns nil when the user is already a guild member (idempotent).
func (s *Session) AddGuildMember(_ context.Context, guildID, discordUserID, accessToken string) error {
	params := &discordgo.GuildMemberAddParams{
		AccessToken: accessToken,
	}
	if err := s.session.GuildMemberAdd(guildID, discordUserID, params); err != nil {
		// Discord returns 204 No Content when the member is already in the guild;
		// discordgo does not surface this as an error — so any error here is real.
		return fmt.Errorf("discord: add guild member %s: %w", discordUserID, err)
	}
	return nil
}

// RemoveGuildMember removes a user from the guild (server-scope expulsion, §6.3).
func (s *Session) RemoveGuildMember(_ context.Context, guildID, discordUserID string) error {
	if err := s.session.GuildMemberDelete(guildID, discordUserID); err != nil {
		return fmt.Errorf("discord: remove guild member %s: %w", discordUserID, err)
	}
	return nil
}

// GetChannelOverwrites returns the permission overwrites on a channel.
// Used by the reconciler to detect any overwrites not backed by Postgres (§4.2).
func (s *Session) GetChannelOverwrites(_ context.Context, channelID string) ([]*discordgo.PermissionOverwrite, error) {
	ch, err := s.session.Channel(channelID)
	if err != nil {
		return nil, fmt.Errorf("discord: get channel %s: %w", channelID, err)
	}
	return ch.PermissionOverwrites, nil
}

// SetChannelPermissionDeny sets a deny-VIEW_CHANNEL overwrite on channelID for targetID.
// Used to re-assert the @everyone deny after a partial failure (fail-closed repair).
func (s *Session) SetChannelPermissionDeny(
	_ context.Context,
	channelID, targetID string,
	targetType discordgo.PermissionOverwriteType,
) error {
	if err := s.session.ChannelPermissionSet(
		channelID,
		targetID,
		targetType,
		0,                               // allow
		discordgo.PermissionViewChannel, // deny
	); err != nil {
		return fmt.Errorf("discord: set channel permission deny on %s for %s: %w", channelID, targetID, err)
	}
	return nil
}
