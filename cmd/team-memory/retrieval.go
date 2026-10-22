package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/telemetry"
)

// embeddingSettings is the retrieval configuration read from the environment.
type embeddingSettings struct {
	ollamaURL          string
	model              embeddings.Model
	minSimilarity      float64
	conflictSimilarity float64
}

// loadEmbeddingSettings reads:
//
//	TEAM_MEMORY_OLLAMA_URL                 Ollama base URL; unset disables embeddings
//	TEAM_MEMORY_EMBEDDING_MODEL            model name (default nomic-embed-text)
//	TEAM_MEMORY_EMBEDDING_DIMENSIONS       required for models without built-in defaults
//	TEAM_MEMORY_EMBEDDING_QUERY_PREFIX     overrides the model's query instruction
//	TEAM_MEMORY_EMBEDDING_DOCUMENT_PREFIX  overrides the model's document instruction
//	TEAM_MEMORY_VECTOR_MIN_SIMILARITY      vector candidate floor (model default)
//	TEAM_MEMORY_CONFLICT_SIMILARITY        conflict-candidate threshold (model default; 0 disables)
func loadEmbeddingSettings() (embeddingSettings, error) {
	s := embeddingSettings{ollamaURL: os.Getenv("TEAM_MEMORY_OLLAMA_URL")}
	name := env("TEAM_MEMORY_EMBEDDING_MODEL", "nomic-embed-text")
	known, ok := embeddings.KnownModel(name)
	s.model = embeddings.Model{Name: name}
	if ok {
		s.model = known.Model
		s.minSimilarity, s.conflictSimilarity = known.MinSimilarity, known.ConflictSimilarity
	}
	if v := os.Getenv("TEAM_MEMORY_EMBEDDING_DIMENSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s, fmt.Errorf("TEAM_MEMORY_EMBEDDING_DIMENSIONS: %w", err)
		}
		s.model.Dimensions = n
	}
	if v, ok := os.LookupEnv("TEAM_MEMORY_EMBEDDING_QUERY_PREFIX"); ok {
		s.model.QueryPrefix = v
	}
	if v, ok := os.LookupEnv("TEAM_MEMORY_EMBEDDING_DOCUMENT_PREFIX"); ok {
		s.model.DocumentPrefix = v
	}
	for key, dst := range map[string]*float64{
		"TEAM_MEMORY_VECTOR_MIN_SIMILARITY": &s.minSimilarity,
		"TEAM_MEMORY_CONFLICT_SIMILARITY":   &s.conflictSimilarity,
	} {
		if v := os.Getenv(key); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return s, fmt.Errorf("%s: %w", key, err)
			}
			*dst = f
		}
	}
	return s, s.model.Validate()
}

