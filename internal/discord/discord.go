// Package discord provides a thin interface over discordgo for all Discord API calls.
// The interface enables mocking in tests (NFR-8 pluggable seam).
// M0: constructor only.
// M1: role assignment/removal methods for agent projection (GuildMemberRoleAdd/Remove).
// M2+: channel create, overwrites.
// M6: merchant role lifecycle (CreateMerchantRole, SetRoleChannelAllow, AssignMerchantRole,
//     RemoveMerchantRole, GetGuildMembersByRole, EnsureWelcomeChannel).
//     OAuth2 guild-join (AddGuildMember) and per-user overwrites (SetCollaboratorOverwrite)
//     are removed; access is now role-based (AC-M6-2, AC-M6-9).
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

	// CreateMerchantRole creates a new guild role named after the merchant and returns
	// its Discord role id. Idempotency is enforced by the caller: if spaces.merchant_role_id
	// is already set, CreateMerchantRole is skipped (AC-M6-1).
	CreateMerchantRole(ctx context.Context, guildID, name string) (roleID string, err error)

	// SetRoleChannelAllow grants VIEW_CHANNEL + SEND_MESSAGES to a role on a channel
	// via a role-type PermissionOverwriteTypeRole allow. Used after CreateChannelDenied
	// to open the channel to the merchant role (AC-M6-2, fail-closed invariant preserved).
	SetRoleChannelAllow(ctx context.Context, channelID, roleID string) error

	// AssignMerchantRole adds a Discord role to a guild member (GuildMemberRoleAdd).
	// Used by the reconciler to repair a collaborator who is missing the merchant role
	// but is present in Postgres space_members (AC-M6-8).
	AssignMerchantRole(ctx context.Context, guildID, discordUserID, roleID string) error

	// RemoveMerchantRole strips a Discord role from a guild member (GuildMemberRoleRemove).
	// Used by the reconciler to revoke access when a collaborator is no longer in
	// Postgres space_members (AC-M6-8).
	RemoveMerchantRole(ctx context.Context, guildID, discordUserID, roleID string) error

	// GetGuildMembersByRole returns the Discord user ids of all guild members currently
	// holding the given role. Used by the reconciler to build the real-state set (AC-M6-8).
	// Returns an empty slice (not an error) when no members hold the role.
	GetGuildMembersByRole(ctx context.Context, guildID, roleID string) ([]string, error)

	// EnsureWelcomeChannel creates or returns the #bienvenida (or configured name) channel
	// under categoryID with @everyone VIEW_CHANNEL allow. If a channel with the given name
	// already exists in the category it is returned as-is (idempotent). The message
	// parameter is posted only on creation; existing channels keep their history.
	// Returns the channel id (AC-M6-7).
	EnsureWelcomeChannel(ctx context.Context, guildID, categoryID, channelName, message string) (channelID string, err error)

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

	// DeleteCollaboratorOverwrite removes the per-user permission overwrite for discordUserID
	// on channelID (PermissionOverwriteTypeMember). Used for server-scope expulsion where the
	// collaborator has a legacy per-user overwrite, and by channel-scope expulsion (§6.3).
	DeleteCollaboratorOverwrite(ctx context.Context, channelID, discordUserID string) error

	// RemoveGuildMember removes a user from the guild entirely (GuildMemberRemove).
	// Used for server-scope expulsion (§6.3).
	RemoveGuildMember(ctx context.Context, guildID, discordUserID string) error

	// GetChannelOverwrites returns the list of permission overwrites on channelID.
	// Used by the reconciler to detect unbacked overwrites (§4.2).
	GetChannelOverwrites(ctx context.Context, channelID string) ([]*discordgo.PermissionOverwrite, error)

	// ArchiveChannel locks and hides a channel by denying VIEW_CHANNEL and SEND_MESSAGES
	// for @everyone. History is preserved; the channel becomes invisible to all non-bot
	// users (FR-7, M4 AC-1). everyoneRoleID is the guild's @everyone role (= guildID).
	ArchiveChannel(ctx context.Context, channelID, everyoneRoleID string) error

	// UnarchiveChannel restores a channel to the active state by removing the @everyone
	// deny overwrite so the per-user overwrites and category-level Agent allow take effect
	// again (FR-7, M4 AC-1 reopen).
	UnarchiveChannel(ctx context.Context, channelID, everyoneRoleID string) error

	// SetChannelTopic updates the human-readable topic string on a channel (FR-15 static).
	// An empty topic string clears the topic.
	SetChannelTopic(ctx context.Context, channelID, topic string) error

	// PinMessage pins an existing message in a channel. Idempotent — if the message is
	// already pinned Discord returns 204 without error.
	PinMessage(ctx context.Context, channelID, messageID string) error

	// EditMessage updates the content of an existing message. Used by sync_welcome to
	// edit the pinned welcome message in place rather than duplicating it (AC-4 idempotent).
	EditMessage(ctx context.Context, channelID, messageID, content string) error

	// SendMessage posts a new message to a channel and returns the message id.
	// Used by sync_welcome to create the initial pinned help-desk message (AC-4).
	SendMessage(ctx context.Context, channelID, content string) (messageID string, err error)

	// SetNickname sets the display nickname for a guild member. Passing an empty string
	// resets the nickname to the user's default Discord username (FR-24, M4 AC-5).
	SetNickname(ctx context.Context, guildID, discordUserID, nickname string) error
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

