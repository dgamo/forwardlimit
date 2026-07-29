// SPDX-License-Identifier: Apache-2.0

// Package observability provides the logger and metrics.
//
// Every metric shares one configurable namespace, including the runtime and process
// collectors, so a single name selector finds everything this service exposes.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultNamespace prefixes every metric unless overridden.
const DefaultNamespace = "forwardlimit"

// NewLogger builds a slog logger. level is debug, info, warn or error; format is
// json or text.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "", "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}

	return slog.New(h), nil
}

// Metrics holds the service's Prometheus collectors.
type Metrics struct {
	registry  *prometheus.Registry
	namespace string

	decisions   *prometheus.CounterVec
	storeErrors prometheus.Counter
	cacheHits   prometheus.Gauge
	breakerOpen prometheus.Gauge
	duration    prometheus.Histogram
}

// NewMetrics builds the collectors on a private registry, under namespace. Empty
// means DefaultNamespace.
//
// Configurable because a metric name is part of an operator's alerting contract.
// Private registry so nothing is registered by accident and tests stay independent.
func NewMetrics(namespace string) *Metrics {
	if namespace = sanitiseNamespace(namespace); namespace == "" {
		namespace = DefaultNamespace
	}

	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry:  reg,
		namespace: namespace,
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "decisions_total",
			Help:      "Rate limit decisions, labelled by limiter and outcome.",
		}, []string{"limiter", "decision"}),
		storeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "store_errors_total",
			Help:      "Store errors encountered while evaluating limits. Each one means a request was allowed without being counted.",
		}),
		cacheHits: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "block_cache_hits",
			Help:      "Decisions served from the in-memory block cache without contacting the store.",
		}),
		breakerOpen: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "store_breaker_open",
			Help:      "1 when the store circuit breaker is open and limiting is failing open.",
		}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "check_duration_seconds",
			Help:      "Time spent evaluating a rate limit check.",
			Buckets:   []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
		}),
	}

	reg.MustRegister(m.decisions, m.storeErrors, m.cacheHits, m.breakerOpen, m.duration)

	// Runtime and process metrics, prefixed to match everything else.
	prefixed := prometheus.WrapRegistererWithPrefix(namespace+"_", reg)
	prefixed.MustRegister(collectors.NewGoCollector())
	prefixed.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return m
}

// sanitiseNamespace maps the namespace onto characters Prometheus permits in a
// metric name, so Namespace() reports what is actually emitted. The client library
// would replace the offending characters anyway; doing it here keeps the value we
// log and print from disagreeing with the exposition.
func sanitiseNamespace(ns string) string {
	var b strings.Builder
	for i, r := range strings.TrimSpace(ns) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == ':':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Namespace reports the prefix applied to every metric.
func (m *Metrics) Namespace() string { return m.namespace }

// Registry exposes the underlying registry.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns the /metrics HTTP handler.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// RecordDecision counts one limiter outcome.
//
// limiter is empty when no limiter applied to the request; it is recorded as
// "none" so the series is still usable as a denominator.
func (m *Metrics) RecordDecision(limiterName, decision string) {
	if limiterName == "" {
		limiterName = "none"
	}
	m.decisions.WithLabelValues(limiterName, decision).Inc()
}

// RecordStoreError counts a store failure.
func (m *Metrics) RecordStoreError() { m.storeErrors.Inc() }

// ObserveCheck records how long a check took.
func (m *Metrics) ObserveCheck(d time.Duration) { m.duration.Observe(d.Seconds()) }

// SetBlockCacheHits publishes the cache hit count.
func (m *Metrics) SetBlockCacheHits(n int64) { m.cacheHits.Set(float64(n)) }

// SetBreakerOpen publishes whether the store breaker is open.
func (m *Metrics) SetBreakerOpen(open bool) {
	if open {
		m.breakerOpen.Set(1)
		return
	}
	m.breakerOpen.Set(0)
}