// provider returns the configured embedding provider, or nil when no Ollama
// URL is set. Calls are traced and, with m, measured.
func (s embeddingSettings) provider(m *telemetry.Metrics, purpose string) (embeddings.EmbeddingProvider, error) {
	if s.ollamaURL == "" {
		return nil, nil
	}
	p, err := embeddings.NewOllama(s.ollamaURL, s.model, &http.Client{Timeout: 60 * time.Second, Transport: telemetry.Transport(nil)})
	if err != nil {
		return nil, err
	}
	return observe(p, m, purpose), nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// newSearch builds the search service for serve. Embeddings are optional:
// without TEAM_MEMORY_OLLAMA_URL every search is full-text only. A configured
// model must be registered with matching dimensions, or serve refuses to
// start. Ollama itself may be down; searches then fall back per request.
//
//	TEAM_MEMORY_QUERY_EMBED_TIMEOUT   query embedding timeout (default 2s)
//	TEAM_MEMORY_HNSW_ITERATIVE_SCAN   off | relaxed_order | strict_order (default relaxed_order)
//	TEAM_MEMORY_HNSW_EF_SEARCH        hnsw.ef_search (default 100)
func newSearch(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, m *telemetry.Metrics) (*search.Service, error) {
	s, err := loadEmbeddingSettings()
	if err != nil {
		return nil, err
	}
	p, err := s.provider(m, "query")
	if err != nil {
		return nil, err
	}
	timeout, err := envDuration("TEAM_MEMORY_QUERY_EMBED_TIMEOUT", 2*time.Second)
	if err != nil {
		return nil, err
	}
	ef, err := envInt("TEAM_MEMORY_HNSW_EF_SEARCH", 100)
	if err != nil {
		return nil, err
	}
	cfg := search.Config{Provider: p, QueryTimeout: timeout, EFSearch: ef,
		IterativeScan: os.Getenv("TEAM_MEMORY_HNSW_ITERATIVE_SCAN"), MinVectorSimilarity: s.minSimilarity, Logger: logger}
	if p == nil {
		logger.Warn("TEAM_MEMORY_OLLAMA_URL is not set; search is full-text only")
		return search.New(pool, cfg), nil
	}
	if err := embeddings.CheckRegistered(ctx, pool, s.model); err != nil {
		return nil, err
	}
	logger.Info("hybrid search enabled", "model", s.model.Name, "dimensions", s.model.Dimensions)
	return search.New(pool, cfg), nil
}

// worker runs embedding jobs for the configured model.
//
//	TEAM_MEMORY_WORKER_NAME           locked_by label (default hostname/pid)
//	TEAM_MEMORY_WORKER_BATCH          jobs per claim and per provider call (default 16)
//	TEAM_MEMORY_WORKER_POLL_INTERVAL  idle poll interval (default 1s)
//	TEAM_MEMORY_WORKER_LEASE          job lease (default 5m)
//	TEAM_MEMORY_WORKER_BACKOFF_BASE   first retry delay (default 5s)
//	TEAM_MEMORY_WORKER_BACKOFF_MAX    retry delay cap (default 10m)
func workerCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	backfill := fs.Bool("backfill", true, "enqueue jobs for current revisions that have no embedding yet")
	_ = fs.Parse(args)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	s, err := loadEmbeddingSettings()
	if err != nil {
		return err
	}
	metrics, stopTracing, err := startTelemetry(ctx, "worker", logger)
	if err != nil {
		return err
	}
	defer stopTracing()
	p, err := s.provider(metrics, "document")
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("TEAM_MEMORY_OLLAMA_URL is not set; the worker needs an embedding provider")
	}
	cfg, err := jobsConfig(logger)
	if err != nil {
		return err
	}
	cfg.Metrics = metrics.Jobs()
	return withRuntimePool(ctx, "TEAM_MEMORY_WORKER_DATABASE_URL", postgres.RoleWorker, telemetry.PGXTracer{}, func(pool *pgxpool.Pool) error {
		metrics.RegisterPool(pool, "worker")
		registerQueue(metrics, pool)
		ix, err := embeddings.NewIndexer(ctx, pool, p)
		if err != nil {
			return err
		}
		ix.ConflictThreshold = s.conflictSimilarity
		ix.Logger = logger
		w := ix.Worker(cfg)
		serveOps(ctx, logger, metrics, workerLiveness(w, cfg), func(ctx context.Context) error { return pool.Ping(ctx) })
		// A wrong dimension count is a configuration error: stop now. An
		// unreachable provider is not; jobs retry until it returns.
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = embeddings.Probe(pctx, p)
		cancel()
		if errors.Is(err, embeddings.ErrDimensionMismatch) {
			return err
		}
		if err != nil {
			logger.Warn("embedding provider probe failed; jobs will retry", "err", err)
		}
		if *backfill {
			n, err := ix.Backfill(ctx)
			if err != nil {
				return err
			}
			logger.Info("backfill", "jobs_enqueued", n)
		}
		logger.Info("worker started", "model", s.model.Name, "name", cfg.Name)
		return w.Run(ctx)
	})
}