// CreateMerchantRole creates a new guild role named after the merchant (AC-M6-1).
// The role has no hoisting, no mentioning, and no colour by default — just a name.
// Idempotency: the caller checks spaces.merchant_role_id before calling; if it is
// already set, this method is never invoked a second time.
func (s *Session) CreateMerchantRole(_ context.Context, guildID, name string) (string, error) {
	role, err := s.session.GuildRoleCreate(guildID, &discordgo.RoleParams{
		Name: name,
	})
	if err != nil {
		return "", fmt.Errorf("discord: create merchant role %q in guild %s: %w", name, guildID, err)
	}
	return role.ID, nil
}

// SetRoleChannelAllow grants VIEW_CHANNEL + SEND_MESSAGES to a role on a channel
// (PermissionOverwriteTypeRole, allow). This opens the channel to merchant-role holders
// after CreateChannelDenied has made it born-denied (AC-M6-2, fail-closed preserved).
func (s *Session) SetRoleChannelAllow(_ context.Context, channelID, roleID string) error {
	allow := int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages)
	if err := s.session.ChannelPermissionSet(
		channelID,
		roleID,
		discordgo.PermissionOverwriteTypeRole,
		allow,
		0, // deny: none
	); err != nil {
		return fmt.Errorf("discord: set role channel allow on %s for role %s: %w",
			channelID, roleID, err)
	}
	return nil
}

// AssignMerchantRole adds the merchant role to a guild member (reconciler repair path, AC-M6-8).
func (s *Session) AssignMerchantRole(_ context.Context, guildID, discordUserID, roleID string) error {
	if err := s.session.GuildMemberRoleAdd(guildID, discordUserID, roleID); err != nil {
		return fmt.Errorf("discord: assign merchant role %s to user %s: %w", roleID, discordUserID, err)
	}
	return nil
}

// RemoveMerchantRole strips the merchant role from a guild member (reconciler revocation, AC-M6-8).
func (s *Session) RemoveMerchantRole(_ context.Context, guildID, discordUserID, roleID string) error {
	if err := s.session.GuildMemberRoleRemove(guildID, discordUserID, roleID); err != nil {
		return fmt.Errorf("discord: remove merchant role %s from user %s: %w", roleID, discordUserID, err)
	}
	return nil
}

// GetGuildMembersByRole returns the Discord user ids of all guild members holding roleID.
// The Discord API does not provide a role-member index endpoint; we page GuildMembers (up to
// 1 000 per call) and filter by role. For guilds under ~200 merchants this is acceptable;
// a dedicated index endpoint can be introduced if scale requires it.
// Returns an empty slice (not an error) when no members hold the role.
func (s *Session) GetGuildMembersByRole(_ context.Context, guildID, roleID string) ([]string, error) {
	const pageSize = 1000
	var out []string
	var afterID string

	for {
		members, err := s.session.GuildMembers(guildID, afterID, pageSize)
		if err != nil {
			return nil, fmt.Errorf("discord: get guild members for role %s: %w", roleID, err)
		}
		for _, m := range members {
			for _, r := range m.Roles {
				if r == roleID {
					out = append(out, m.User.ID)
					break
				}
			}
		}
		if len(members) < pageSize {
			break // last page
		}
		afterID = members[len(members)-1].User.ID
	}
	return out, nil
}

// EnsureWelcomeChannel creates or returns the welcome channel (AC-M6-7).
// If a text channel with channelName already exists under categoryID it is returned as-is.
// A new channel is created with @everyone VIEW_CHANNEL allow (visible to all guild members).
// The welcome message is posted on creation only; existing channels keep their history.
func (s *Session) EnsureWelcomeChannel(
	_ context.Context,
	guildID, categoryID, channelName, message string,
) (string, error) {
	// Check if the channel already exists in the category.
	channels, err := s.session.GuildChannels(guildID)
	if err != nil {
		return "", fmt.Errorf("discord: list channels for welcome check: %w", err)
	}
	for _, ch := range channels {
		if ch.ParentID == categoryID && ch.Name == channelName {
			return ch.ID, nil
		}
	}

	// Create the welcome channel with @everyone VIEW_CHANNEL allow so it is visible
	// to all guild members (the category may have a deny for merchants; the explicit
	// allow on this channel overrides it per Discord's channel-level precedence).
	ch, err := s.session.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:     channelName,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: categoryID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{
				ID:    guildID, // @everyone role id = guild id in Discord
				Type:  discordgo.PermissionOverwriteTypeRole,
				Allow: discordgo.PermissionViewChannel,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("discord: create welcome channel %q: %w", channelName, err)
	}

	// Post the welcome message on creation if one is provided.
	if message != "" {
		_, err = s.session.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
			Content:         message,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		})
		if err != nil {
			// Non-fatal: the channel was created; the message send failure is logged.
			slog.Warn("discord: welcome message send failed (channel created)", "channel_id", ch.ID, "err", err)
		}
	}
	return ch.ID, nil
}

