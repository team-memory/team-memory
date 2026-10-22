// Package telemetry provides Prometheus metrics and OpenTelemetry tracing
// for serve and the workers. Metrics are served on a separate listener
// (TEAM_MEMORY_METRICS_LISTEN) that the ingress never exposes. Labels are
// bounded: routes are normalized and tool names come from a fixed set.
package telemetry

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "team_memory"

// Metrics holds every metric of one process.
type Metrics struct {
	Registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	toolCalls    *prometheus.CounterVec
	toolDuration *prometheus.HistogramVec
	ready        prometheus.Gauge

	jobsClaimed  *prometheus.CounterVec
	jobsDone     *prometheus.CounterVec
	jobsRetried  *prometheus.CounterVec
	jobsDead     *prometheus.CounterVec
	jobsReaped   prometheus.Counter
	jobDuration  *prometheus.HistogramVec
	lastPoll     *prometheus.GaugeVec
	embedLatency *prometheus.HistogramVec
	embedErrors  *prometheus.CounterVec

	mirrorExports  *prometheus.CounterVec
	mirrorCommits  *prometheus.CounterVec
	mirrorDuration prometheus.Histogram
	mirrorFailures *prometheus.CounterVec
	mirrorSecrets  *prometheus.CounterVec
	mirrorHeld     *prometheus.CounterVec
}

// New registers the process, Go and build collectors and every metric.
// component is "serve", "worker" or "mirror".
func New(component, version string) *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(c prometheus.Collector) { r.MustRegister(c) }
	build := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "build_info", Help: "Build information.",
		ConstLabels: prometheus.Labels{"version": version, "component": component}})
	build.Set(1)
	f(build)
	latency := []float64{.005, .01, .025, .05, .1, .2, .3, .5, .75, 1, 2, 5, 10}
	m := &Metrics{Registry: r,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "http_requests_total",
			Help: "HTTP requests by normalized route, method and status class."}, []string{"route", "method", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "http_request_duration_seconds",
			Help: "HTTP request latency by normalized route.", Buckets: latency}, []string{"route"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "mcp_tool_calls_total",
			Help: "MCP tool calls by tool and outcome (ok, tool_error, error)."}, []string{"tool", "outcome"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "mcp_tool_duration_seconds",
			Help: "MCP tool call latency, including query embedding for memory_search.", Buckets: latency}, []string{"tool"}),
		ready: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "ready",
			Help: "1 when the process reports ready (database reachable, SSO usable when configured)."}),
		jobsClaimed: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "jobs_claimed_total",
			Help: "Jobs claimed by this worker."}, []string{"kind"}),
		jobsDone: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "jobs_succeeded_total",
			Help: "Jobs completed."}, []string{"kind"}),
		jobsRetried: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "jobs_retried_total",
			Help: "Job attempts that failed and were rescheduled."}, []string{"kind"}),
		jobsDead: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "jobs_dead_lettered_total",
			Help: "Jobs dead-lettered by this worker."}, []string{"kind"}),
		jobsReaped: prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "jobs_reaped_total",
			Help: "Jobs released after their lease expired."}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "job_batch_duration_seconds",
			Help: "Time to process one claimed batch.", Buckets: latency}, []string{"kind"}),
		lastPoll: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "worker_last_poll_timestamp_seconds",
			Help: "Unix time of the worker's last queue poll."}, []string{"kind"}),
		embedLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "embedding_duration_seconds",
			Help: "Embedding provider call latency by model and purpose (query or document).", Buckets: latency}, []string{"model", "purpose"}),
		embedErrors: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "embedding_errors_total",
			Help: "Failed embedding provider calls."}, []string{"model", "purpose"}),
		mirrorExports: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "mirror_exports_total",
			Help: "Successful repository exports (one sync and at most one push)."}, []string{"repository"}),
		mirrorCommits: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "mirror_commits_total",
			Help: "Commits written to mirror repositories."}, []string{"repository"}),
		mirrorDuration: prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "mirror_export_duration_seconds",
			Help: "Time to export one repository.", Buckets: latency}),
		mirrorFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "mirror_push_failures_total",
			Help: "Failed repository exports (remote unavailable, rejected push)."}, []string{"repository"}),
		mirrorSecrets: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "mirror_secret_detected_total",
			Help: "Events not exported because the secret scan matched."}, []string{"repository"}),
		mirrorHeld: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "mirror_held_total",
			Help: "Events held behind an earlier event that was not exported."}, []string{"repository"}),
	}
	for _, c := range []prometheus.Collector{m.httpRequests, m.httpDuration, m.toolCalls, m.toolDuration, m.ready,
		m.jobsClaimed, m.jobsDone, m.jobsRetried, m.jobsDead, m.jobsReaped, m.jobDuration, m.lastPoll, m.embedLatency, m.embedErrors,
		m.mirrorExports, m.mirrorCommits, m.mirrorDuration, m.mirrorFailures, m.mirrorSecrets, m.mirrorHeld} {
		f(c)
	}
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}

