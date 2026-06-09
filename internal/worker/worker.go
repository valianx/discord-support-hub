// Package worker registers asynq task handlers for all job kinds.
// In M0 all handlers are stubs that log receipt and return nil.
// Real implementations land in M2 (provisioning), M3 (membership), M4 (lifecycle/marking).
package worker

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/queue"
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
}

// New creates an asynq.Server with the four-queue topology and stub handlers registered.
// Queue priorities match docs/02-architecture.md §3.4.
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
			// IsFailure: rate-limit retries are NOT failures — implemented in M2.
			// TODO(M2): customise IsFailure and RetryDelayFunc for rate-limit handling.
		},
	)

	mux := NewServeMux()

	return &Server{server: srv, mux: mux}
}

// NewServeMux builds a ServeMux with all stub handlers registered.
// Exposed so tests can reuse the exact same handler wiring without duplication.
func NewServeMux() *asynq.ServeMux {
	mux := asynq.NewServeMux()
	registerHandlers(mux)
	return mux
}

// Start begins processing tasks. It blocks until the context is cancelled.
// The asynq.Server handles graceful shutdown internally on Shutdown().
func (s *Server) Start() error {
	return s.server.Run(s.mux)
}

// Shutdown initiates a graceful shutdown, waiting for in-flight tasks to complete.
func (s *Server) Shutdown() {
	s.server.Shutdown()
}

// registerHandlers binds every task kind to its stub handler.
func registerHandlers(mux *asynq.ServeMux) {
	mux.HandleFunc(queue.KindProvisionSpace, stubHandler(queue.KindProvisionSpace))
	mux.HandleFunc(queue.KindInviteCollaborator, stubHandler(queue.KindInviteCollaborator))
	mux.HandleFunc(queue.KindExpelCollaborator, stubHandler(queue.KindExpelCollaborator))
	mux.HandleFunc(queue.KindProjectAgentRole, stubHandler(queue.KindProjectAgentRole))
	mux.HandleFunc(queue.KindChangeLifecycle, stubHandler(queue.KindChangeLifecycle))
	mux.HandleFunc(queue.KindReconcileGuild, stubHandler(queue.KindReconcileGuild))
	mux.HandleFunc(queue.KindReconcileSpace, stubHandler(queue.KindReconcileSpace))
	mux.HandleFunc(queue.KindSyncWelcome, stubHandler(queue.KindSyncWelcome))
	mux.HandleFunc(queue.KindApplyNicknameSuffix, stubHandler(queue.KindApplyNicknameSuffix))
}

// stubHandler returns an asynq.HandlerFunc that logs receipt and returns nil.
// Every stub is replaced by a real implementation in later milestones.
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
