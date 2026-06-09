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
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/discord"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/ratelimit"
	"github.com/valianx/discord-support-hub/internal/reconcile"
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

	// M2b: rate limiter, locker, and cache needed by the provision handler.
	Limiter           ratelimit.Limiter
	Locker            lock.Locker
	Cache             cache.Cache
	EveryoneRoleID    string // Discord @everyone role id (equals guildID in Discord)
	DefaultCategoryID string // optional default Discord category for spaces without category_id

	// M3: OAuth2 token store for the invite_collaborator handler (guilds.join).
	TokenStore *oauth.TokenStore

	// M3: reconcile engine for post-mutation targeted sweeps.
	ReconcileEngine *reconcile.Engine

	// M4: optional nickname suffix for agent marking (FR-24). Empty = disabled.
	AgentNicknameSuffix string

	// M5: Prometheus metrics instance. Nil → no-op (AC-2 wire-up).
	// Pass observability.DefaultMetrics (or the instance returned by InitMetrics) to
	// activate real metric recording on the provision worker path.
	Metrics *observability.Metrics
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
	// M1: real project_agent_role handler; M4: wired with optional nickname suffix (FR-24).
	roleHandler := newProjectAgentRoleHandlerWithMarking(
		cfg.Store, cfg.DiscordClient, cfg.DiscordGuildID, cfg.AgentRoleID, cfg.AgentNicknameSuffix,
	)
	mux.HandleFunc(queue.KindProjectAgentRole, roleHandler)

	// M2b: real provision_space handler.
	// fix(NFR-5): wire AgentRoleID (not guildID) so the category allow targets the Agent role,
	// and defaultCategory so spaces without category_id still receive the Agent allow.
	provisionHandler := newProvisionSpaceHandler(provisionSpaceConfig{
		store:           cfg.Store,
		discord:         cfg.DiscordClient,
		limiter:         cfg.Limiter,
		locker:          cfg.Locker,
		cache:           cfg.Cache,
		metrics:         cfg.Metrics, // fix(AC-2): wire metrics so /metrics reflects real outcomes
		guildID:         cfg.DiscordGuildID,
		everyoneRoleID:  cfg.EveryoneRoleID,
		agentRoleID:     cfg.AgentRoleID,
		defaultCategory: cfg.DefaultCategoryID,
	})
	mux.HandleFunc(queue.KindProvisionSpace, provisionHandler)

	// M3: real invite and expel handlers.
	inviteHandler := newInviteCollaboratorHandler(inviteCollaboratorConfig{
		store:      cfg.Store,
		discord:    cfg.DiscordClient,
		locker:     cfg.Locker,
		tokenStore: cfg.TokenStore,
		guildID:    cfg.DiscordGuildID,
	})
	mux.HandleFunc(queue.KindInviteCollaborator, inviteHandler)

	expelHandler := newExpelCollaboratorHandler(expelCollaboratorConfig{
		store:   cfg.Store,
		discord: cfg.DiscordClient,
		locker:  cfg.Locker,
		guildID: cfg.DiscordGuildID,
	})
	mux.HandleFunc(queue.KindExpelCollaborator, expelHandler)

	// M5: real reconcile_space handler.
	// The reconcile engine is constructed with a locker in cmd/worker/main.go (SEC-M5-002);
	// if ReconcileEngine is nil the stub is used.
	mux.HandleFunc(queue.KindReconcileSpace, newReconcileSpaceHandler(cfg.ReconcileEngine))

	// M4: real lifecycle handler.
	lifecycleHandler := newChangeLifecycleHandler(lifecycleConfig{
		store:          cfg.Store,
		discord:        cfg.DiscordClient,
		guildID:        cfg.DiscordGuildID,
		everyoneRoleID: cfg.EveryoneRoleID,
	})
	mux.HandleFunc(queue.KindChangeLifecycle, lifecycleHandler)

	// M4: real sync_welcome handler.
	welcomeHandler := newSyncWelcomeHandler(syncWelcomeConfig{
		store:   cfg.Store,
		discord: cfg.DiscordClient,
	})
	mux.HandleFunc(queue.KindSyncWelcome, welcomeHandler)

	// M4: real nickname-suffix marking handler (no-op when suffix is empty).
	nickHandler := newApplyNicknameSuffixHandler(nickmarkingConfig{
		store:   cfg.Store,
		discord: cfg.DiscordClient,
		guildID: cfg.DiscordGuildID,
		suffix:  cfg.AgentNicknameSuffix,
	})
	mux.HandleFunc(queue.KindApplyNicknameSuffix, nickHandler)

	// M5: real reconcile_guild handler — runs the full sweep across all active spaces.
	mux.HandleFunc(queue.KindReconcileGuild, newReconcileGuildHandler(cfg.ReconcileEngine, cfg.DiscordGuildID))
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
