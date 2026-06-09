// apply_nickname_suffix_test.go — hermetic tests for M4 AC-5 nickname-suffix marking.
//
// Tests cover:
//   - Marking disabled (empty suffix): no SetNickname call; handler returns nil.
//   - Marking enabled: SetNickname called with displayName + suffix.
//   - buildNickname idempotency: suffix already present → not doubled.
//   - buildNickname truncation: candidate over 32 chars → truncated cleanly.
//   - User without discord_user_id: SetNickname not called (deferred).
//   - Bad JSON payload: SkipRetry returned.
//   - projectAgentRoleHandler with marking: SetNickname called after AssignAgentRole.
//   - projectAgentRoleHandler without marking: SetNickname not called.
package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// ─── Fakes for nickname suffix tests ─────────────────────────────────────────

// nickFakeDiscord extends fakeDiscordClient with call tracking for SetNickname.
type nickFakeDiscord struct {
	fakeDiscordClient
	nicknameCalls []setNicknameCall
	nicknameErr   error
}

type setNicknameCall struct {
	GuildID       string
	DiscordUserID string
	Nickname      string
}

func (f *nickFakeDiscord) SetNickname(_ context.Context, guildID, discordUserID, nickname string) error {
	f.nicknameCalls = append(f.nicknameCalls, setNicknameCall{
		GuildID:       guildID,
		DiscordUserID: discordUserID,
		Nickname:      nickname,
	})
	return f.nicknameErr
}

// nickFakeStore extends workerFakeStore for marking tests.
type nickFakeStore struct {
	workerFakeStore
	auditEntries []store.InsertAuditEntryParams
}

func newNickFakeStore() *nickFakeStore {
	return &nickFakeStore{
		workerFakeStore: workerFakeStore{users: make(map[string]*domain.User)},
	}
}

func (f *nickFakeStore) InsertAuditEntry(_ context.Context, p store.InsertAuditEntryParams) error {
	f.auditEntries = append(f.auditEntries, p)
	return nil
}

