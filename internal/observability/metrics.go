// Package observability — metrics.go registers and exposes the minimal M5 metric set.
//
// Metric set (NFR-7 v1 form, AC-2):
//   - hub_provisioning_latency_seconds   histogram — end-to-end provisioning job duration
//   - hub_active_spaces_total            gauge     — number of spaces in lifecycle=active
//   - hub_ratelimit_hits_total           counter   — times a worker was denied by the token bucket
//   - hub_errors_total{kind}             counter   — worker errors by kind (fatal vs transient)
//
// The registry is intentionally separate from prometheus.DefaultRegisterer so the router
// can serve a scoped /metrics handler without exposing process/Go runtime metrics that
// leak implementation details. Call RegisterMetrics once at startup.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// Metrics holds the registered Prometheus metrics.
// The zero value is unusable — call NewMetrics() or use the package-level DefaultMetrics.
type Metrics struct {
	ProvisioningLatency *prometheus.HistogramVec
	ActiveSpaces        prometheus.Gauge
	RateLimitHits       prometheus.Counter
	Errors              *prometheus.CounterVec
	registry            *prometheus.Registry
}

// DefaultMetrics is the package-level instance registered at startup.
var DefaultMetrics *Metrics

// NewMetrics creates a Metrics instance backed by a new, isolated registry.
// Calling this function registers the metrics — do not call more than once per process
// unless you need isolated registries (e.g. in tests).
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	provisioningLatency := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "hub_provisioning_latency_seconds",
			Help:    "End-to-end latency of space provisioning jobs in seconds.",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"status"}, // "success" | "failure"
	)
	reg.MustRegister(provisioningLatency)

	activeSpaces := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hub_active_spaces_total",
		Help: "Current number of spaces in lifecycle state=active.",
	})
	reg.MustRegister(activeSpaces)

	rateLimitHits := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hub_ratelimit_hits_total",
		Help: "Total number of times a worker was denied by the distributed token bucket.",
	})
	reg.MustRegister(rateLimitHits)

	errors := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hub_errors_total",
			Help: "Total number of worker errors by kind.",
		},
		[]string{"kind"}, // "fatal" | "transient"
	)
	reg.MustRegister(errors)

	return &Metrics{
		ProvisioningLatency: provisioningLatency,
		ActiveSpaces:        activeSpaces,
		RateLimitHits:       rateLimitHits,
		Errors:              errors,
		registry:            reg,
	}
}

// Handler returns an http.Handler that serves the /metrics endpoint for this Metrics instance.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// InitMetrics initialises DefaultMetrics and returns it.
// Safe to call once at startup. Subsequent calls replace DefaultMetrics.
func InitMetrics() *Metrics {
	DefaultMetrics = NewMetrics()
	return DefaultMetrics
}

// RecordProvisioningLatency records a provisioning job's duration and outcome.
// Call at the end of a provision_space worker run.
func RecordProvisioningLatency(m *Metrics, seconds float64, success bool) {
	if m == nil {
		return
	}
	status := "success"
	if !success {
		status = "failure"
	}
	m.ProvisioningLatency.WithLabelValues(status).Observe(seconds)
}

// IncRateLimitHit increments the rate-limit hit counter.
// Call whenever the distributed token bucket denies a worker request.
func IncRateLimitHit(m *Metrics) {
	if m == nil {
		return
	}
	m.RateLimitHits.Inc()
}

// IncError increments the error counter for the given kind ("fatal" or "transient").
func IncError(m *Metrics, kind string) {
	if m == nil {
		return
	}
	m.Errors.WithLabelValues(kind).Inc()
}

// SetActiveSpaces sets the active-spaces gauge to n.
func SetActiveSpaces(m *Metrics, n float64) {
	if m == nil {
		return
	}
	m.ActiveSpaces.Set(n)
}