// SetReady records readiness.
func (m *Metrics) SetReady(ok bool) {
	if ok {
		m.ready.Set(1)
	} else {
		m.ready.Set(0)
	}
}

// Route normalizes a request path to a bounded label. portalBase is the
// portal's base path ("/" for a dedicated hostname, "" when disabled).
func Route(path, portalBase string) string {
	switch {
	case path == "/mcp":
		return "/mcp"
	case path == "/healthz", path == "/readyz":
		return path
	case strings.HasPrefix(path, "/oauth/"):
		return "/oauth"
	case strings.HasPrefix(path, "/.well-known/"):
		return "/.well-known"
	case portalBase == "/":
		return "/portal"
	case portalBase != "" && (path == portalBase || strings.HasPrefix(path, portalBase+"/")):
		return "/portal"
	}
	return "other"
}

// HTTPMiddleware records request counts and latency.
func (m *Metrics) HTTPMiddleware(portalBase string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &codeRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := Route(r.URL.Path, portalBase)
		m.httpRequests.WithLabelValues(route, methodLabel(r.Method), codeClass(rec.code)).Inc()
		m.httpDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodHead, http.MethodOptions, http.MethodPut:
		return m
	}
	return "other"
}

func codeClass(c int) string {
	return string(rune('0'+c/100)) + "xx"
}

type codeRecorder struct {
	http.ResponseWriter
	code int
}

func (r *codeRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *codeRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *codeRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ToolCall records one MCP tool call. outcome is ok, tool_error (a result
// with isError) or error (a protocol error).
func (m *Metrics) ToolCall(tool, outcome string, d time.Duration) {
	m.toolCalls.WithLabelValues(tool, outcome).Inc()
	m.toolDuration.WithLabelValues(tool).Observe(d.Seconds())
}

// Embedding records one provider call.
func (m *Metrics) Embedding(model, purpose string, d time.Duration, err error) {
	m.embedLatency.WithLabelValues(model, purpose).Observe(d.Seconds())
	if err != nil {
		m.embedErrors.WithLabelValues(model, purpose).Inc()
	}
}

// Jobs adapts Metrics to jobs.Metrics.
func (m *Metrics) Jobs() *JobMetrics { return &JobMetrics{m: m} }

// JobMetrics implements jobs.Metrics.
type JobMetrics struct{ m *Metrics }

func (j *JobMetrics) Claimed(kind string, n int) {
	j.m.jobsClaimed.WithLabelValues(kind).Add(float64(n))
}
func (j *JobMetrics) Succeeded(kind string, d time.Duration) {
	j.m.jobsDone.WithLabelValues(kind).Inc()
	j.m.jobDuration.WithLabelValues(kind).Observe(d.Seconds())
}
func (j *JobMetrics) Retried(kind string, _ int, _ error) {
	j.m.jobsRetried.WithLabelValues(kind).Inc()
}
func (j *JobMetrics) DeadLettered(kind string, _ error) { j.m.jobsDead.WithLabelValues(kind).Inc() }
func (j *JobMetrics) Reaped(n int)                      { j.m.jobsReaped.Add(float64(n)) }
func (j *JobMetrics) Polled(kind string)                { j.m.lastPoll.WithLabelValues(kind).SetToCurrentTime() }

// Mirror adapts Metrics to mirror.Metrics.
func (m *Metrics) Mirror() *MirrorMetrics { return &MirrorMetrics{m: m} }

// MirrorMetrics implements mirror.Metrics.
type MirrorMetrics struct{ m *Metrics }

func (x *MirrorMetrics) Exported(repo string, commits int, d time.Duration) {
	x.m.mirrorExports.WithLabelValues(repo).Inc()
	x.m.mirrorCommits.WithLabelValues(repo).Add(float64(commits))
	x.m.mirrorDuration.Observe(d.Seconds())
}
func (x *MirrorMetrics) PushFailed(repo string, _ error) {
	x.m.mirrorFailures.WithLabelValues(repo).Inc()
}
func (x *MirrorMetrics) SecretDetected(repo string) { x.m.mirrorSecrets.WithLabelValues(repo).Inc() }
func (x *MirrorMetrics) Held(repo string)           { x.m.mirrorHeld.WithLabelValues(repo).Inc() }

// RegisterPool exports pgxpool statistics for one pool.
func (m *Metrics) RegisterPool(pool *pgxpool.Pool, name string) {
	m.Registry.MustRegister(&poolCollector{pool: pool, name: name})
}

var (
	poolLabels   = []string{"pool"}
	poolAcquired = prometheus.NewDesc(ns+"_db_pool_acquired_connections", "Connections in use.", poolLabels, nil)
	poolIdle     = prometheus.NewDesc(ns+"_db_pool_idle_connections", "Idle connections.", poolLabels, nil)
	poolTotal    = prometheus.NewDesc(ns+"_db_pool_total_connections", "Open connections.", poolLabels, nil)
	poolMax      = prometheus.NewDesc(ns+"_db_pool_max_connections", "Configured maximum.", poolLabels, nil)
	poolWaits    = prometheus.NewDesc(ns+"_db_pool_empty_acquire_total", "Acquires that had to wait for a connection.", poolLabels, nil)
	poolWaitTime = prometheus.NewDesc(ns+"_db_pool_acquire_wait_seconds_total", "Total time spent waiting for a connection.", poolLabels, nil)
	poolAcquires = prometheus.NewDesc(ns+"_db_pool_acquires_total", "Successful acquires.", poolLabels, nil)
)

type poolCollector struct {
	pool *pgxpool.Pool
	name string
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{poolAcquired, poolIdle, poolTotal, poolMax, poolWaits, poolWaitTime, poolAcquires} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	g := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, c.name)
	}
	k := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, c.name)
	}
	g(poolAcquired, float64(s.AcquiredConns()))
	g(poolIdle, float64(s.IdleConns()))
	g(poolTotal, float64(s.TotalConns()))
	g(poolMax, float64(s.MaxConns()))
	k(poolWaits, float64(s.EmptyAcquireCount()))
	k(poolWaitTime, s.EmptyAcquireWaitTime().Seconds())
	k(poolAcquires, float64(s.AcquireCount()))
}