func (f *nickFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	return nil, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeApplyNickSuffixTask(userID, guildID, suffix string) *asynq.Task {
	payload, _ := json.Marshal(queue.ApplyNicknameSuffixPayload{
		UserID:  userID,
		GuildID: guildID,
		Suffix:  suffix,
	})
	return asynq.NewTask(queue.KindApplyNicknameSuffix, payload)
}

func agentUserWithDiscordID(id, displayName, discordUserID string) *domain.User {
	now := time.Now()
	return &domain.User{
		ID:            id,
		Type:          domain.UserTypeAgent,
		DisplayName:   &displayName,
		DiscordUserID: &discordUserID,
		ProvisionedAt: &now,
	}
}

func agentUserWithoutDiscordID(id string) *domain.User {
	return &domain.User{
		ID:   id,
		Type: domain.UserTypeAgent,
	}
}

func runNickHandler(s *nickFakeStore, d *nickFakeDiscord, suffix string, task *asynq.Task) error {
	mux := worker.NewServeMux(worker.Config{
		Store:               s,
		DiscordClient:       d,
		DiscordGuildID:      "guild-001",
		AgentNicknameSuffix: suffix,
	})
	return mux.ProcessTask(context.Background(), task)
}

// ─── AC-5: marking disabled (default) ────────────────────────────────────────

// TestNicknameSuffix_MarkingDisabled_NoSetNickname verifies that when the suffix is
// empty (default), no SetNickname call is made (AC-5: off by default).
func TestNicknameSuffix_MarkingDisabled_NoSetNickname(t *testing.T) {
	s := newNickFakeStore()
	s.users["u-01"] = agentUserWithDiscordID("u-01", "Alice", "discord-u-01")
	d := &nickFakeDiscord{}

	task := makeApplyNickSuffixTask("u-01", "guild-001", "")
	err := runNickHandler(s, d, "", task) // empty suffix = marking disabled
	if err != nil {
		t.Fatalf("disabled marking handler must return nil, got: %v", err)
	}
	if len(d.nicknameCalls) != 0 {
		t.Errorf("SetNickname must not be called when marking is disabled, called %d times", len(d.nicknameCalls))
	}
}

// TestNicknameSuffix_MarkingEnabled_CallsSetNickname verifies that when marking is
// configured, SetNickname is called with the correct nickname (AC-5).
func TestNicknameSuffix_MarkingEnabled_CallsSetNickname(t *testing.T) {
	s := newNickFakeStore()
	s.users["u-02"] = agentUserWithDiscordID("u-02", "Bob", "discord-u-02")
	d := &nickFakeDiscord{}

	task := makeApplyNickSuffixTask("u-02", "guild-001", "")
	err := runNickHandler(s, d, "[Support]", task)
	if err != nil {
		t.Fatalf("enabled marking handler must return nil, got: %v", err)
	}
	if len(d.nicknameCalls) != 1 {
		t.Fatalf("SetNickname must be called once, called %d times", len(d.nicknameCalls))
	}
	if d.nicknameCalls[0].Nickname != "Bob [Support]" {
		t.Errorf("nickname must be 'Bob [Support]', got %q", d.nicknameCalls[0].Nickname)
	}
	if d.nicknameCalls[0].DiscordUserID != "discord-u-02" {
		t.Errorf("SetNickname must target correct discord user id, got %q", d.nicknameCalls[0].DiscordUserID)
	}
}

// TestNicknameSuffix_NoDiscordUserID_Skipped verifies that when the agent has no
// discord_user_id yet, SetNickname is not called (deferred until connected).
func TestNicknameSuffix_NoDiscordUserID_Skipped(t *testing.T) {
	s := newNickFakeStore()
	s.users["u-03"] = agentUserWithoutDiscordID("u-03")
	d := &nickFakeDiscord{}

	task := makeApplyNickSuffixTask("u-03", "guild-001", "")
	err := runNickHandler(s, d, "[Support]", task)
	if err != nil {
		t.Fatalf("missing discord_user_id must return nil (deferred), got: %v", err)
	}
	if len(d.nicknameCalls) != 0 {
		t.Errorf("SetNickname must not be called when discord_user_id is nil")
	}
}

// TestNicknameSuffix_BadPayload_SkipRetry verifies that a malformed payload is
// rejected with SkipRetry.
func TestNicknameSuffix_BadPayload_SkipRetry(t *testing.T) {
	s := newNickFakeStore()
	d := &nickFakeDiscord{}

	task := asynq.NewTask(queue.KindApplyNicknameSuffix, []byte("{bad"))
	err := runNickHandler(s, d, "[Support]", task)
	if err == nil {
		t.Fatal("bad payload must return an error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("bad payload must be wrapped in asynq.SkipRetry, got: %v", err)
	}
}

// ─── buildNickname tests (pure function — no network or store) ────────────────

// TestBuildNickname_AppendsSuffix verifies that the helper appends suffix with a space.
func TestBuildNickname_AppendsSuffix(t *testing.T) {
	s := newNickFakeStore()
	s.users["u-04"] = agentUserWithDiscordID("u-04", "Carol", "discord-u-04")
	d := &nickFakeDiscord{}

	task := makeApplyNickSuffixTask("u-04", "", "")
	if err := runNickHandler(s, d, "(Agent)", task); err != nil {
		t.Fatalf("handler must succeed, got: %v", err)
	}
	if len(d.nicknameCalls) == 0 {
		t.Fatal("SetNickname must be called")
	}
	if d.nicknameCalls[0].Nickname != "Carol (Agent)" {
		t.Errorf("expected 'Carol (Agent)', got %q", d.nicknameCalls[0].Nickname)
	}
}

// TestBuildNickname_Idempotent_SuffixAlreadyPresent verifies that the suffix is
// not doubled when the displayName already ends with it (idempotent re-application).
func TestBuildNickname_Idempotent_SuffixAlreadyPresent(t *testing.T) {
	s := newNickFakeStore()
	suffix := "[Support]"
	displayName := "Dave [Support]"
	s.users["u-05"] = agentUserWithDiscordID("u-05", displayName, "discord-u-05")
	d := &nickFakeDiscord{}

	task := makeApplyNickSuffixTask("u-05", "", "")
	if err := runNickHandler(s, d, suffix, task); err != nil {
		t.Fatalf("handler must succeed, got: %v", err)
	}
	if len(d.nicknameCalls) == 0 {
		t.Fatal("SetNickname must be called")
	}
	if d.nicknameCalls[0].Nickname != "Dave [Support]" {
		t.Errorf("suffix must not be doubled: expected 'Dave [Support]', got %q", d.nicknameCalls[0].Nickname)
	}
	if strings.Count(d.nicknameCalls[0].Nickname, "[Support]") != 1 {
		t.Errorf("suffix must appear exactly once, got %q", d.nicknameCalls[0].Nickname)
	}
}

// TestBuildNickname_Truncation verifies that a nickname over 32 chars is truncated
// to fit Discord's limit (32 chars max).
func TestBuildNickname_Truncation(t *testing.T) {
	s := newNickFakeStore()
	// 30-char display name + " " + "[Support]" = 40 chars — over the 32-char Discord limit.
	longName := "VeryLongDisplayNameWithManyChars" // 32 chars exactly
	s.users["u-06"] = agentUserWithDiscordID("u-06", longName, "discord-u-06")
	d := &nickFakeDiscord{}

	suffix := "[Sup]"
	task := makeApplyNickSuffixTask("u-06", "", "")
	if err := runNickHandler(s, d, suffix, task); err != nil {
		t.Fatalf("handler must succeed, got: %v", err)
	}
	if len(d.nicknameCalls) == 0 {
		t.Fatal("SetNickname must be called")
	}
	nick := d.nicknameCalls[0].Nickname
	if len(nick) > 32 {
		t.Errorf("nickname must be ≤ 32 chars (Discord limit), got %d chars: %q", len(nick), nick)
	}
	if !strings.HasSuffix(nick, suffix) {
		t.Errorf("truncated nickname must still end with suffix %q, got %q", suffix, nick)
	}
}

// ─── SEC-M4-003: rune-based truncation for multibyte display names ────────────

// TestBuildNickname_Truncation_MultibyteSafe verifies that truncation operates on
// runes (code points), not bytes, so multibyte UTF-8 characters are never split
// and Discord's 32-character (code-point) limit is respected (SEC-M4-003).
//
// "日本語名前" is 5 runes but 15 bytes per UTF-8. A name of 10 such characters
// (10 runes = 30 bytes) + " " + "[Sup]" = 10 + 1 + 5 = 16 runes — under the limit.
// A name of 30 such characters (30 runes = 90 bytes) + " " + "[Sup]" would need
// truncation to 26 base runes to leave room for " [Sup]" (6 runes).
func TestBuildNickname_Truncation_MultibyteSafe(t *testing.T) {
	// 30-rune CJK display name — each rune is 3 bytes in UTF-8, 90 bytes total.
	longCJKName := strings.Repeat("名", 30) // 30 runes, 90 bytes
	s := newNickFakeStore()
	s.users["u-mb-01"] = agentUserWithDiscordID("u-mb-01", longCJKName, "discord-mb-01")
	d := &nickFakeDiscord{}

	suffix := "[Sup]"
	task := makeApplyNickSuffixTask("u-mb-01", "", "")
	if err := runNickHandler(s, d, suffix, task); err != nil {
		t.Fatalf("handler must succeed with multibyte name, got: %v", err)
	}
	if len(d.nicknameCalls) == 0 {
		t.Fatal("SetNickname must be called")
	}
	nick := d.nicknameCalls[0].Nickname
	runeCount := len([]rune(nick))
	if runeCount > 32 {
		t.Errorf("nickname must be ≤ 32 runes (Discord character limit), got %d runes (%d bytes): %q",
			runeCount, len(nick), nick)
	}
	if !strings.HasSuffix(nick, suffix) {
		t.Errorf("truncated multibyte nickname must still end with suffix %q, got %q", suffix, nick)
	}
	// Verify the result is valid UTF-8 (no split multibyte sequences).
	if !isValidUTF8(nick) {
		t.Errorf("truncated nickname must be valid UTF-8, got invalid sequence: %q", nick)
	}
}

// TestBuildNickname_ShortMultibyte_NotTruncated verifies that a short multibyte name
// (total rune count ≤ 32) is not truncated even though its byte length exceeds 32.
func TestBuildNickname_ShortMultibyte_NotTruncated(t *testing.T) {
	// 5-rune CJK name + " " + "[Sup]" = 11 runes — well under 32.
	cjkName := "日本語の名" // 5 runes, 15 bytes
	s := newNickFakeStore()
	s.users["u-mb-02"] = agentUserWithDiscordID("u-mb-02", cjkName, "discord-mb-02")
	d := &nickFakeDiscord{}

	suffix := "[Sup]"
	task := makeApplyNickSuffixTask("u-mb-02", "", "")
	if err := runNickHandler(s, d, suffix, task); err != nil {
		t.Fatalf("handler must succeed, got: %v", err)
	}
	if len(d.nicknameCalls) == 0 {
		t.Fatal("SetNickname must be called")
	}
	// Full name + space + suffix — should not be truncated.
	expected := cjkName + " " + suffix
	if d.nicknameCalls[0].Nickname != expected {
		t.Errorf("short multibyte name must not be truncated: want %q, got %q",
			expected, d.nicknameCalls[0].Nickname)
	}
}

// isValidUTF8 reports whether s is valid UTF-8.
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' && s != "�" {
			return false
		}
	}
	// Use the unicode/utf8 package check.
	return strings.ToValidUTF8(s, "") == s
}

