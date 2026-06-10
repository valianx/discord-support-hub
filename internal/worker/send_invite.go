// send_invite.go implements the KindSendInvite worker handler (AC-M6-5, AC-M6-6).
//
// The handler:
//  1. Decodes SendInvitePayload.
//  2. Loads the merchant and verifies invite_link is set (fail-closed: skip retry on nil).
//  3. Loads the user and verifies email is set.
//  4. Sends the email via the SMTP sender (email package).
//  5. Stamps space_members.invite_sent_at via StampSpaceMemberInviteSent.
//  6. Writes an audit entry.
//
// The notify queue is isolated from provision/membership queues (AC-M6-6).
// Email address is redacted in log messages (personal data, NFR-6).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/email"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// sendInviteConfig carries dependencies for the send_invite handler.
type sendInviteConfig struct {
	store  store.Store
	sender *email.Sender // nil = config missing; handler logs a warning and skips send
}

type sendInviteHandler struct {
	cfg sendInviteConfig
}

func newSendInviteHandler(cfg sendInviteConfig) asynq.HandlerFunc {
	if cfg.store == nil {
		return stubHandler(queue.KindSendInvite)
	}
	h := &sendInviteHandler{cfg: cfg}
	return h.handle
}

func (h *sendInviteHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.SendInvitePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: send_invite: decode payload: %v", asynq.SkipRetry, err)
	}

	if payload.SpaceMemberID == "" || payload.SpaceID == "" || payload.UserID == "" || payload.MerchantID == "" {
		return fmt.Errorf("%w: send_invite: payload missing required fields", asynq.SkipRetry)
	}

	// Load merchant — verify invite link is stored.
	merchant, err := h.cfg.store.GetMerchantByID(ctx, payload.MerchantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: send_invite: merchant %s not found", asynq.SkipRetry, payload.MerchantID)
		}
		return fmt.Errorf("send_invite: load merchant: %w", err)
	}
	if merchant.InviteLink == nil || *merchant.InviteLink == "" {
		// Operator has not set an invite link — fail permanently (no link to send).
		// This mirrors the 409 precondition check in the API handler (AC-M6-5).
		return fmt.Errorf("%w: send_invite: merchant %s has no invite_link set; cannot send",
			asynq.SkipRetry, payload.MerchantID)
	}
	inviteLink := *merchant.InviteLink

	// Load user — require email.
	user, err := h.cfg.store.GetUserByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: send_invite: user %s not found", asynq.SkipRetry, payload.UserID)
		}
		return fmt.Errorf("send_invite: load user: %w", err)
	}
	if user.Email == nil || *user.Email == "" {
		return fmt.Errorf("%w: send_invite: user %s has no email address", asynq.SkipRetry, payload.UserID)
	}
	toEmail := *user.Email

	// Warn and skip (non-fatal) if the email sender is not configured.
	if h.cfg.sender == nil {
		slog.WarnContext(ctx, "send_invite: SMTP sender not configured; invite not sent",
			"space_member_id", payload.SpaceMemberID,
			"user_id", payload.UserID,
		)
		return fmt.Errorf("%w: send_invite: SMTP sender not configured", asynq.SkipRetry)
	}

	// Build the email body — contains the invite link (not a secret).
	subject := fmt.Sprintf("You have been invited to %s's support space", merchant.Name)
	body := buildInviteEmailBody(merchant.Name, inviteLink)

	// Send email. Email address is redacted in any error log inside the sender.
	if sendErr := h.cfg.sender.Send(ctx, toEmail, subject, body); sendErr != nil {
		// Retryable: transient SMTP failures should be retried by asynq.
		slog.ErrorContext(ctx, "send_invite: SMTP send failed",
			"space_member_id", payload.SpaceMemberID,
			"err", sendErr,
		)
		return fmt.Errorf("send_invite: smtp send: %w", sendErr)
	}

	// Stamp invite_sent_at — non-fatal if this fails; the email was already delivered.
	if _, stampErr := h.cfg.store.StampSpaceMemberInviteSent(ctx, payload.SpaceMemberID); stampErr != nil {
		slog.WarnContext(ctx, "send_invite: could not stamp invite_sent_at",
			"space_member_id", payload.SpaceMemberID, "err", stampErr)
	}

	// Write audit entry. Email is intentionally omitted from detail (personal data).
	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:       "collaborator.invite_sent",
		SpaceID:      &payload.SpaceID,
		TargetUserID: &payload.UserID,
		Detail: map[string]any{
			"space_member_id": payload.SpaceMemberID,
			"merchant_id":     payload.MerchantID,
		},
	})

	slog.InfoContext(ctx, "send_invite: invite email sent",
		"space_member_id", payload.SpaceMemberID,
		"user_id", payload.UserID,
		"merchant_id", payload.MerchantID,
	)
	return nil
}

// buildInviteEmailBody produces the plain-text body for the merchant invite email.
// The invite link is included; no personal data beyond the merchant name appears.
func buildInviteEmailBody(merchantName, inviteLink string) string {
	return fmt.Sprintf(
		"Hello,\n\n"+
			"You have been invited to access %s's dedicated support space on Discord.\n\n"+
			"Use the link below to join:\n%s\n\n"+
			"This link grants you access to the support channel for %s.\n\n"+
			"If you did not expect this invitation, please disregard this email.\n",
		merchantName, inviteLink, merchantName,
	)
}
