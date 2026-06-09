// Package worker registers asynq task handlers for all job kinds.
// M0: stub handlers only.
// M1: project_agent_role handler is real (assigns/removes Agent role via Discord).
// M2: rate-limit retry config (RetryDelayFunc, IsFailure, SkipRetry) wired.
// M2b+: provision, membership handlers.
package worker

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/discord"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// Server wraps the asynq.Server and its ServeMux.
type Server struct {
	server *asynq.Server
	mux    *asynq.ServeMux
}

// Config holds the parameters needed to build the asynq.Server.
type Config struct {
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	Concurrency   int

	// Runtime dependencies for real handlers (M1+).
	Store          store.Store
	DiscordClient  discord.Client
	DiscordGuildID string
	AgentRoleID    string
}

// provisionMaxRetry is the MaxRetry for the provision queue. Rate-limit retries are
// expected flow (IsFailure returns false for them), so the budget is generous (AC-8).
const provisionMaxRetry = 10

// New creates an asynq.Server with the four-queue topology and handlers registered.
// Queue priorities match docs/02-architecture.md §3.4.
// RetryDelayFunc and IsFailure are wired for rate-limit handling (AC-5, AC-8, §3.2).
func New(cfg Config) *Server {
	srv := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		},
		asynq.Config{
			Concurrency: cfg.Concurrency,
			Queues: map[string]int{
				queue.QueueProvision:  3, // high
				queue.QueueMembership: 3, // high
				queue.QueueReconcile:  1, // low
				queue.QueueMarking:    1, // low
			},
			// RetryDelayFunc returns Retry-After for rate-limit errors; exponential
			// backoff for all other transient errors (AC-5, §3.2).
			RetryDelayFunc: RetryDelayFunc,
			// IsFailure returns false for rate-limit retries so error-rate metrics
			// stay honest — rate limiting is expected flow, not a failure (AC-8, NFR-7).
			IsFailure: IsFailure,
		},
	)

	mux := newServeMux(cfg)

	return &Server{server: srv, mux: mux}
}

// NewServeMux builds a ServeMux with handlers registered.
// Accepts Config so real handlers receive their dependencies.
// Exposed so tests can reuse the exact same handler wiring without duplication.
func NewServeMux(cfg Config) *asynq.ServeMux {
	return newServeMux(cfg)
}

func newServeMux(cfg Config) *asynq.ServeMux {
	mux := asynq.NewServeMux()
	registerHandlers(mux, cfg)
	return mux
}

// Start begins processing tasks. It blocks until the context is cancelled.
func (s *Server) Start() error {
	return s.server.Run(s.mux)
}

// Shutdown initiates a graceful shutdown.
func (s *Server) Shutdown() {
	s.server.Shutdown()
}

// registerHandlers binds every task kind to its handler.
func registerHandlers(mux *asynq.ServeMux, cfg Config) {
	// M1: real project_agent_role handler.
	roleHandler := newProjectAgentRoleHandler(cfg.Store, cfg.DiscordClient, cfg.DiscordGuildID, cfg.AgentRoleID)
	mux.HandleFunc(queue.KindProjectAgentRole, roleHandler)

	// Remaining kinds are stubs pending M2/M3/M4.
	mux.HandleFunc(queue.KindProvisionSpace, stubHandler(queue.KindProvisionSpace))
	mux.HandleFunc(queue.KindInviteCollaborator, stubHandler(queue.KindInviteCollaborator))
	mux.HandleFunc(queue.KindExpelCollaborator, stubHandler(queue.KindExpelCollaborator))
	mux.HandleFunc(queue.KindChangeLifecycle, stubHandler(queue.KindChangeLifecycle))
	mux.HandleFunc(queue.KindReconcileGuild, stubHandler(queue.KindReconcileGuild))
	mux.HandleFunc(queue.KindReconcileSpace, stubHandler(queue.KindReconcileSpace))
	mux.HandleFunc(queue.KindSyncWelcome, stubHandler(queue.KindSyncWelcome))
	mux.HandleFunc(queue.KindApplyNicknameSuffix, stubHandler(queue.KindApplyNicknameSuffix))
}

// stubHandler returns an asynq.HandlerFunc that logs receipt and returns nil.
func stubHandler(kind string) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(task.Payload(), &payload)
		slog.InfoContext(ctx, "worker: stub handler received task",
			"kind", kind,
			"payload_keys", payloadKeys(payload),
		)
		// TODO(M2/M3/M4): replace with real implementation.
		return nil
	}
}

func payloadKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