// ─── AC-5: projectAgentRoleHandler with marking ───────────────────────────────

// TestProjectAgentRole_WithMarking_CallsSetNickname verifies that when marking is
// enabled (suffix configured), the project_agent_role handler calls SetNickname
// after AssignAgentRole (AC-5 integration via role handler).
func TestProjectAgentRole_WithMarking_CallsSetNickname(t *testing.T) {
	s := newWorkerFakeStore()
	discordUserID := "discord-agent-001"
	displayName := "Eve"
	s.users["agent-001"] = &domain.User{
		ID:            "agent-001",
		Type:          domain.UserTypeAgent,
		DisplayName:   &displayName,
		DiscordUserID: &discordUserID,
	}

	d := &nickFakeDiscord{}
	mux := worker.NewServeMux(worker.Config{
		Store:               s,
		DiscordClient:       d,
		DiscordGuildID:      "guild-001",
		AgentRoleID:         "role-agent",
		AgentNicknameSuffix: "[Support]",
	})

	payload, _ := json.Marshal(queue.ProjectAgentRolePayload{UserID: "agent-001", Add: true})
	task := asynq.NewTask(queue.KindProjectAgentRole, payload)
	if err := mux.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("role projection must succeed, got: %v", err)
	}

	if d.assignCalls[0] != discordUserID {
		t.Errorf("AssignAgentRole must target the correct discord user id")
	}
	if len(d.nicknameCalls) != 1 {
		t.Fatalf("SetNickname must be called once when marking is enabled, called %d times",
			len(d.nicknameCalls))
	}
	if d.nicknameCalls[0].Nickname != "Eve [Support]" {
		t.Errorf("nickname must be 'Eve [Support]', got %q", d.nicknameCalls[0].Nickname)
	}
}

