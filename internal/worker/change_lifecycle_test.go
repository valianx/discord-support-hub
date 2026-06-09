// change_lifecycle_test.go — hermetic tests for the KindChangeLifecycle worker handler (M4).
//
// Tests cover:
//   - AC-1: archive action → ArchiveChannel called, lifecycle state persisted as 'archived'.
//   - AC-1: reopen action → UnarchiveChannel called, lifecycle state persisted as 'active'.
//   - AC-1: resolve action → no Discord call, lifecycle state persisted as 'resolved'.
//   - AC-1: space without discord_channel_id → Discord step skipped, Postgres updated.
//   - AC-2: audit entry is written after each lifecycle transition.
//   - AC-6: unknown action → SkipRetry returned.
//   - AC-6: bad JSON payload → SkipRetry returned.
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

// ─── Fake discord client with lifecycle tracking ──────────────────────────────

// lifecycleTrackingDiscord extends fakeDiscordClient with call tracking for the
// archive/unarchive methods added in M4.
type lifecycleTrackingDiscord struct {
	fakeDiscordClient
	archiveCalls   []string // channelIDs passed to ArchiveChannel
	unarchiveCalls []string // channelIDs passed to UnarchiveChannel
	archiveErr     error
	unarchiveErr   error
}

func (f *lifecycleTrackingDiscord) ArchiveChannel(_ context.Context, channelID, _ string) error {
	f.archiveCalls = append(f.archiveCalls, channelID)
	return f.archiveErr
}

func (f *lifecycleTrackingDiscord) UnarchiveChannel(_ context.Context, channelID, _ string) error {
	f.unarchiveCalls = append(f.unarchiveCalls, channelID)
	return f.unarchiveErr
}

// Override M4 methods inherited from fakeDiscordClient with no-ops (already there).
// SetChannelTopic, PinMessage, EditMessage, SendMessage, SetNickname stay as no-ops.

// ─── Fake store for lifecycle tests ──────────────────────────────────────────

// lifecycleFakeStore builds on workerFakeStore with lifecycle-specific overrides.
type lifecycleFakeStore struct {
	workerFakeStore
	spaces           map[string]*domain.Space
	lifecycleUpdates []store.UpdateSpaceLifecycleParams
	auditEntries     []store.InsertAuditEntryParams
	jobs             map[string]*domain.Job
	jobUpdates       []store.UpdateJobStatusParams
}

func newLifecycleFakeStore() *lifecycleFakeStore {
	return &lifecycleFakeStore{
		workerFakeStore: workerFakeStore{users: make(map[string]*domain.User)},
		spaces:          make(map[string]*domain.Space),
		jobs:            make(map[string]*domain.Job),
	}
}

