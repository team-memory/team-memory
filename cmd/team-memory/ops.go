package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/mcp"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/telemetry"
)

// startTelemetry sets up tracing (exported only when an OTLP endpoint is
// configured) and the metrics registry for one process. The returned stop
// flushes traces.
func startTelemetry(ctx context.Context, component string, logger *slog.Logger) (*telemetry.Metrics, func(), error) {
	shutdown, err := telemetry.SetupTracing(ctx, "team-memory-"+component, mcp.Version, logger)
	if err != nil {
		return nil, nil, err
	}
	stop := func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(c)
	}
	return telemetry.New(component, mcp.Version), stop, nil
}

// serveOps runs the operations listener (TEAM_MEMORY_METRICS_LISTEN,
// default 127.0.0.1:9090): /metrics, /healthz (live) and /readyz. It is
// separate from the public listener so the ingress never exposes metrics.
// An empty address is refused; "off" disables it.
func serveOps(ctx context.Context, logger *slog.Logger, m *telemetry.Metrics, live func() error, ready func(context.Context) error) {
	addr := env("TEAM_MEMORY_METRICS_LISTEN", "127.0.0.1:9090")
	if addr == "off" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	check := func(fn func() error) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if fn != nil {
				if err := fn(); err != nil {
					http.Error(w, "not ok: "+err.Error(), http.StatusServiceUnavailable)
					return
				}
			}
			_, _ = w.Write([]byte("ok\n"))
		}
	}
	mux.HandleFunc("GET /healthz", check(live))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		err := ready(r.Context())
		m.SetReady(err == nil)
		check(func() error { return err })(w, r)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	go func() {
		logger.Info("operations listener", "addr", addr, "paths", "/metrics /healthz /readyz")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("operations listener failed", "err", err)
		}
	}()
}

// workerLiveness fails when the worker loop has not polled the queue for
// longer than a lease plus a margin: it is stuck, not merely idle.
func workerLiveness(w *jobs.Worker, cfg jobs.Config) func() error {
	started := time.Now()
	limit := cfg.Lease + cfg.PollInterval + 2*time.Minute
	if cfg.Lease == 0 {
		limit = 5*time.Minute + 2*time.Minute
	}
	return func() error {
		last := w.LastPoll()
		if last.IsZero() {
			last = started
		}
		if time.Since(last) > limit {
			return errors.New("worker has not polled the job queue since " + last.UTC().Format(time.RFC3339))
		}
		return nil
	}
}

// registerQueue exports the queue depth visible to the pool's role (each
// worker role sees only its own job kinds).
func registerQueue(m *telemetry.Metrics, pool *pgxpool.Pool) {
	q := db.New(pool)
	m.RegisterQueue(func(ctx context.Context) (map[[2]string]float64, error) {
		rows, err := q.JobCounts(ctx)
		out := map[[2]string]float64{}
		for _, r := range rows {
			out[[2]string{r.Kind, r.Status}] = float64(r.Jobs)
		}
		return out, err
	}, func(ctx context.Context) (map[string]float64, error) {
		rows, err := q.OldestReadyJobAge(ctx)
		out := map[string]float64{}
		for _, r := range rows {
			out[r.Kind] = r.AgeSeconds
		}
		return out, err
	})
}

// observedProvider records embedding latency and errors.
type observedProvider struct {
	embeddings.EmbeddingProvider
	m       *telemetry.Metrics
	purpose string
}

func (p observedProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	start := time.Now()
	v, err := p.EmbeddingProvider.Embed(ctx, texts)
	p.m.Embedding(p.Model().Name, p.purpose, time.Since(start), err)
	return v, err
}

func observe(p embeddings.EmbeddingProvider, m *telemetry.Metrics, purpose string) embeddings.EmbeddingProvider {
	if p == nil || m == nil {
		return p
	}
	return observedProvider{EmbeddingProvider: p, m: m, purpose: purpose}
}

// healthcheck probes the operations listener's /healthz for the image's
// HEALTHCHECK (distroless has no shell or curl). Every component runs the
// operations listener.
// With --url it probes that URL instead (the chart's helm test).
func healthcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	target := fs.String("url", "", "URL to probe (default: this process's operations listener /healthz)")
	wait := fs.Duration("wait", 0, "keep retrying for this long (a new pod may be admitted by network policy a few seconds after it starts)")
	_ = fs.Parse(args)
	if *target == "" {
		addr := env("TEAM_MEMORY_METRICS_LISTEN", "127.0.0.1:9090")
		if addr == "off" {
			return errors.New("TEAM_MEMORY_METRICS_LISTEN=off: no health endpoint")
		}
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		*target = "http://" + addr + "/healthz"
	}
	deadline := time.Now().Add(*wait)
	for {
		err := probe(ctx, *target)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}

func probe(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", target, resp.Status)
	}
	return nil
}