// TestProjectAgentRole_WithoutMarking_NoSetNickname verifies that when marking is
// disabled (empty suffix), SetNickname is NOT called (AC-5: off by default).
func TestProjectAgentRole_WithoutMarking_NoSetNickname(t *testing.T) {
	s := newWorkerFakeStore()
	discordUserID := "discord-agent-002"
	displayName := "Frank"
	s.users["agent-002"] = &domain.User{
		ID:            "agent-002",
		Type:          domain.UserTypeAgent,
		DisplayName:   &displayName,
		DiscordUserID: &discordUserID,
	}

	d := &nickFakeDiscord{}
	mux := worker.NewServeMux(worker.Config{
		Store:               s,
		DiscordClient:       d,
		DiscordGuildID:      "guild-001",
		AgentRoleID:         "role-agent",
		AgentNicknameSuffix: "", // marking disabled
	})

	payload, _ := json.Marshal(queue.ProjectAgentRolePayload{UserID: "agent-002", Add: true})
	task := asynq.NewTask(queue.KindProjectAgentRole, payload)
	if err := mux.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("role projection must succeed, got: %v", err)
	}

	if len(d.nicknameCalls) != 0 {
		t.Errorf("SetNickname must NOT be called when marking is disabled, called %d times",
			len(d.nicknameCalls))
	}
}