// DeleteCollaboratorOverwrite removes the per-user overwrite for discordUserID on channelID.
// Used for server-scope expulsion and any remaining legacy per-user overwrites (§6.3).
func (s *Session) DeleteCollaboratorOverwrite(_ context.Context, channelID, discordUserID string) error {
	if err := s.session.ChannelPermissionDelete(channelID, discordUserID); err != nil {
		return fmt.Errorf("discord: delete collaborator overwrite on channel %s for user %s: %w",
			channelID, discordUserID, err)
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

// ArchiveChannel denies VIEW_CHANNEL and SEND_MESSAGES for @everyone so the channel
// becomes invisible and read-only. History is preserved in Discord (FR-7, M4 AC-1).
func (s *Session) ArchiveChannel(_ context.Context, channelID, everyoneRoleID string) error {
	deny := int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages)
	if err := s.session.ChannelPermissionSet(
		channelID,
		everyoneRoleID,
		discordgo.PermissionOverwriteTypeRole,
		0,    // allow: none
		deny, // deny: VIEW_CHANNEL + SEND_MESSAGES
	); err != nil {
		return fmt.Errorf("discord: archive channel %s: %w", channelID, err)
	}
	return nil
}

// UnarchiveChannel removes the @everyone deny overwrite so per-user overwrites and the
// category-level Agent allow resume normal visibility (FR-7, M4 AC-1 reopen).
func (s *Session) UnarchiveChannel(_ context.Context, channelID, everyoneRoleID string) error {
	// Removing the overwrite restores the inherited (deny) state from the creation-time
	// @everyone deny. We then re-apply the creation deny so the channel is born-denied again.
	// In practice, the channel was created with @everyone deny-VIEW_CHANNEL already in place
	// (fail-closed); removing the archive overwrite returns to that state.
	if err := s.session.ChannelPermissionDelete(channelID, everyoneRoleID); err != nil {
		return fmt.Errorf("discord: unarchive channel %s (delete @everyone overwrite): %w", channelID, err)
	}
	return nil
}

// SetChannelTopic updates the topic string on a channel (FR-15 static, M4 AC-4).
func (s *Session) SetChannelTopic(_ context.Context, channelID, topic string) error {
	_, err := s.session.ChannelEdit(channelID, &discordgo.ChannelEdit{Topic: topic})
	if err != nil {
		return fmt.Errorf("discord: set channel topic on %s: %w", channelID, err)
	}
	return nil
}

// PinMessage pins a message in a channel. Idempotent per Discord spec (M4 AC-4).
func (s *Session) PinMessage(_ context.Context, channelID, messageID string) error {
	if err := s.session.ChannelMessagePin(channelID, messageID); err != nil {
		return fmt.Errorf("discord: pin message %s on channel %s: %w", messageID, channelID, err)
	}
	return nil
}

// EditMessage updates the content of an existing message (M4 AC-4 idempotent re-sync).
// AllowedMentions is set to an empty parse list to prevent mention resolution on edit,
// consistent with the send path (SEC-M4-001).
func (s *Session) EditMessage(_ context.Context, channelID, messageID, content string) error {
	_, err := s.session.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:         channelID,
		ID:              messageID,
		Content:         &content,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	if err != nil {
		return fmt.Errorf("discord: edit message %s on channel %s: %w", messageID, channelID, err)
	}
	return nil
}

// SendMessage posts a new message to a channel and returns its id (M4 AC-4).
// AllowedMentions is set to an empty parse list so the welcome content can never
// trigger @everyone, @here, or role pings regardless of message content (SEC-M4-001).
func (s *Session) SendMessage(_ context.Context, channelID, content string) (string, error) {
	msg, err := s.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content,
		// fix(SEC-M4-001): empty Parse list suppresses all mention resolution.
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	if err != nil {
		return "", fmt.Errorf("discord: send message to channel %s: %w", channelID, err)
	}
	return msg.ID, nil
}

// SetNickname sets the display nickname for a guild member (FR-24, M4 AC-5).
// Passing an empty string resets the nickname to the user's default username.
func (s *Session) SetNickname(_ context.Context, guildID, discordUserID, nickname string) error {
	if err := s.session.GuildMemberNickname(guildID, discordUserID, nickname); err != nil {
		return fmt.Errorf("discord: set nickname for user %s in guild %s: %w", discordUserID, guildID, err)
	}
	return nil
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
