package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/mirror"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/telemetry"
)

const mirrorUsage = `usage: team-memory mirror <command>

  enable              start enqueuing exports and backfill every earlier event
  disable             stop enqueuing exports (queued jobs still run)
  status              delivery state per repository
  worker              run the export worker (TEAM_MEMORY_MIRROR_DATABASE_URL)
  verify              compare every repository with the database; exit 3 on drift
  requeue --repo ID   retry held and failed events of a repository
  mark-purged --memory ID
                      purge runbook: never export this memory's events again
  rebuild --repo ID --scope PATH --as PRINCIPAL [--include-archived] [--dry-run]
                      disaster recovery only: recreate memories from a repository

Git settings: TEAM_MEMORY_MIRROR_REMOTE (URL template with {repo}),
TEAM_MEMORY_MIRROR_BRANCH, TEAM_MEMORY_MIRROR_CACHE_DIR, TEAM_MEMORY_MIRROR_SSH_KEY_FILE,
TEAM_MEMORY_MIRROR_SSH_KNOWN_HOSTS_FILE, TEAM_MEMORY_MIRROR_TOKEN_FILE,
TEAM_MEMORY_MIRROR_TOKEN_USER, TEAM_MEMORY_MIRROR_CA_FILE, TEAM_MEMORY_MIRROR_EMAIL_DOMAIN.
See docs/operations/mirror.md.`

// mirrorGit builds the go-git implementation from the environment.
// Credentials are file paths (mounted Secrets), never values.
func mirrorGit() (*mirror.GoGit, error) {
	remote := os.Getenv("TEAM_MEMORY_MIRROR_REMOTE")
	if remote == "" {
		return nil, errors.New("TEAM_MEMORY_MIRROR_REMOTE is not set (e.g. ssh://git@git.example.com/team-memory-mirror/{repo}.git)")
	}
	return mirror.NewGoGit(mirror.GoGitConfig{
		RemoteTemplate:    remote,
		Branch:            os.Getenv("TEAM_MEMORY_MIRROR_BRANCH"),
		CacheDir:          env("TEAM_MEMORY_MIRROR_CACHE_DIR", filepath.Join(os.TempDir(), "team-memory-mirror")),
		SSHKeyFile:        os.Getenv("TEAM_MEMORY_MIRROR_SSH_KEY_FILE"),
		SSHKnownHostsFile: os.Getenv("TEAM_MEMORY_MIRROR_SSH_KNOWN_HOSTS_FILE"),
		TokenFile:         os.Getenv("TEAM_MEMORY_MIRROR_TOKEN_FILE"),
		TokenUser:         os.Getenv("TEAM_MEMORY_MIRROR_TOKEN_USER"),
		CAFile:            os.Getenv("TEAM_MEMORY_MIRROR_CA_FILE"),
	})
}

func mirrorCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, mirrorUsage)
		return errors.New("a mirror command is required")
	}
	switch args[0] {
	case "worker":
		return mirrorWorker(ctx)
	case "verify":
		return mirrorVerify(ctx, args[1:])
	case "rebuild":
		return mirrorRebuild(ctx, args[1:])
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		q := db.New(pool)
		switch args[0] {
		case "enable":
			if err := q.MirrorSetEnabled(ctx, true); err != nil {
				return err
			}
			n, err := q.MirrorBackfill(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("mirror enabled; %d earlier events enqueued\n", n)
		case "disable":
			if err := q.MirrorSetEnabled(ctx, false); err != nil {
				return err
			}
			fmt.Println("mirror disabled; new events are no longer enqueued (run `mirror enable` to backfill them later)")
		case "status":
			enabled, err := q.MirrorEnabled(ctx)
			if err != nil {
				return err
			}
			rows, err := q.MirrorStatus(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("enabled: %t\n", enabled)
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "REPOSITORY\tSTATE\tEVENTS\tLAST UPDATE")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", r.Repository, r.DeliveryState, r.Events, r.LastUpdate.Format(time.RFC3339))
			}
			return w.Flush()
		case "requeue":
			fs := flag.NewFlagSet("mirror requeue", flag.ExitOnError)
			repo := fs.String("repo", "", "repository key (scope id) (required)")
			_ = fs.Parse(args[1:])
			if _, err := uuid.Parse(*repo); err != nil {
				return errors.New("--repo must be a scope id")
			}
			n, err := q.MirrorRequeueHeld(ctx, *repo)
			if err != nil {
				return err
			}
			fmt.Printf("requeued %d held or failed events\n", n)
		case "mark-purged":
			fs := flag.NewFlagSet("mirror mark-purged", flag.ExitOnError)
			mem := fs.String("memory", "", "memory id (required)")
			_ = fs.Parse(args[1:])
			id, err := uuid.Parse(*mem)
			if err != nil {
				return errors.New("--memory must be a memory id")
			}
			n, err := q.MirrorMarkPurged(ctx, id)
			if err != nil {
				return err
			}
			fmt.Printf("marked %d mirror events purged; they will not be exported again\n", n)
		default:
			return fmt.Errorf("unknown mirror command %q\n%s", args[0], mirrorUsage)
		}
		return nil
	})
}

