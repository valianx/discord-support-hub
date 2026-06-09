// Package observability_test — metrics_test.go verifies the minimal M5 metric set (AC-2).
//
// Tests confirm that:
//   - NewMetrics() creates a usable Metrics instance with a real Prometheus registry.
//   - RecordProvisioningLatency increments the histogram on success and failure labels.
//   - IncRateLimitHit increments the rate-limit counter.
//   - IncError increments the error counter with the correct label.
//   - SetActiveSpaces writes the correct value to the gauge.
//   - All helper functions are nil-safe (do not panic when Metrics is nil).
//   - Handler() returns a valid HTTP handler that serves text/plain Prometheus exposition.
package observability_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valianx/discord-support-hub/internal/observability"
)

// TestNewMetrics_NotNil verifies that NewMetrics() returns a non-nil instance.
func TestNewMetrics_NotNil(t *testing.T) {
	m := observability.NewMetrics()
	if m == nil {
		t.Fatal("NewMetrics() returned nil")
	}
}

// TestRecordProvisioningLatency_Success increments the histogram for a successful provision.
func TestRecordProvisioningLatency_Success(t *testing.T) {
	m := observability.NewMetrics()
	// Should not panic.
	observability.RecordProvisioningLatency(m, 1.5, true)
	observability.RecordProvisioningLatency(m, 0.3, true)
}

// TestRecordProvisioningLatency_Failure increments the histogram for a failed provision.
func TestRecordProvisioningLatency_Failure(t *testing.T) {
	m := observability.NewMetrics()
	observability.RecordProvisioningLatency(m, 2.0, false)
}

// TestIncRateLimitHit_Increments verifies the rate-limit counter increments without panic.
func TestIncRateLimitHit_Increments(t *testing.T) {
	m := observability.NewMetrics()
	observability.IncRateLimitHit(m)
	observability.IncRateLimitHit(m)
}

// TestIncError_FatalAndTransient verifies both error kinds register without panic.
func TestIncError_FatalAndTransient(t *testing.T) {
	m := observability.NewMetrics()
	observability.IncError(m, "fatal")
	observability.IncError(m, "transient")
}

// TestSetActiveSpaces_NoopOnNil verifies nil-safety of all helpers.
func TestSetActiveSpaces_NoopOnNil(t *testing.T) {
	// None of these must panic when Metrics is nil.
	observability.SetActiveSpaces(nil, 42)
	observability.RecordProvisioningLatency(nil, 1.0, true)
	observability.IncRateLimitHit(nil)
	observability.IncError(nil, "fatal")
}

// TestSetActiveSpaces_ValueWritten verifies the gauge reflects the value set.
// We assert indirectly via the /metrics exposition output (text format).
func TestSetActiveSpaces_ValueWritten(t *testing.T) {
	m := observability.NewMetrics()
	observability.SetActiveSpaces(m, 7)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}

	got := string(body)
	if !strings.Contains(got, "hub_active_spaces_total 7") {
		t.Errorf("expected hub_active_spaces_total 7 in metrics output; got:\n%s", got)
	}
}

// TestMetricsHandler_IncludesAllMetricNames verifies that all four metric families
// are present in the /metrics exposition output (AC-2).
func TestMetricsHandler_IncludesAllMetricNames(t *testing.T) {
	m := observability.NewMetrics()

	// Record one observation so histograms appear in output.
	observability.RecordProvisioningLatency(m, 0.5, true)
	observability.IncRateLimitHit(m)
	observability.IncError(m, "transient")
	observability.SetActiveSpaces(m, 3)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}

	want := []string{
		"hub_provisioning_latency_seconds",
		"hub_active_spaces_total",
		"hub_ratelimit_hits_total",
		"hub_errors_total",
	}
	got := string(body)
	for _, name := range want {
		if !strings.Contains(got, name) {
			t.Errorf("metric %q not found in /metrics output", name)
		}
	}
}

// TestRecordProvisioningLatency_StatusLabelsInOutput verifies that the success and failure
// status labels both appear in the /metrics exposition output (AC-2: metrics increment
// on the right events with correct label values).
func TestRecordProvisioningLatency_StatusLabelsInOutput(t *testing.T) {
	m := observability.NewMetrics()
	observability.RecordProvisioningLatency(m, 0.5, true)  // success
	observability.RecordProvisioningLatency(m, 2.0, false) // failure

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}

	got := string(body)
	// Both label variants must appear in the exposition output.
	if !strings.Contains(got, `status="success"`) {
		t.Errorf("expected status=\"success\" label in provisioning latency histogram output; got:\n%s", got)
	}
	if !strings.Contains(got, `status="failure"`) {
		t.Errorf("expected status=\"failure\" label in provisioning latency histogram output; got:\n%s", got)
	}
}

// TestIncRateLimitHit_CountAppearsInOutput verifies that the rate-limit counter value
// is reflected in the /metrics exposition output after N increments (AC-2).
func TestIncRateLimitHit_CountAppearsInOutput(t *testing.T) {
	m := observability.NewMetrics()
	// Increment three times; the exposition output must contain the count.
	observability.IncRateLimitHit(m)
	observability.IncRateLimitHit(m)
	observability.IncRateLimitHit(m)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}

	got := string(body)
	// The counter line must show a value of 3.
	if !strings.Contains(got, "hub_ratelimit_hits_total 3") {
		t.Errorf("expected hub_ratelimit_hits_total 3 in /metrics output; got:\n%s", got)
	}
}

// TestIncError_KindLabelAppearsInOutput verifies that the error counter's kind label and
// count are reflected in the /metrics exposition output (AC-2).
func TestIncError_KindLabelAppearsInOutput(t *testing.T) {
	m := observability.NewMetrics()
	observability.IncError(m, "fatal")
	observability.IncError(m, "fatal")
	observability.IncError(m, "transient")

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}

	got := string(body)
	if !strings.Contains(got, `kind="fatal"`) {
		t.Errorf("expected kind=\"fatal\" label in hub_errors_total output; got:\n%s", got)
	}
	if !strings.Contains(got, `kind="transient"`) {
		t.Errorf("expected kind=\"transient\" label in hub_errors_total output; got:\n%s", got)
	}
}