// jobsConfig reads the TEAM_MEMORY_WORKER_* settings shared by the
// embedding and mirror workers.
func jobsConfig(logger *slog.Logger) (jobs.Config, error) {
	host, _ := os.Hostname()
	cfg := jobs.Config{Name: env("TEAM_MEMORY_WORKER_NAME", fmt.Sprintf("%s/%d", host, os.Getpid())), Logger: logger}
	var err error
	if cfg.BatchSize, err = envInt("TEAM_MEMORY_WORKER_BATCH", 16); err != nil {
		return cfg, err
	}
	for key, dst := range map[string]*time.Duration{
		"TEAM_MEMORY_WORKER_POLL_INTERVAL": &cfg.PollInterval,
		"TEAM_MEMORY_WORKER_LEASE":         &cfg.Lease,
		"TEAM_MEMORY_WORKER_BACKOFF_BASE":  &cfg.BackoffBase,
		"TEAM_MEMORY_WORKER_BACKOFF_MAX":   &cfg.BackoffMax,
	} {
		if *dst, err = envDuration(key, 0); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func embeddingsCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory embeddings add-model|list|enable|disable")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		q := db.New(pool)
		switch args[0] {
		case "add-model":
			fs := flag.NewFlagSet("embeddings add-model", flag.ExitOnError)
			name := fs.String("name", "", "model name as the provider knows it (required)")
			dims := fs.Int("dimensions", 0, "vector dimensions (required unless the model is known)")
			_ = fs.Parse(args[1:])
			m := embeddings.Model{Name: *name, Dimensions: *dims}
			if known, ok := embeddings.KnownModel(*name); ok && *dims == 0 {
				m = known.Model
			}
			if err := embeddings.AddModel(ctx, pool, m, true); err != nil {
				return err
			}
			fmt.Printf("registered %s (%d dimensions) and built index %s\n", m.Name, m.Dimensions, m.IndexName())
			fmt.Println("new revisions now enqueue jobs for it; run a worker for this model to backfill existing memories")
		case "list":
			rows, err := q.ListEmbeddingModels(ctx)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "MODEL\tDIMENSIONS\tENABLED\tCREATED")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%d\t%t\t%s\n", r.Name, r.Dimensions, r.Enabled, r.CreatedAt.Format(time.RFC3339))
			}
			return w.Flush()
		case "enable", "disable":
			if len(args) < 2 {
				return fmt.Errorf("usage: team-memory embeddings %s <model>", args[0])
			}
			n, err := q.SetEmbeddingModelEnabled(ctx, db.SetEmbeddingModelEnabledParams{Name: args[1], Enabled: args[0] == "enable"})
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("model %q is not registered", args[1])
			}
			fmt.Printf("%sd %s\n", args[0], args[1])
		default:
			return fmt.Errorf("unknown embeddings command %q", args[0])
		}
		return nil
	})
}

func jobsCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory jobs status|dead|retry-dead")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		q := db.New(pool)
		switch args[0] {
		case "status":
			rows, err := q.JobCounts(ctx)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "KIND\tSTATUS\tJOBS")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%d\n", r.Kind, r.Status, r.Jobs)
			}
			return w.Flush()
		case "dead":
			fs := flag.NewFlagSet("jobs dead", flag.ExitOnError)
			kind := fs.String("kind", "", "job kind (embed or git_mirror; default all)")
			limit := fs.Int("limit", 50, "maximum rows")
			_ = fs.Parse(args[1:])
			rows, err := q.ListDeadJobs(ctx, db.ListDeadJobsParams{Kind: *kind, MaxRows: int32(*limit)})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tKIND\tATTEMPTS\tDEAD SINCE\tPAYLOAD\tLAST ERROR")
			for _, r := range rows {
				fmt.Fprintf(w, "%d\t%s\t%d/%d\t%s\t%s\t%s\n", r.ID, r.Kind, r.Attempts, r.MaxAttempts,
					r.UpdatedAt.Format(time.RFC3339), r.Payload, strings.ReplaceAll(r.LastError, "\n", " "))
			}
			return w.Flush()
		case "retry-dead":
			fs := flag.NewFlagSet("jobs retry-dead", flag.ExitOnError)
			kind := fs.String("kind", embeddings.JobKind, "job kind: embed or git_mirror")
			_ = fs.Parse(args[1:])
			n, err := q.RetryDeadJobs(ctx, *kind)
			if err != nil {
				return err
			}
			fmt.Printf("requeued %d dead %s jobs\n", n, *kind)
		default:
			return fmt.Errorf("unknown jobs command %q", args[0])
		}
		return nil
	})
}
