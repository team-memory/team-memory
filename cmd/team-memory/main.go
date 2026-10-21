// Command team-memory runs the Team Memory server and its operator CLI.
//
// Operator subcommands (init, scope, principal, member, token) act directly on
// the database and are trusted by virtue of database access. Authorized,
// audited administration through the API arrives in stage 04.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/mcp"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

const usage = `usage: team-memory <command> [flags]

commands:
  serve                         run the MCP server
  migrate                       apply database migrations
  init                          create the first organization, owner and token
  scope create|list|config      manage the scope tree
  principal create|list         manage principals
  member add|list               grant scope roles (initial bootstrap)
  token create|list|revoke      manage personal access tokens

Configuration comes from the environment: DATABASE_URL, TEAM_MEMORY_LISTEN,
TEAM_MEMORY_DEFAULT_SCOPE.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "serve":
		return serve(ctx, args)
	case "migrate":
		return withPool(ctx, func(pool *pgxpool.Pool) error {
			if err := postgres.Migrate(ctx, pool); err != nil {
				return err
			}
			fmt.Println("migrations applied")
			return nil
		})
	case "init":
		return initCmd(ctx, args)
	case "scope":
		return scopeCmd(ctx, args)
	case "principal":
		return principalCmd(ctx, args)
	case "member":
		return memberCmd(ctx, args)
	case "token":
		return tokenCmd(ctx, args)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func withPool(ctx context.Context, fn func(*pgxpool.Pool) error) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is not set")
	}
	pool, err := postgres.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(pool)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", env("TEAM_MEMORY_LISTEN", "127.0.0.1:8080"), "listen address")
	defaultScope := fs.String("default-scope", os.Getenv("TEAM_MEMORY_DEFAULT_SCOPE"), "scope used when a tool call names none")
	migrate := fs.Bool("migrate", false, "apply migrations before serving")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		if *migrate {
			if err := postgres.Migrate(ctx, pool); err != nil {
				return err
			}
		}
		svc := memory.NewService(pool)
		mux := http.NewServeMux()
		mux.Handle("/mcp", logRequests(logger, mcp.Handler(svc, auth.NewPATs(pool), mcp.Options{DefaultScope: *defaultScope, Logger: logger})))
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
			if err := pool.Ping(r.Context()); err != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok\n"))
		})
		srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		errc := make(chan error, 1)
		go func() { errc <- srv.ListenAndServe() }()
		logger.Info("serving", "addr", *listen, "mcp_path", "/mcp")
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdown)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequests logs MCP request metadata. It never logs credentials or bodies.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"mcp_protocol_version", r.Header.Get("Mcp-Protocol-Version"), "mcp_method", r.Header.Get("Mcp-Method"),
			"user_agent", r.UserAgent(), "duration_ms", time.Since(start).Milliseconds())
	})
}

func initCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	org := fs.String("org", "", "organization slug (required), e.g. acme")
	orgName := fs.String("org-name", "", "organization display name (defaults to the slug)")
	project := fs.String("project", "default", "default project slug")
	owner := fs.String("owner", "", "owner display name (required)")
	ttl := fs.Duration("token-ttl", auth.DefaultTokenTTL, "initial token lifetime")
	_ = fs.Parse(args)
	if *org == "" || *owner == "" {
		return errors.New("--org and --owner are required")
	}
	if *orgName == "" {
		*orgName = *org
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		if err := postgres.Migrate(ctx, pool); err != nil {
			return err
		}
		res, err := auth.Init(ctx, pool, auth.InitOptions{OrgSlug: *org, OrgName: *orgName, ProjectSlug: *project, OwnerName: *owner, TokenTTL: *ttl})
		if err != nil {
			return err
		}
		fmt.Printf("organization: %s (%s)\nproject:      %s\nowner:        %s (%s)\n\n", res.OrgPath, res.OrgID, res.ProjectPath, *owner, res.OwnerID)
		fmt.Printf("Personal access token (shown once, expires %s):\n\n  %s\n\n", res.Token.ExpiresAt.Format(time.RFC3339), res.Token.Secret)
		fmt.Println("Store it in a secret manager or environment variable; it is not stored in plain text.")
		return nil
	})
}

func scopeCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory scope create|list|config")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		svc := memory.NewService(pool)
		switch args[0] {
		case "create":
			fs := flag.NewFlagSet("scope create", flag.ExitOnError)
			parent := fs.String("parent", "", "parent scope path or id (required)")
			level := fs.String("level", "", "level label, e.g. team or project (required)")
			slug := fs.String("slug", "", "slug (required)")
			name := fs.String("name", "", "display name")
			_ = fs.Parse(args[1:])
			s, err := svc.CreateScope(ctx, uuid.Nil, *parent, *level, *slug, *name)
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%s\t%s\n", s.Path, s.Level, s.ID)
		case "list":
			fs := flag.NewFlagSet("scope list", flag.ExitOnError)
			org := fs.String("org", "", "organization slug or id (default: all)")
			_ = fs.Parse(args[1:])
			scopes, err := svc.ListScopes(ctx, *org)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PATH\tLEVEL\tDEPTH\tID")
			for _, s := range scopes {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", s.Path, s.Level, s.Depth, s.ID)
			}
			return w.Flush()
		case "config":
			fs := flag.NewFlagSet("scope config", flag.ExitOnError)
			org := fs.String("org", "", "organization slug or id (required)")
			levels := fs.String("levels", "", "comma-separated allowed level labels")
			maxDepth := fs.Int("max-depth", 0, "maximum number of levels")
			_ = fs.Parse(args[1:])
			cur, err := svc.GetOrgSettings(ctx, *org)
			if err != nil {
				return err
			}
			lv, md := cur.AllowedLevels, cur.MaxDepth
			if *levels != "" {
				lv = strings.Split(*levels, ",")
			}
			if *maxDepth != 0 {
				md = *maxDepth
			}
			st, err := svc.ConfigureOrg(ctx, *org, lv, md)
			if err != nil {
				return err
			}
			fmt.Printf("levels: %s\nmax depth: %d\n", strings.Join(st.AllowedLevels, ","), st.MaxDepth)
		default:
			return fmt.Errorf("unknown scope command %q", args[0])
		}
		return nil
	})
}

func principalCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory principal create|list")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		q := db.New(pool)
		switch args[0] {
		case "create":
			fs := flag.NewFlagSet("principal create", flag.ExitOnError)
			name := fs.String("name", "", "display name (required)")
			kind := fs.String("kind", "human", "human or service")
			_ = fs.Parse(args[1:])
			if *name == "" {
				return errors.New("--name is required")
			}
			p, err := q.CreatePrincipal(ctx, db.CreatePrincipalParams{Kind: *kind, DisplayName: *name})
			if err != nil {
				return err
			}
			fmt.Println(p.ID)
		case "list":
			ps, err := q.ListPrincipals(ctx)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tKIND\tNAME")
			for _, p := range ps {
				fmt.Fprintf(w, "%s\t%s\t%s\n", p.ID, p.Kind, p.DisplayName)
			}
			return w.Flush()
		default:
			return fmt.Errorf("unknown principal command %q", args[0])
		}
		return nil
	})
}

func memberCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory member add|list")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		svc := memory.NewService(pool)
		switch args[0] {
		case "add":
			fs := flag.NewFlagSet("member add", flag.ExitOnError)
			scope := fs.String("scope", "", "scope path or id (required)")
			principal := fs.String("principal", "", "principal id (required)")
			role := fs.String("role", "reader", "reader, writer or maintainer")
			cascade := fs.Bool("cascade", false, "also grant the role on all descendant scopes")
			_ = fs.Parse(args[1:])
			pid, err := uuid.Parse(*principal)
			if err != nil {
				return errors.New("--principal must be a principal id")
			}
			r, err := memory.ParseRole(*role)
			if err != nil {
				return err
			}
			if err := svc.AddMember(ctx, uuid.Nil, *scope, pid, r, *cascade); err != nil {
				return err
			}
			fmt.Println("granted")
		case "list":
			fs := flag.NewFlagSet("member list", flag.ExitOnError)
			scope := fs.String("scope", "", "scope path or id (required)")
			_ = fs.Parse(args[1:])
			sc, err := svc.ResolveScope(ctx, *scope)
			if err != nil {
				return err
			}
			rows, err := db.New(pool).ListScopeMembers(ctx, sc.ID)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PRINCIPAL\tNAME\tROLE\tCASCADE")
			for _, m := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", m.PrincipalID, m.DisplayName, m.Role, m.Cascade)
			}
			return w.Flush()
		default:
			return fmt.Errorf("unknown member command %q", args[0])
		}
		return nil
	})
}

func tokenCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory token create|list|revoke")
	}
	return withPool(ctx, func(pool *pgxpool.Pool) error {
		pats := auth.NewPATs(pool)
		switch args[0] {
		case "create":
			fs := flag.NewFlagSet("token create", flag.ExitOnError)
			principal := fs.String("principal", "", "principal id (required)")
			name := fs.String("name", "cli", "token name")
			ttl := fs.Duration("ttl", auth.DefaultTokenTTL, "lifetime, e.g. 720h")
			_ = fs.Parse(args[1:])
			pid, err := uuid.Parse(*principal)
			if err != nil {
				return errors.New("--principal must be a principal id")
			}
			tok, err := pats.Issue(ctx, pid, *name, *ttl)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "token %s expires %s (shown once):\n", tok.ID, tok.ExpiresAt.Format(time.RFC3339))
			fmt.Println(tok.Secret)
		case "list":
			fs := flag.NewFlagSet("token list", flag.ExitOnError)
			principal := fs.String("principal", "", "principal id (required)")
			_ = fs.Parse(args[1:])
			pid, err := uuid.Parse(*principal)
			if err != nil {
				return errors.New("--principal must be a principal id")
			}
			rows, err := db.New(pool).ListPersonalAccessTokens(ctx, pid)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tPREFIX\tEXPIRES\tREVOKED")
			for _, t := range rows {
				revoked := "-"
				if t.RevokedAt != nil {
					revoked = t.RevokedAt.Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%s\t%s\t%s…\t%s\t%s\n", t.ID, t.Name, t.TokenPrefix, t.ExpiresAt.Format(time.RFC3339), revoked)
			}
			return w.Flush()
		case "revoke":
			if len(args) < 2 {
				return errors.New("usage: team-memory token revoke <token-id>")
			}
			id, err := uuid.Parse(args[1])
			if err != nil {
				return errors.New("token id must be a UUID")
			}
			if err := pats.Revoke(ctx, id); err != nil {
				return err
			}
			fmt.Println("revoked")
		default:
			return fmt.Errorf("unknown token command %q", args[0])
		}
		return nil
	})
}