// QueueQuery returns rows of (kind, status, count). Each worker passes a
// query its role may run, so it reports the part of the queue it owns.
type QueueQuery func(ctx context.Context) (map[[2]string]float64, error)

// RegisterQueue exports job queue depth by kind and status, plus the age of
// the oldest pending job, queried at scrape time.
func (m *Metrics) RegisterQueue(q QueueQuery, oldest func(ctx context.Context) (map[string]float64, error)) {
	m.Registry.MustRegister(&queueCollector{q: q, oldest: oldest})
}

var (
	queueDepth  = prometheus.NewDesc(ns+"_jobs", "Jobs by kind and status (pending, running, succeeded, failed, dead).", []string{"kind", "status"}, nil)
	queueOldest = prometheus.NewDesc(ns+"_jobs_oldest_pending_age_seconds", "Age of the oldest ready pending job.", []string{"kind"}, nil)
	queueErr    = prometheus.NewDesc(ns+"_jobs_scrape_error", "1 when the queue query failed during this scrape.", nil, nil)
)

type queueCollector struct {
	q      QueueQuery
	oldest func(ctx context.Context) (map[string]float64, error)
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- queueDepth
	ch <- queueOldest
	ch <- queueErr
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failed := 0.0
	rows, err := c.q(ctx)
	if err != nil {
		failed = 1
	}
	for k, v := range rows {
		ch <- prometheus.MustNewConstMetric(queueDepth, prometheus.GaugeValue, v, k[0], k[1])
	}
	if c.oldest != nil {
		ages, err := c.oldest(ctx)
		if err != nil {
			failed = 1
		}
		for kind, v := range ages {
			ch <- prometheus.MustNewConstMetric(queueOldest, prometheus.GaugeValue, v, kind)
		}
	}
	ch <- prometheus.MustNewConstMetric(queueErr, prometheus.GaugeValue, failed)
}

// RegisterQueryGauge exports a gauge with one label whose values are
// queried at scrape time (for example undelivered mirror events by state).
func (m *Metrics) RegisterQueryGauge(name, help, label string, fn func(ctx context.Context) (map[string]float64, error)) {
	m.Registry.MustRegister(&queryGauge{desc: prometheus.NewDesc(ns+"_"+name, help, []string{label}, nil), fn: fn})
}

type queryGauge struct {
	desc *prometheus.Desc
	fn   func(ctx context.Context) (map[string]float64, error)
}

func (g *queryGauge) Describe(ch chan<- *prometheus.Desc) { ch <- g.desc }

func (g *queryGauge) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vals, err := g.fn(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(g.desc, err)
		return
	}
	for k, v := range vals {
		ch <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, v, k)
	}
}
