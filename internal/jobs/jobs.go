// Package jobs runs background work from the PostgreSQL jobs table, which
// doubles as a transactional outbox: producers insert jobs in the same
// transaction as the change that needs them.
//
// Delivery is at least once. A worker claims ready jobs with FOR UPDATE SKIP
// LOCKED and a lease; a failed job is retried with exponential backoff and
// dead-lettered after its max_attempts; a job whose worker vanished is
// released when its lease expires. Handlers must therefore be idempotent.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/telemetry"
)

// Job is a claimed job.
type Job struct {
	ID          int64
	Kind        string
	Payload     []byte
	Attempt     int // 1 for the first try
	MaxAttempts int
	// Lease identifies this claim. Outcomes are recorded only while it is
	// current, so a worker whose lease expired cannot overwrite a newer claim.
	Lease uuid.UUID
}

// Outcome is a handler's verdict on one job. A nil Err means success.
type Outcome struct {
	Err error
	// Permanent marks failures that retrying cannot fix (bad payload,
	// misconfiguration); the job is dead-lettered immediately.
	Permanent bool
}

// Handler processes a batch of claimed jobs of one kind and returns one
// outcome per job, in order. Batching lets the embedding handler send one
// provider request for many jobs.
type Handler func(ctx context.Context, batch []Job) []Outcome

// Metrics receives worker events (telemetry.JobMetrics in production). The
// default is a no-op. A Metrics that also has Polled(kind string) is told
// about every queue poll.
type Metrics interface {
	Claimed(kind string, n int)
	Succeeded(kind string, d time.Duration)
	Retried(kind string, attempt int, err error)
	DeadLettered(kind string, err error)
	Reaped(n int)
}

type nopMetrics struct{}

func (nopMetrics) Claimed(string, int)             {}
func (nopMetrics) Succeeded(string, time.Duration) {}
func (nopMetrics) Retried(string, int, error)      {}
func (nopMetrics) DeadLettered(string, error)      {}
func (nopMetrics) Reaped(int)                      {}

// Config tunes a worker. Zero values take the defaults noted.
type Config struct {
	// Name labels this worker in locked_by for operators (default "worker").
	// It is informational: claims are identified by a per-claim lease id.
	Name string
	// KeyPrefix restricts claims to jobs whose dedupe key starts with it.
	KeyPrefix string
	// BatchSize is the maximum number of jobs claimed at once (default 16).
	BatchSize int
	// PollInterval is the wait between polls when no job is ready (default 1s).
	PollInterval time.Duration
	// Lease is how long a claimed job may run before another worker may take
	// it over (default 5m). It must exceed the slowest handler call.
	Lease time.Duration
	// BackoffBase and BackoffMax bound the retry delay:
	// min(BackoffMax, BackoffBase * 2^(attempt-1)), with +-20% jitter
	// (defaults 5s and 10m).
	BackoffBase time.Duration
	BackoffMax  time.Duration
	Metrics     Metrics
	Logger      *slog.Logger
}

