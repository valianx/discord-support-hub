// sync_welcome_test.go — hermetic tests for the KindSyncWelcome worker handler (M4, AC-4).
//
// Tests cover:
//   - First sync: SendMessage + PinMessage called; welcome_message_id persisted.
//   - Re-sync (welcome_message_id already set): EditMessage called instead of SendMessage.
//   - Re-sync after edit failure: falls back to SendMessage + PinMessage.
//   - Space without discord_channel_id: SkipRetry returned.
//   - Bad JSON payload: SkipRetry returned.
//   - Audit entry is written on success.
package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// ─── Fake discord client for welcome sync ────────────────────────────────────

// welcomeTrackingDiscord extends fakeDiscordClient with call tracking for the
// welcome-sync-specific methods (SetChannelTopic, SendMessage, EditMessage, PinMessage).
type welcomeTrackingDiscord struct {
	fakeDiscordClient
	topicCalls []string // channelIDs passed to SetChannelTopic

	sentMessages []sentMsgCall // SendMessage calls
	editedMsgs   []editMsgCall // EditMessage calls
	pinnedMsgs   []pinnedMsgCall

	sendMsgID  string // returned message id from SendMessage
	sendMsgErr error
	editMsgErr error
}

type sentMsgCall struct {
	ChannelID string
	Content   string
}

type editMsgCall struct {
	ChannelID string
	MessageID string
	Content   string
}

type pinnedMsgCall struct {
	ChannelID string
	MessageID string
}

func (f *welcomeTrackingDiscord) SetChannelTopic(_ context.Context, channelID, _ string) error {
	f.topicCalls = append(f.topicCalls, channelID)
	return nil
}

func (f *welcomeTrackingDiscord) SendMessage(_ context.Context, channelID, content string) (string, error) {
	f.sentMessages = append(f.sentMessages, sentMsgCall{ChannelID: channelID, Content: content})
	if f.sendMsgErr != nil {
		return "", f.sendMsgErr
	}
	id := f.sendMsgID
	if id == "" {
		id = "msg-new-001"
	}
	return id, nil
}

func (f *welcomeTrackingDiscord) EditMessage(_ context.Context, channelID, messageID, content string) error {
	f.editedMsgs = append(f.editedMsgs, editMsgCall{ChannelID: channelID, MessageID: messageID, Content: content})
	return f.editMsgErr
}

func (f *welcomeTrackingDiscord) PinMessage(_ context.Context, channelID, messageID string) error {
	f.pinnedMsgs = append(f.pinnedMsgs, pinnedMsgCall{ChannelID: channelID, MessageID: messageID})
	return nil
}

// ─── Fake store for welcome sync tests ───────────────────────────────────────

// welcomeFakeStore builds on workerFakeStore with welcome-sync-specific overrides.
type welcomeFakeStore struct {
	workerFakeStore
	spaces           map[string]*domain.Space
	welcomeIDUpdates []welcomeIDUpdate
	auditEntries     []store.InsertAuditEntryParams
	jobs             map[string]*domain.Job
}

type welcomeIDUpdate struct {
	spaceID   string
	messageID string
}

func newWelcomeFakeStore() *welcomeFakeStore {
	return &welcomeFakeStore{
		workerFakeStore: workerFakeStore{users: make(map[string]*domain.User)},
		spaces:          make(map[string]*domain.Space),
		jobs:            make(map[string]*domain.Job),
	}
}