func (f *lifecycleFakeStore) GetSpaceByID(_ context.Context, id string) (*domain.Space, error) {
	sp, ok := f.spaces[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sp, nil
}

func (f *lifecycleFakeStore) UpdateSpaceLifecycle(
	_ context.Context,
	p store.UpdateSpaceLifecycleParams,
) (*domain.Space, error) {
	f.lifecycleUpdates = append(f.lifecycleUpdates, p)
	sp, ok := f.spaces[p.SpaceID]
	if !ok {
		return nil, store.ErrNotFound
	}
	sp.LifecycleState = p.LifecycleState
	return sp, nil
}

func (f *lifecycleFakeStore) InsertAuditEntry(_ context.Context, p store.InsertAuditEntryParams) error {
	f.auditEntries = append(f.auditEntries, p)
	return nil
}

func (f *lifecycleFakeStore) CreateJob(_ context.Context, p store.CreateJobParams) (*domain.Job, error) {
	j := &domain.Job{
		ID:     "job-" + p.TaskID,
		TaskID: p.TaskID,
		Kind:   p.Kind,
		Status: domain.JobStatusPending,
	}
	f.jobs[j.ID] = j
	return j, nil
}

func (f *lifecycleFakeStore) UpdateJobStatus(
	_ context.Context,
	p store.UpdateJobStatusParams,
) (*domain.Job, error) {
	f.jobUpdates = append(f.jobUpdates, p)
	j, ok := f.jobs[p.JobID]
	if !ok {
		return nil, store.ErrNotFound
	}
	j.Status = p.Status
	return j, nil
}

func (f *lifecycleFakeStore) GetJobByID(_ context.Context, id string) (*domain.Job, error) {
	j, ok := f.jobs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return j, nil
}

func (f *lifecycleFakeStore) GetJobBySpaceIDAndKind(
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

func (f *lifecycleFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	return nil, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeLifecycleTask(spaceID, action string) *asynq.Task {
	payload, _ := json.Marshal(queue.ChangeLifecyclePayload{SpaceID: spaceID, Action: action})
	return asynq.NewTask(queue.KindChangeLifecycle, payload)
}

func spaceWithChannel(id, channelID string) *domain.Space {
	return &domain.Space{
		ID:               id,
		MerchantID:       "m-001",
		LifecycleState:   domain.SpaceLifecycleActive,
		DiscordChannelID: &channelID,
		CreatedAt:        time.Now(),
	}
}

func spaceWithoutChannel(id string) *domain.Space {
	return &domain.Space{
		ID:             id,
		MerchantID:     "m-001",
		LifecycleState: domain.SpaceLifecycleActive,
		CreatedAt:      time.Now(),
	}
}

func runLifecycleTask(s *lifecycleFakeStore, d *lifecycleTrackingDiscord, task *asynq.Task) error {
	mux := worker.NewServeMux(worker.Config{
		Store:          s,
		DiscordClient:  d,
		DiscordGuildID: "guild-001",
		EveryoneRoleID: "everyone-001",
	})
	return mux.ProcessTask(context.Background(), task)
}

// ─── AC-1: archive ────────────────────────────────────────────────────────────

// TestChangeLifecycle_Worker_Archive_CallsDiscordAndPersists verifies that the archive
// action calls ArchiveChannel and persists lifecycle_state='archived' (AC-1).
func TestChangeLifecycle_Worker_Archive_CallsDiscordAndPersists(t *testing.T) {
	s := newLifecycleFakeStore()
	s.spaces["sp-01"] = spaceWithChannel("sp-01", "ch-101")
	d := &lifecycleTrackingDiscord{}

	if err := runLifecycleTask(s, d, makeLifecycleTask("sp-01", "archive")); err != nil {
		t.Fatalf("archive task must succeed, got: %v", err)
	}

	if len(d.archiveCalls) != 1 {
		t.Fatalf("ArchiveChannel must be called once, called %d times", len(d.archiveCalls))
	}
	if d.archiveCalls[0] != "ch-101" {
		t.Errorf("ArchiveChannel called with wrong channelID: %q", d.archiveCalls[0])
	}
	if len(s.lifecycleUpdates) == 0 {
		t.Fatal("UpdateSpaceLifecycle must be called")
	}
	if s.lifecycleUpdates[0].LifecycleState != domain.SpaceLifecycleArchived {
		t.Errorf("persisted state must be 'archived', got %q", s.lifecycleUpdates[0].LifecycleState)
	}
}

// TestChangeLifecycle_Worker_Reopen_CallsUnarchive verifies that the reopen action
// calls UnarchiveChannel and persists lifecycle_state='active' (AC-1).
func TestChangeLifecycle_Worker_Reopen_CallsUnarchive(t *testing.T) {
	s := newLifecycleFakeStore()
	sp := spaceWithChannel("sp-02", "ch-102")
	sp.LifecycleState = domain.SpaceLifecycleArchived
	now := time.Now()
	sp.ArchivedAt = &now
	s.spaces["sp-02"] = sp
	d := &lifecycleTrackingDiscord{}

	if err := runLifecycleTask(s, d, makeLifecycleTask("sp-02", "reopen")); err != nil {
		t.Fatalf("reopen task must succeed, got: %v", err)
	}

	if len(d.unarchiveCalls) != 1 {
		t.Fatalf("UnarchiveChannel must be called once, called %d times", len(d.unarchiveCalls))
	}
	if len(s.lifecycleUpdates) == 0 {
		t.Fatal("UpdateSpaceLifecycle must be called")
	}
	if s.lifecycleUpdates[0].LifecycleState != domain.SpaceLifecycleActive {
		t.Errorf("persisted state after reopen must be 'active', got %q", s.lifecycleUpdates[0].LifecycleState)
	}
}

// TestChangeLifecycle_Worker_Resolve_NoDiscordCall verifies that the resolve action
// does NOT call any Discord visibility method (AC-1 — resolve = Postgres-only change).
func TestChangeLifecycle_Worker_Resolve_NoDiscordCall(t *testing.T) {
	s := newLifecycleFakeStore()
	s.spaces["sp-03"] = spaceWithChannel("sp-03", "ch-103")
	d := &lifecycleTrackingDiscord{}

	if err := runLifecycleTask(s, d, makeLifecycleTask("sp-03", "resolve")); err != nil {
		t.Fatalf("resolve task must succeed, got: %v", err)
	}

	if len(d.archiveCalls) != 0 || len(d.unarchiveCalls) != 0 {
		t.Error("resolve must not call ArchiveChannel or UnarchiveChannel")
	}
	if len(s.lifecycleUpdates) == 0 {
		t.Fatal("UpdateSpaceLifecycle must be called")
	}
	if s.lifecycleUpdates[0].LifecycleState != domain.SpaceLifecycleResolved {
		t.Errorf("persisted state after resolve must be 'resolved', got %q", s.lifecycleUpdates[0].LifecycleState)
	}
}

// TestChangeLifecycle_Worker_NoChannelID_SkipsDiscordCall verifies that when a space
// has no discord_channel_id yet, the Discord step is skipped but Postgres is updated (AC-1).
func TestChangeLifecycle_Worker_NoChannelID_SkipsDiscordCall(t *testing.T) {
	s := newLifecycleFakeStore()
	s.spaces["sp-04"] = spaceWithoutChannel("sp-04")
	d := &lifecycleTrackingDiscord{}

	if err := runLifecycleTask(s, d, makeLifecycleTask("sp-04", "archive")); err != nil {
		t.Fatalf("archive without channel_id must still succeed, got: %v", err)
	}

	if len(d.archiveCalls) != 0 {
		t.Error("no Discord call must happen when discord_channel_id is nil")
	}
	if len(s.lifecycleUpdates) == 0 {
		t.Fatal("UpdateSpaceLifecycle must still be called even without a channel_id")
	}
}

// TestChangeLifecycle_Worker_AuditEntry verifies that a lifecycle action writes an
// audit entry recording the action and new state (AC-2 audit trail).
func TestChangeLifecycle_Worker_AuditEntry(t *testing.T) {
	s := newLifecycleFakeStore()
	s.spaces["sp-05"] = spaceWithChannel("sp-05", "ch-105")
	d := &lifecycleTrackingDiscord{}

	if err := runLifecycleTask(s, d, makeLifecycleTask("sp-05", "archive")); err != nil {
		t.Fatalf("archive must succeed, got: %v", err)
	}

	if len(s.auditEntries) == 0 {
		t.Fatal("audit entry must be written after lifecycle transition")
	}
	if s.auditEntries[0].Action != "space.lifecycle.archive" {
		t.Errorf("audit action must be 'space.lifecycle.archive', got %q", s.auditEntries[0].Action)
	}
}

// ─── AC-6: illegal action ─────────────────────────────────────────────────────

// TestChangeLifecycle_Worker_UnknownAction_SkipRetry verifies that an unknown action
// in the payload results in SkipRetry (AC-6: bad payload → no retry).
func TestChangeLifecycle_Worker_UnknownAction_SkipRetry(t *testing.T) {
	s := newLifecycleFakeStore()
	s.spaces["sp-06"] = spaceWithChannel("sp-06", "ch-106")
	d := &lifecycleTrackingDiscord{}

	err := runLifecycleTask(s, d, makeLifecycleTask("sp-06", "delete_everything"))
	if err == nil {
		t.Fatal("unknown action must return an error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("unknown action must be wrapped in asynq.SkipRetry, got: %v", err)
	}
}

// TestChangeLifecycle_Worker_BadPayload_SkipRetry verifies that a malformed JSON
// payload is rejected with SkipRetry (AC-6: undecodable payload → no retry).
func TestChangeLifecycle_Worker_BadPayload_SkipRetry(t *testing.T) {
	s := newLifecycleFakeStore()
	d := &lifecycleTrackingDiscord{}

	task := asynq.NewTask(queue.KindChangeLifecycle, []byte("{bad json"))
	err := runLifecycleTask(s, d, task)
	if err == nil {
		t.Fatal("bad payload must return an error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("bad payload must be wrapped in asynq.SkipRetry, got: %v", err)
	}
}

// ─── Job-mirror: pending → active → completed ────────────────────────────────

// TestChangeLifecycle_Worker_JobMirror_AdvancesToCompleted verifies that a completed
// lifecycle job advances the jobs mirror row through pending → active → completed so
// GET /jobs/{id} reflects the final status (M4 job-mirror fix, AC-6 async path).
func TestChangeLifecycle_Worker_JobMirror_AdvancesToCompleted(t *testing.T) {
	s := newLifecycleFakeStore()
	s.spaces["sp-jm-01"] = spaceWithChannel("sp-jm-01", "ch-jm-01")

	// Pre-seed a job row that the handler should advance.
	job, _ := s.CreateJob(context.Background(), store.CreateJobParams{
		TaskID: "task-jm-01",
		Kind:   queue.KindChangeLifecycle,
	})
	// The handler looks up the job by (spaceID, kind) — store it using the
	// internal map so GetJobBySpaceIDAndKind can find it.
	s.jobs[job.ID] = job

	d := &lifecycleTrackingDiscord{}
	if err := runLifecycleTask(s, d, makeLifecycleTask("sp-jm-01", "archive")); err != nil {
		t.Fatalf("archive must succeed, got: %v", err)
	}

	// The handler must have emitted at least two status updates:
	//   1. active  (before the Discord call)
	//   2. completed (on success)
	if len(s.jobUpdates) < 2 {
		t.Fatalf("expected at least 2 job status updates (active + completed), got %d", len(s.jobUpdates))
	}

	// The last update must set the status to 'completed'.
	last := s.jobUpdates[len(s.jobUpdates)-1]
	if last.Status != domain.JobStatusCompleted {
		t.Errorf("last job status update must be 'completed', got %q", last.Status)
	}
	if !last.Completed {
		t.Error("last job status update must set Completed=true")
	}

	// The in-memory job row must now reflect completed status.
	final := s.jobs[job.ID]
	if final.Status != domain.JobStatusCompleted {
		t.Errorf("job row in store must be 'completed', got %q", final.Status)
	}
}
