package server

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

// Metrics is the lightweight counter set the daemon exposes at /metrics
// in Prometheus text format. Kept stdlib-only (no client_golang dep) so
// the binary stays small; a real Prometheus scraper happily ingests the
// hand-rolled text exposition format.
//
// Operators wire MetricsCounter fields into the dispatcher / rotators /
// MCP so increments land in the right gauge. Lock-free atomic adds keep
// the hot path off the scrape goroutine.
type Metrics struct {
	RunsStarted   atomic.Uint64
	RunsSucceeded atomic.Uint64
	RunsFailed    atomic.Uint64
	RunsDLQ       atomic.Uint64
	RotationsRun  atomic.Uint64
	RotationsErr  atomic.Uint64
	MCPCalls      atomic.Uint64
	WebhookCalls  atomic.Uint64

	startedAt time.Time
}

// NewMetrics constructs a Metrics with startedAt = now so uptime
// renders correctly from the first scrape.
func NewMetrics() *Metrics {
	return &Metrics{startedAt: time.Now()}
}

// IncRunsStarted implements dispatcher.Counters.
func (m *Metrics) IncRunsStarted() { m.RunsStarted.Add(1) }

// IncRunsTerminal implements dispatcher.Counters; status maps to the
// right gauge (succeeded / failed / failed_dlq are the meaningful
// values dispatcher sees, suspended is in-flight not terminal).
func (m *Metrics) IncRunsTerminal(status string) {
	switch status {
	case "succeeded":
		m.RunsSucceeded.Add(1)
	case "failed_dlq":
		m.RunsDLQ.Add(1)
	default:
		m.RunsFailed.Add(1)
	}
}

// metrics handles GET /metrics in the Prometheus text exposition format
// (https://prometheus.io/docs/instrumenting/exposition_formats/).
// Returns 503 when no Metrics is wired (server constructed without it).
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.Metrics == nil {
		http.Error(w, "metrics not wired", http.StatusServiceUnavailable)
		return
	}
	m := s.Metrics
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	mem := runtime.MemStats{}
	runtime.ReadMemStats(&mem)
	uptimeSeconds := time.Since(m.startedAt).Seconds()

	write := func(name, help, kind string, value uint64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %s\n",
			name, help, name, kind, name, strconv.FormatUint(value, 10))
	}
	writeFloat := func(name, help, kind string, value float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %g\n",
			name, help, name, kind, name, value)
	}

	write("reactor_runs_started_total", "Total run dispatches.", "counter", m.RunsStarted.Load())
	write("reactor_runs_succeeded_total", "Total runs that terminated with status=succeeded.", "counter", m.RunsSucceeded.Load())
	write("reactor_runs_failed_total", "Total runs that terminated with status=failed.", "counter", m.RunsFailed.Load())
	write("reactor_runs_dlq_total", "Total runs that terminated with status=failed_dlq.", "counter", m.RunsDLQ.Load())
	write("reactor_rotations_run_total", "Total credential rotations attempted.", "counter", m.RotationsRun.Load())
	write("reactor_rotations_error_total", "Total rotation attempts that errored.", "counter", m.RotationsErr.Load())
	write("reactor_mcp_calls_total", "Total MCP tool invocations across stdio + HTTP transports.", "counter", m.MCPCalls.Load())
	write("reactor_webhook_calls_total", "Total inbound webhook deliveries.", "counter", m.WebhookCalls.Load())
	writeFloat("reactor_uptime_seconds", "Daemon uptime in seconds.", "gauge", uptimeSeconds)
	writeFloat("reactor_goroutines", "Live goroutines.", "gauge", float64(runtime.NumGoroutine()))
	writeFloat("reactor_memory_alloc_bytes", "Bytes allocated and still in use.", "gauge", float64(mem.Alloc))
	writeFloat("reactor_memory_sys_bytes", "Bytes of memory obtained from the OS.", "gauge", float64(mem.Sys))
	writeFloat("reactor_memory_gc_cycles_total", "Total completed GC cycles.", "counter", float64(mem.NumGC))
}