func (f *welcomeFakeStore) GetSpaceByID(_ context.Context, id string) (*domain.Space, error) {
	sp, ok := f.spaces[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sp, nil
}

func (f *welcomeFakeStore) UpdateSpaceWelcomeMessageID(
	_ context.Context,
	spaceID, messageID string,
) (*domain.Space, error) {
	f.welcomeIDUpdates = append(f.welcomeIDUpdates, welcomeIDUpdate{spaceID: spaceID, messageID: messageID})
	sp, ok := f.spaces[spaceID]
	if !ok {
		return nil, store.ErrNotFound
	}
	sp.WelcomeMessageID = &messageID
	return sp, nil
}

func (f *welcomeFakeStore) InsertAuditEntry(_ context.Context, p store.InsertAuditEntryParams) error {
	f.auditEntries = append(f.auditEntries, p)
	return nil
}

func (f *welcomeFakeStore) GetJobBySpaceIDAndKind(
	_ context.Context,
	_ string,
	kind string,
) (*domain.Job, error) {
	for _, j := range f.jobs {
		if j.Kind == kind {
			return j, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *welcomeFakeStore) UpdateJobStatus(
	_ context.Context,
	p store.UpdateJobStatusParams,
) (*domain.Job, error) {
	j, ok := f.jobs[p.JobID]
	if !ok {
		return nil, store.ErrNotFound
	}
	j.Status = p.Status
	return j, nil
}

func (f *welcomeFakeStore) GetJobByID(_ context.Context, id string) (*domain.Job, error) {
	j, ok := f.jobs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return j, nil
}

func (f *welcomeFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	return nil, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeSyncWelcomeTask(spaceID, message string) *asynq.Task {
	payload, _ := json.Marshal(queue.SyncWelcomePayload{SpaceID: spaceID, Message: message})
	return asynq.NewTask(queue.KindSyncWelcome, payload)
}

func spaceWithChannelAndNoPin(id, channelID string) *domain.Space {
	return &domain.Space{
		ID:               id,
		MerchantID:       "m-001",
		LifecycleState:   domain.SpaceLifecycleActive,
		DiscordChannelID: &channelID,
		CreatedAt:        time.Now(),
	}
}

func spaceWithChannelAndPin(id, channelID, pinMsgID string) *domain.Space {
	sp := spaceWithChannelAndNoPin(id, channelID)
	sp.WelcomeMessageID = &pinMsgID
	return sp
}

func runWelcomeTask(s *welcomeFakeStore, d *welcomeTrackingDiscord, task *asynq.Task) error {
	mux := worker.NewServeMux(worker.Config{
		Store:          s,
		DiscordClient:  d,
		DiscordGuildID: "guild-001",
	})
	return mux.ProcessTask(context.Background(), task)
}

// ─── AC-4: first sync ─────────────────────────────────────────────────────────

// TestSyncWelcome_Worker_FirstSync_SendsAndPins verifies that the first sync
// sends a new message, pins it, and persists the welcome_message_id (AC-4).
func TestSyncWelcome_Worker_FirstSync_SendsAndPins(t *testing.T) {
	s := newWelcomeFakeStore()
	s.spaces["sp-01"] = spaceWithChannelAndNoPin("sp-01", "ch-001")
	d := &welcomeTrackingDiscord{sendMsgID: "msg-001"}

	if err := runWelcomeTask(s, d, makeSyncWelcomeTask("sp-01", "")); err != nil {
		t.Fatalf("first sync must succeed, got: %v", err)
	}

	if len(d.topicCalls) != 1 {
		t.Fatalf("SetChannelTopic must be called once, called %d times", len(d.topicCalls))
	}
	if len(d.sentMessages) != 1 {
		t.Fatalf("SendMessage must be called once, called %d times", len(d.sentMessages))
	}
	if len(d.pinnedMsgs) != 1 {
		t.Fatalf("PinMessage must be called once, called %d times", len(d.pinnedMsgs))
	}
	if d.pinnedMsgs[0].MessageID != "msg-001" {
		t.Errorf("PinMessage must use the sent message id 'msg-001', got %q", d.pinnedMsgs[0].MessageID)
	}
	if len(d.editedMsgs) != 0 {
		t.Error("EditMessage must not be called on first sync")
	}

	if len(s.welcomeIDUpdates) == 0 {
		t.Fatal("UpdateSpaceWelcomeMessageID must be called to persist message id")
	}
	if s.welcomeIDUpdates[0].messageID != "msg-001" {
		t.Errorf("persisted welcome_message_id must be 'msg-001', got %q", s.welcomeIDUpdates[0].messageID)
	}
}

// TestSyncWelcome_Worker_ReSync_EditsExistingPin verifies that a re-sync edits the
// existing pinned message instead of sending a new one (AC-4 idempotent — no duplicate).
func TestSyncWelcome_Worker_ReSync_EditsExistingPin(t *testing.T) {
	s := newWelcomeFakeStore()
	s.spaces["sp-02"] = spaceWithChannelAndPin("sp-02", "ch-002", "msg-existing")
	d := &welcomeTrackingDiscord{}

	if err := runWelcomeTask(s, d, makeSyncWelcomeTask("sp-02", "Updated message")); err != nil {
		t.Fatalf("re-sync must succeed, got: %v", err)
	}

	if len(d.editedMsgs) != 1 {
		t.Fatalf("EditMessage must be called once on re-sync, called %d times", len(d.editedMsgs))
	}
	if d.editedMsgs[0].MessageID != "msg-existing" {
		t.Errorf("EditMessage must target the existing pin 'msg-existing', got %q", d.editedMsgs[0].MessageID)
	}
	if len(d.sentMessages) != 0 {
		t.Error("SendMessage must NOT be called on re-sync when edit succeeds")
	}
}

// TestSyncWelcome_Worker_ReSync_EditFails_SendsNewPin verifies that when EditMessage
// fails (e.g., message was deleted), the handler falls back to sending a new message
// and pinning it (AC-4 idempotent fallback).
func TestSyncWelcome_Worker_ReSync_EditFails_SendsNewPin(t *testing.T) {
	s := newWelcomeFakeStore()
	s.spaces["sp-03"] = spaceWithChannelAndPin("sp-03", "ch-003", "msg-deleted")
	d := &welcomeTrackingDiscord{
		editMsgErr: errors.New("message not found"),
		sendMsgID:  "msg-new",
	}

	if err := runWelcomeTask(s, d, makeSyncWelcomeTask("sp-03", "")); err != nil {
		t.Fatalf("re-sync fallback must succeed, got: %v", err)
	}

	if len(d.sentMessages) != 1 {
		t.Fatalf("SendMessage must be called as fallback when edit fails, called %d times", len(d.sentMessages))
	}
	if len(d.pinnedMsgs) != 1 {
		t.Fatalf("PinMessage must be called for the new message, called %d times", len(d.pinnedMsgs))
	}
	if d.pinnedMsgs[0].MessageID != "msg-new" {
		t.Errorf("PinMessage must use new message id 'msg-new', got %q", d.pinnedMsgs[0].MessageID)
	}
}

// TestSyncWelcome_Worker_NoChannelID_SkipRetry verifies that when a space has no
// discord_channel_id, the handler skips with SkipRetry (cannot sync without a channel).
func TestSyncWelcome_Worker_NoChannelID_SkipRetry(t *testing.T) {
	s := newWelcomeFakeStore()
	s.spaces["sp-04"] = &domain.Space{
		ID:             "sp-04",
		MerchantID:     "m-001",
		LifecycleState: domain.SpaceLifecycleActive,
		CreatedAt:      time.Now(),
		// DiscordChannelID intentionally nil.
	}
	d := &welcomeTrackingDiscord{}

	err := runWelcomeTask(s, d, makeSyncWelcomeTask("sp-04", ""))
	if err == nil {
		t.Fatal("sync without channel_id must return an error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("error must be wrapped in asynq.SkipRetry, got: %v", err)
	}
}

// TestSyncWelcome_Worker_BadPayload_SkipRetry verifies that a malformed JSON payload
// is rejected with SkipRetry (undecodable payload → no retry).
func TestSyncWelcome_Worker_BadPayload_SkipRetry(t *testing.T) {
	s := newWelcomeFakeStore()
	d := &welcomeTrackingDiscord{}

	task := asynq.NewTask(queue.KindSyncWelcome, []byte("{invalid"))
	err := runWelcomeTask(s, d, task)
	if err == nil {
		t.Fatal("bad payload must return an error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("bad payload must be wrapped in asynq.SkipRetry, got: %v", err)
	}
}

// TestSyncWelcome_Worker_AuditEntry verifies that a successful sync writes an
// audit entry recording the channel and message id (AC-2 audit trail).
func TestSyncWelcome_Worker_AuditEntry(t *testing.T) {
	s := newWelcomeFakeStore()
	s.spaces["sp-05"] = spaceWithChannelAndNoPin("sp-05", "ch-005")
	d := &welcomeTrackingDiscord{sendMsgID: "msg-005"}

	if err := runWelcomeTask(s, d, makeSyncWelcomeTask("sp-05", "")); err != nil {
		t.Fatalf("sync must succeed, got: %v", err)
	}

	if len(s.auditEntries) == 0 {
		t.Fatal("audit entry must be written after successful sync")
	}
	if s.auditEntries[0].Action != "space.welcome.sync" {
		t.Errorf("audit action must be 'space.welcome.sync', got %q", s.auditEntries[0].Action)
	}
}

// ─── SEC-M4-001: AllowedMentions enforcement at the send path ────────────────

// TestSyncWelcome_Worker_SendPath_ContentReachedDiscord verifies that the worker's
// sync_welcome handler calls SendMessage with the configured message content and
// that SendMessage is called exactly once (first sync path, SEC-M4-001 send path).
// AllowedMentions suppression is applied in the discord.Session implementation;
// this test confirms the content flows through correctly so the mention-suppression
// wrapper in discord.Session receives the real content.
func TestSyncWelcome_Worker_SendPath_ContentReachedDiscord(t *testing.T) {
	s := newWelcomeFakeStore()
	s.spaces["sp-sec-01"] = spaceWithChannelAndNoPin("sp-sec-01", "ch-sec-01")
	d := &welcomeTrackingDiscord{sendMsgID: "msg-sec-01"}

	const customMsg = "@everyone please see this" // mentions must be suppressed by discord.Session
	if err := runWelcomeTask(s, d, makeSyncWelcomeTask("sp-sec-01", customMsg)); err != nil {
		t.Fatalf("sync must succeed, got: %v", err)
	}

	if len(d.sentMessages) != 1 {
		t.Fatalf("SendMessage must be called once, called %d times", len(d.sentMessages))
	}
	// The content reaches the discord client unchanged; AllowedMentions suppression is
	// enforced inside discord.Session.SendMessage (ChannelMessageSendComplex with empty
	// Parse list). The handler must not mangle the content before passing it.
	if d.sentMessages[0].Content != customMsg {
		t.Errorf("SendMessage must receive the configured content %q, got %q",
			customMsg, d.sentMessages[0].Content)
	}
}

// TestSyncWelcome_Worker_EditPath_ContentReachedDiscord verifies that the re-sync
// (edit) path passes content to EditMessage unchanged (SEC-M4-001 edit path).
func TestSyncWelcome_Worker_EditPath_ContentReachedDiscord(t *testing.T) {
	s := newWelcomeFakeStore()
	s.spaces["sp-sec-02"] = spaceWithChannelAndPin("sp-sec-02", "ch-sec-02", "msg-existing-sec")
	d := &welcomeTrackingDiscord{}

	const updatedMsg = "@here check the new message"
	if err := runWelcomeTask(s, d, makeSyncWelcomeTask("sp-sec-02", updatedMsg)); err != nil {
		t.Fatalf("re-sync must succeed, got: %v", err)
	}

	if len(d.editedMsgs) != 1 {
		t.Fatalf("EditMessage must be called once on re-sync, called %d times", len(d.editedMsgs))
	}
	if d.editedMsgs[0].Content != updatedMsg {
		t.Errorf("EditMessage must receive the configured content %q, got %q",
			updatedMsg, d.editedMsgs[0].Content)
	}
	if d.editedMsgs[0].MessageID != "msg-existing-sec" {
		t.Errorf("EditMessage must target the existing pin, got %q", d.editedMsgs[0].MessageID)
	}
}
