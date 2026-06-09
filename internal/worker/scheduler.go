// Package worker — scheduler.go wires the asynq.Scheduler for periodic tasks (M5, AC-5).
//
// The scheduler enqueues a reconcile:guild task on the low-priority reconcile queue at
// the configured cron interval (default every 5 minutes). This causes the reconcile worker
// to run a full-guild sweep, detecting and repairing drift between Postgres (desired) and
// Discord (real). Postgres always wins (NFR-5).
package worker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/robfig/cron/v3"
	"github.com/valianx/discord-support-hub/internal/queue"
)

// minSweepInterval is the floor below which RECONCILE_SWEEP_CRON is rejected at start
// (SEC-M5-003). A cron that fires more frequently than once per minute would spin the
// full-guild sweep aggressively and amplify Discord rate-limit exposure.
const minSweepInterval = 1 * time.Minute

// Scheduler wraps the asynq.Scheduler.
type Scheduler struct {
	inner *asynq.Scheduler
}

// NewScheduler creates and starts the periodic reconcile sweep scheduler.
//
// cronExpr is a standard 5-field cron expression (minute, hour, dom, month, dow).
// guildID is passed as the payload so the reconcile_guild handler knows which guild to sweep.
// redisAddr/redisPassword/redisDB point to the same Valkey instance as the worker server.
//
// Returns an error when:
//   - cronExpr is malformed.
//   - The effective interval between two consecutive fires is below minSweepInterval
//     (SEC-M5-003: guards against a misconfig that would spin the sweep aggressively).
func NewScheduler(redisAddr, redisPassword string, redisDB int, cronExpr, guildID string) (*Scheduler, error) {
	// fix(SEC-M5-003): validate the cron interval floor before registering the task.
	// Parse with robfig/cron (standard 5-field; already a transitive dep via asynq).
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, parseErr := parser.Parse(cronExpr)
	if parseErr != nil {
		return nil, fmt.Errorf("scheduler: invalid cron expression %q: %w", cronExpr, parseErr)
	}
	// Calculate the interval between the next two consecutive fires.
	t1 := schedule.Next(time.Now())
	t2 := schedule.Next(t1)
	interval := t2.Sub(t1)
	if interval < minSweepInterval {
		return nil, fmt.Errorf("scheduler: RECONCILE_SWEEP_CRON %q fires every %s which is below the minimum allowed interval of %s (SEC-M5-003); "+
			"set a cron that fires no more than once per minute to avoid aggressive Discord rate-limit exposure",
			cronExpr, interval, minSweepInterval)
	}

	s := asynq.NewScheduler(
		asynq.RedisClientOpt{
			Addr:     redisAddr,
			Password: redisPassword,
			DB:       redisDB,
		},
		nil, // use default SchedulerOpts
	)

	payload, err := buildReconcileGuildPayload(guildID)
	if err != nil {
		return nil, fmt.Errorf("scheduler: build payload: %w", err)
	}

	task := asynq.NewTask(queue.KindReconcileGuild, payload)
	entryID, err := s.Register(cronExpr, task,
		asynq.Queue(queue.QueueReconcile),
		asynq.MaxRetry(2),
	)
	if err != nil {
		return nil, fmt.Errorf("scheduler: register reconcile:guild @ %q: %w", cronExpr, err)
	}

	slog.Info("scheduler: registered full-guild reconcile sweep",
		"cron", cronExpr,
		"guild_id", guildID,
		"entry_id", entryID)

	return &Scheduler{inner: s}, nil
}

// Start begins executing scheduled tasks. It blocks until Stop is called.
func (s *Scheduler) Start() error {
	return s.inner.Start()
}

// Stop shuts down the scheduler gracefully.
func (s *Scheduler) Stop() {
	s.inner.Shutdown()
}

// buildReconcileGuildPayload serialises the ReconcileGuildPayload to JSON.
func buildReconcileGuildPayload(guildID string) ([]byte, error) {
	return json.Marshal(reconcileGuildPayload{GuildID: guildID})
}

type reconcileGuildPayload struct {
	GuildID string `json:"guild_id"`
}