func (c *Config) defaults() {
	if c.Name == "" {
		c.Name = "worker"
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 16
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 5 * time.Minute
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 5 * time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 10 * time.Minute
	}
	if c.Metrics == nil {
		c.Metrics = nopMetrics{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

// Worker polls one job kind.
type Worker struct {
	pool     *pgxpool.Pool
	kind     string
	handler  Handler
	cfg      Config
	lastPoll atomic.Int64 // unix nanoseconds of the last completed claim query
}

// LastPoll reports when the worker last polled the queue (zero before the
// first poll). Liveness probes use it to detect a stuck loop.
func (w *Worker) LastPoll() time.Time {
	n := w.lastPoll.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// NewWorker returns a worker for one job kind.
func NewWorker(pool *pgxpool.Pool, kind string, h Handler, cfg Config) *Worker {
	cfg.defaults()
	return &Worker{pool: pool, kind: kind, handler: h, cfg: cfg}
}

// Backoff returns the retry delay before the next attempt, without jitter.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := float64(base) * math.Pow(2, float64(attempt-1))
	if d > float64(max) || math.IsInf(d, 1) {
		return max
	}
	return time.Duration(d)
}

func (w *Worker) backoff(attempt int) time.Duration {
	d := Backoff(attempt, w.cfg.BackoffBase, w.cfg.BackoffMax)
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// Run polls until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	lastReap := time.Time{}
	for {
		if time.Since(lastReap) > w.cfg.Lease/2 {
			if err := w.Reap(ctx); err != nil && ctx.Err() == nil {
				w.cfg.Logger.Error("reap expired jobs", "err", err)
			}
			lastReap = time.Now()
		}
		n, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.cfg.Logger.Error("job batch failed", "kind", w.kind, "err", err)
		}
		if n == w.cfg.BatchSize {
			continue // more work is probably ready
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.cfg.PollInterval):
		}
	}
}

// Reap releases jobs whose lease expired.
func (w *Worker) Reap(ctx context.Context) error {
	n, err := db.New(w.pool).ReapExpiredJobs(ctx, w.cfg.Lease.Seconds())
	if err != nil {
		return err
	}
	if n > 0 {
		w.cfg.Metrics.Reaped(int(n))
		w.cfg.Logger.Warn("released jobs with expired leases", "jobs", n)
	}
	return nil
}

// RunOnce claims and processes at most one batch and reports how many jobs
// it claimed.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	q := db.New(w.pool)
	rows, err := q.ClaimJobs(ctx, db.ClaimJobsParams{
		Worker: w.cfg.Name, Kind: w.kind, KeyPrefix: w.cfg.KeyPrefix, MaxJobs: int32(w.cfg.BatchSize),
	})
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	w.lastPoll.Store(time.Now().UnixNano())
	if p, ok := w.cfg.Metrics.(interface{ Polled(string) }); ok {
		p.Polled(w.kind)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	w.cfg.Metrics.Claimed(w.kind, len(rows))
	batch := make([]Job, len(rows))
	for i, r := range rows {
		batch[i] = Job{ID: r.ID, Kind: r.Kind, Payload: r.Payload, Attempt: int(r.Attempts), MaxAttempts: int(r.MaxAttempts)}
		if r.LeaseID != nil {
			batch[i].Lease = *r.LeaseID
		}
	}
	start := time.Now()
	ctx, span := telemetry.Tracer.Start(ctx, "jobs.batch "+w.kind, trace.WithAttributes(
		attribute.String("job.kind", w.kind), attribute.Int("job.count", len(batch))))
	outcomes := w.handler(ctx, batch)
	span.End()
	if len(outcomes) != len(batch) {
		err := fmt.Errorf("handler returned %d outcomes for %d jobs", len(outcomes), len(batch))
		outcomes = make([]Outcome, len(batch))
		for i := range outcomes {
			outcomes[i] = Outcome{Err: err}
		}
	}
	// Record outcomes even if ctx was cancelled mid-batch, so a shutdown does
	// not leave jobs running until their lease expires.
	rec, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var errs []error
	for i, j := range batch {
		if err := w.record(rec, q, j, outcomes[i], time.Since(start)); err != nil {
			errs = append(errs, err)
		}
	}
	return len(batch), errors.Join(errs...)
}

func (w *Worker) record(ctx context.Context, q *db.Queries, j Job, o Outcome, d time.Duration) error {
	var (
		n   int64
		err error
	)
	switch {
	case o.Err == nil:
		n, err = q.CompleteJob(ctx, db.CompleteJobParams{ID: j.ID, LeaseID: j.Lease})
		if err == nil {
			w.cfg.Metrics.Succeeded(j.Kind, d)
		}
	case o.Permanent || j.Attempt >= j.MaxAttempts:
		n, err = q.DeadLetterJob(ctx, db.DeadLetterJobParams{ID: j.ID, LeaseID: j.Lease, LastError: truncate(o.Err.Error())})
		if err == nil {
			w.cfg.Metrics.DeadLettered(j.Kind, o.Err)
			w.cfg.Logger.Error("job dead-lettered", "job", j.ID, "kind", j.Kind, "attempt", j.Attempt, "err", o.Err)
		}
	default:
		delay := w.backoff(j.Attempt)
		n, err = q.RetryJob(ctx, db.RetryJobParams{ID: j.ID, LeaseID: j.Lease, LastError: truncate(o.Err.Error()), DelaySeconds: delay.Seconds()})
		if err == nil {
			w.cfg.Metrics.Retried(j.Kind, j.Attempt, o.Err)
			w.cfg.Logger.Warn("job failed; will retry", "job", j.ID, "kind", j.Kind, "attempt", j.Attempt, "retry_in", delay, "err", o.Err)
		}
	}
	if err != nil {
		return fmt.Errorf("record job %d: %w", j.ID, err)
	}
	if n == 0 {
		// The lease expired and the job was released or claimed again; the
		// newer claim's outcome wins.
		w.cfg.Logger.Warn("job outcome not recorded: lease lost", "job", j.ID)
	}
	return nil
}

func truncate(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}