func mirrorWorker(ctx context.Context) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	g, err := mirrorGit()
	if err != nil {
		return err
	}
	cfg, err := jobsConfig(logger)
	if err != nil {
		return err
	}
	metrics, stopTracing, err := startTelemetry(ctx, "mirror", logger)
	if err != nil {
		return err
	}
	defer stopTracing()
	cfg.Metrics = metrics.Jobs()
	return withRuntimePool(ctx, "TEAM_MEMORY_MIRROR_DATABASE_URL", postgres.RoleMirror, telemetry.PGXTracer{}, func(pool *pgxpool.Pool) error {
		metrics.RegisterPool(pool, "mirror")
		registerQueue(metrics, pool)
		q := db.New(pool)
		metrics.RegisterQueryGauge("mirror_undelivered", "Mirror events not yet pushed, by delivery state.", "state",
			func(ctx context.Context) (map[string]float64, error) {
				rows, err := q.MirrorUndeliveredCounts(ctx)
				out := map[string]float64{"pending": 0, "committed": 0, "failed": 0, "held": 0}
				for _, r := range rows {
					out[r.DeliveryState] = float64(r.Events)
				}
				return out, err
			})
		ex := mirror.NewExporter(pool, g)
		ex.Logger = logger
		ex.Metrics = metrics.Mirror()
		if d := os.Getenv("TEAM_MEMORY_MIRROR_EMAIL_DOMAIN"); d != "" {
			ex.EmailDomain = d
		}
		enabled, err := q.MirrorEnabled(ctx)
		if err != nil {
			return err
		}
		if !enabled {
			logger.Warn("the mirror is disabled; no new exports are enqueued until an operator runs `team-memory mirror enable`")
		}
		w := ex.Worker(cfg)
		serveOps(ctx, logger, metrics, workerLiveness(w, cfg), func(ctx context.Context) error { return pool.Ping(ctx) })
		logger.Info("mirror worker started", "name", cfg.Name)
		return w.Run(ctx)
	})
}

func mirrorVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mirror verify", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	_ = fs.Parse(args)
	g, err := mirrorGit()
	if err != nil {
		return err
	}
	var rep mirror.VerifyReport
	err = withPool(ctx, func(pool *pgxpool.Pool) error {
		rep, err = mirror.Verify(ctx, pool, g)
		return err
	})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		fmt.Printf("repositories: %d, files: %d, drift: %d\n", rep.Repositories, rep.Files, len(rep.Drift))
		for _, d := range rep.Drift {
			fmt.Printf("  %s\t%s\t%s\t%s\n", d.Kind, d.ScopePath, d.Path, d.Detail)
		}
		for _, n := range rep.Notes {
			fmt.Println("  note:", n)
		}
	}
	if len(rep.Drift) > 0 {
		os.Exit(3)
	}
	return nil
}

func mirrorRebuild(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mirror rebuild", flag.ExitOnError)
	repo := fs.String("repo", "", "repository key: the lost scope's id (required)")
	scope := fs.String("scope", "", "scope path or id that receives the memories (required)")
	as := fs.String("as", "", "principal id the restored memories are attributed to (required)")
	archived := fs.Bool("include-archived", false, "also restore archived memories (then archive them)")
	dry := fs.Bool("dry-run", false, "report what would be restored without writing")
	_ = fs.Parse(args)
	if *repo == "" || *scope == "" || *as == "" {
		return errors.New("--repo, --scope and --as are required")
	}
	g, err := mirrorGit()
	if err != nil {
		return err
	}
	r, err := g.Open(ctx, *repo)
	if err != nil {
		return err
	}
	if err := r.Sync(ctx); err != nil {
		return err
	}
	head, _ := r.Head()
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		p, err := principalArg(ctx, pool, *as, "cli")
		if err != nil {
			return err
		}
		items, err := mirror.Rebuild(ctx, memory.NewService(pool), r, mirror.RebuildOptions{Scope: *scope, As: p, IncludeArchived: *archived, DryRun: *dry})
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "rebuilt from %s at %s\n", g.RemoteURL(*repo), head)
		fmt.Fprintln(w, "ACTION\tORIGINAL\tNEW\tPATH\tREASON")
		for _, it := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", it.Action, it.OriginalID, it.NewID, it.Path, it.Reason)
		}
		_ = w.Flush()
		return err
	})
}
