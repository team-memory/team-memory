// Command team-memory runs the Team Memory server and its operator CLI.
//
// Operator subcommands (init, scope, principal, member, token, memory, idp,
// audit, embeddings, jobs, mirror) connect as the operator role (DATABASE_URL) and
// are trusted by virtue of that access. serve connects as the app role
// (TEAM_MEMORY_APP_DATABASE_URL) and worker as the worker role
// (TEAM_MEMORY_WORKER_DATABASE_URL); both refuse to start with a role that
// could bypass row-level security.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/mcp"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/portal"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/server"
	"github.com/team-memory/team-memory/internal/telemetry"
)

const usage = `usage: team-memory <command> [flags]

commands:
  serve                         run the MCP server (app database role)
  migrate                       apply database migrations
  init                          create the first organization, owner and token
  db create-login               create a login role for serve, worker or operator
  scope create|list|config      manage the scope tree
  principal create|list|disable|enable
                                manage principals
  principal identities|link-identity|unlink-identity
                                show, link or unlink SSO identities (operator recovery)
  member add|revoke|list        grant and revoke scope roles
  memory archive                archive a memory as a maintainer principal
  idp-groups add|remove|list    map IdP groups to scope roles (optional)
  audit list                    show recent audit events of a scope
  token create|list|revoke      manage personal access tokens
  worker                        run embedding jobs for the configured model
  embeddings add-model|list|enable|disable
                                manage embedding models and their indexes
  jobs status|dead|retry-dead   inspect the job queue and dead letters, requeue them
  healthcheck                   probe this container's /healthz (Docker HEALTHCHECK)
  version                       print the version
  mirror enable|status|worker|verify|requeue|mark-purged|rebuild
                                one-way git mirror (see docs/operations/mirror.md)

Configuration comes from the environment: DATABASE_URL (operator role),
TEAM_MEMORY_APP_DATABASE_URL (serve), TEAM_MEMORY_WORKER_DATABASE_URL (worker), TEAM_MEMORY_LISTEN,
TEAM_MEMORY_DEFAULT_SCOPE, TEAM_MEMORY_PUBLIC_URL, TEAM_MEMORY_SSO_CONFIG (enables
OAuth sign-in through upstream OIDC providers), and for retrieval TEAM_MEMORY_OLLAMA_URL,
TEAM_MEMORY_EMBEDDING_MODEL and related settings (see README).`

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
	case "db":
		return dbCmd(ctx, args)
	case "memory":
		return memoryCmd(ctx, args)
	case "idp-groups":
		return idpGroupsCmd(ctx, args)
	case "audit":
		return auditCmd(ctx, args)
	case "worker":
		return workerCmd(ctx, args)
	case "embeddings":
		return embeddingsCmd(ctx, args)
	case "jobs":
		return jobsCmd(ctx, args)
	case "mirror":
		return mirrorCmd(ctx, args)
	case "healthcheck":
		return healthcheck(ctx, args)
	case "version":
		fmt.Println(mcp.Version)
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

// withPool connects as the operator (DATABASE_URL) and refuses a role that
// is neither a superuser nor a member of team_memory_operator: operator
// commands would otherwise silently see nothing under RLS.
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
	if err := postgres.CheckOperatorRole(ctx, pool); err != nil {
		return err
	}
	return fn(pool)
}

// withRuntimePool connects a runtime role from envKey and checks it with
// postgres.CheckRuntimeRole. There is no fallback to DATABASE_URL: the
// operator role would bypass the policies serve and worker depend on.
func withRuntimePool(ctx context.Context, envKey, role string, tracer pgx.QueryTracer, fn func(*pgxpool.Pool) error) error {
	url := os.Getenv(envKey)
	if url == "" {
		return fmt.Errorf("%s is not set; create a login role with `team-memory db create-login` (see docs/decisions/rls.md)", envKey)
	}
	pool, err := postgres.OpenWithTracer(ctx, url, tracer)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.CheckRuntimeRole(ctx, pool, role); err != nil {
		return err
	}
	return fn(pool)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// portalConfig reads the TEAM_MEMORY_PORTAL_* settings and rejects values
// that would otherwise silently fall back to a default.
func portalConfig() (cfg portal.Config, disabled bool, err error) {
	switch v := os.Getenv("TEAM_MEMORY_PORTAL"); v {
	case "", "on":
	case "off":
		disabled = true
	default:
		return cfg, false, fmt.Errorf("TEAM_MEMORY_PORTAL=%q must be on or off", v)
	}
	cfg = portal.Config{BasePath: env("TEAM_MEMORY_PORTAL_BASE_PATH", "/portal"), TokenSignIn: env("TEAM_MEMORY_PORTAL_TOKEN_SIGNIN", "auto"),
		CSPExtra: os.Getenv("TEAM_MEMORY_PORTAL_CSP_EXTRA")}
	for name, dst := range map[string]*time.Duration{"TEAM_MEMORY_PORTAL_IDLE_TIMEOUT": &cfg.IdleTimeout, "TEAM_MEMORY_PORTAL_ABSOLUTE_TIMEOUT": &cfg.AbsoluteTimeout} {
		if v := os.Getenv(name); v != "" {
			d, perr := time.ParseDuration(v)
			if perr != nil || d <= 0 {
				return cfg, false, fmt.Errorf("%s=%q is not a positive duration such as 30m", name, v)
			}
			*dst = d
		}
	}
	if cfg.RatePerSecond, err = envFloat("TEAM_MEMORY_PORTAL_RATE_LIMIT", 0); err != nil {
		return cfg, false, err
	}
	if cfg.Burst, err = envInt("TEAM_MEMORY_PORTAL_RATE_BURST", 0); err != nil {
		return cfg, false, err
	}
	for _, s := range strings.Split(os.Getenv("TEAM_MEMORY_PORTAL_SOURCE_SCHEMES"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.SourceSchemes = append(cfg.SourceSchemes, s)
		}
	}
	return cfg, disabled, cfg.Validate()
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", env("TEAM_MEMORY_LISTEN", "127.0.0.1:8080"), "listen address")
	defaultScope := fs.String("default-scope", os.Getenv("TEAM_MEMORY_DEFAULT_SCOPE"), "scope used when a tool call names none")
	migrate := fs.Bool("migrate", false, "apply migrations before serving")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *migrate {
		if err := withPool(ctx, func(pool *pgxpool.Pool) error { return postgres.Migrate(ctx, pool) }); err != nil {
			return err
		}
	}
	rate, err := envFloat("TEAM_MEMORY_RATE_LIMIT", 20)
	if err != nil {
		return err
	}
	burst, err := envInt("TEAM_MEMORY_RATE_BURST", 60)
	if err != nil {
		return err
	}
	pcfg, pdisabled, err := portalConfig()
	if err != nil {
		return err
	}
	trusted, err := server.ParseTrustedProxies(os.Getenv("TEAM_MEMORY_TRUSTED_PROXIES"))
	if err != nil {
		return err
	}
	shutdownDelay, err := envDuration("TEAM_MEMORY_SHUTDOWN_DELAY", 3*time.Second)
	if err != nil {
		return err
	}
	shutdownTimeout, err := envDuration("TEAM_MEMORY_SHUTDOWN_TIMEOUT", 20*time.Second)
	if err != nil {
		return err
	}
	metrics, stopTracing, err := startTelemetry(ctx, "serve", logger)
	if err != nil {
		return err
	}
	defer stopTracing()
	guard := &postgres.TxGuard{Logger: logger}
	tracer := telemetry.MultiTracer{guard, telemetry.PGXTracer{}}
	return withRuntimePool(ctx, "TEAM_MEMORY_APP_DATABASE_URL", postgres.RoleApp, tracer, func(pool *pgxpool.Pool) error {
		metrics.RegisterPool(pool, "app")
		srch, err := newSearch(ctx, pool, logger, metrics)
		if err != nil {
			return err
		}
		handler := server.New(server.Options{
			Pool: pool, Search: srch, Logger: logger, DefaultScope: *defaultScope,
			SSOConfigPath: os.Getenv("TEAM_MEMORY_SSO_CONFIG"), PublicURL: os.Getenv("TEAM_MEMORY_PUBLIC_URL"),
			RatePerSecond: rate, RateBurst: burst,
			Portal: pcfg, DisablePortal: pdisabled, Metrics: metrics, TrustedProxies: trusted,
		})
		handler.Start(ctx)
		serveOps(ctx, logger, metrics, nil, handler.Ready)
		srv := &http.Server{Addr: *listen, Handler: logRequests(logger, handler), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
			MaxHeaderBytes: 64 << 10}
		errc := make(chan error, 1)
		go func() { errc <- srv.ListenAndServe() }()
		logger.Info("serving", "addr", *listen, "mcp_path", "/mcp")
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			// Graceful shutdown: fail readiness first so endpoints are
			// removed, keep serving for the delay, then drain in-flight
			// requests within the timeout (Kubernetes'
			// terminationGracePeriodSeconds must exceed delay + timeout).
			logger.Info("shutting down", "delay", shutdownDelay, "timeout", shutdownTimeout)
			handler.Drain()
			time.Sleep(shutdownDelay)
			shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
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

// logRequests logs request metadata for MCP and OAuth endpoints. It never logs credentials or bodies.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mcpMethod := r.Header.Get("Mcp-Method")
		var captured *prefixCapture
		inner := r
		if mcpMethod == "" && r.Method == http.MethodPost && r.Body != nil && r.Body != http.NoBody {
			// Clients before protocol 2026-07-28 send no Mcp-Method header.
			// Record the body prefix as the handler reads it (after
			// authentication), rather than reading ahead. A shallow copy
			// keeps the server's own view of r.Body unchanged.
			captured = &prefixCapture{ReadCloser: r.Body}
			inner = r.WithContext(r.Context())
			inner.Body = captured
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, inner)
		if captured != nil {
			mcpMethod = jsonRPCMethod(captured.buf.Bytes())
		}
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"request_id", w.Header().Get("X-Request-Id"), "trace_id", w.Header().Get("X-Trace-Id"),
			"mcp_protocol_version", truncate(r.Header.Get("Mcp-Protocol-Version"), 32), "mcp_method", truncate(mcpMethod, 64),
			"user_agent", truncate(r.UserAgent(), 200), "duration_ms", time.Since(start).Milliseconds())
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// prefixCaptureLimit bounds how much of a request body is kept for finding
// the JSON-RPC method.
const prefixCaptureLimit = 64 << 10

// prefixCapture passes reads through and keeps the first
// prefixCaptureLimit bytes that were read.
type prefixCapture struct {
	io.ReadCloser
	buf bytes.Buffer
}

func (c *prefixCapture) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if room := prefixCaptureLimit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(n, room)])
	}
	return n, err
}

// jsonRPCMethod returns the "method" member of a JSON-RPC request object,
// scanning top-level keys in order so a truncated prefix still yields the
// method when it precedes the cut. Keys match exactly. It returns "" for
// anything else, including batches and non-string or overlong methods.
func jsonRPCMethod(prefix []byte) string {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, _ := tok.(string)
		if key != "method" {
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return ""
			}
			continue
		}
		tok, err = dec.Token()
		method, ok := tok.(string)
		if err != nil || !ok || len(method) > 64 {
			return ""
		}
		return method
	}
	return ""
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
		return errors.New("usage: team-memory principal create|list|disable|enable|identities|link-identity|unlink-identity")
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
		case "identities":
			fs := flag.NewFlagSet("principal identities", flag.ExitOnError)
			principal := fs.String("principal", "", "principal id (required)")
			_ = fs.Parse(args[1:])
			pid, err := uuid.Parse(*principal)
			if err != nil {
				return errors.New("--principal must be a principal id")
			}
			rows, err := q.ListPrincipalIdentities(ctx, pid)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ISSUER\tSUBJECT\tLINKED")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\n", r.Issuer, r.Subject, r.CreatedAt.Format(time.RFC3339))
			}
			return w.Flush()
		case "link-identity":
			// Operator recovery only: the operator vouches for both sides.
			// Users link through the portal, which proves control of both.
			fs := flag.NewFlagSet("principal link-identity", flag.ExitOnError)
			principal := fs.String("principal", "", "principal id (required)")
			issuer := fs.String("issuer", "", "upstream issuer URL, exactly as in its ID tokens (required)")
			subject := fs.String("subject", "", "upstream subject (sub claim), not an email address (required)")
			_ = fs.Parse(args[1:])
			pid, err := uuid.Parse(*principal)
			if err != nil {
				return errors.New("--principal must be a principal id")
			}
			if err := auth.NewIdentities(pool).Link(ctx, pid, auth.ExternalIdentity{Issuer: *issuer, Subject: *subject}); err != nil {
				return err
			}
			fmt.Println("linked")
		case "unlink-identity":
			// Operator recovery for a wrong link. An identity is removed from
			// one principal; it is never moved. Audited.
			fs := flag.NewFlagSet("principal unlink-identity", flag.ExitOnError)
			principal := fs.String("principal", "", "principal id (required)")
			issuer := fs.String("issuer", "", "upstream issuer URL (required)")
			subject := fs.String("subject", "", "upstream subject (required)")
			_ = fs.Parse(args[1:])
			pid, err := uuid.Parse(*principal)
			if err != nil {
				return errors.New("--principal must be a principal id")
			}
			if err := auth.NewIdentities(pool).Unlink(ctx, pid, auth.ExternalIdentity{Issuer: *issuer, Subject: *subject}); err != nil {
				return err
			}
			fmt.Println("unlinked")
		case "disable", "enable":
			// Disabling stops every credential of the principal at its next
			// request (PATs, OAuth access and refresh tokens).
			if len(args) < 2 {
				return fmt.Errorf("usage: team-memory principal %s <principal-id>", args[0])
			}
			pid, err := uuid.Parse(args[1])
			if err != nil {
				return errors.New("principal id must be a UUID")
			}
			n, err := q.SetPrincipalDisabled(ctx, db.SetPrincipalDisabledParams{ID: pid, Disabled: args[0] == "disable"})
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("principal %s not found", pid)
			}
			if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{Action: "principal." + args[0], TargetType: "principal",
				TargetID: pid.String(), Outcome: "success", Details: []byte(`{"via":"cli"}`)}); err != nil {
				return err
			}
			fmt.Println(args[0] + "d")
		default:
			return fmt.Errorf("unknown principal command %q", args[0])
		}
		return nil
	})
}

func memberCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: team-memory member add|revoke|list")
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
		case "revoke":
			fs := flag.NewFlagSet("member revoke", flag.ExitOnError)
			scope := fs.String("scope", "", "scope path or id (required)")
			principal := fs.String("principal", "", "principal id (required)")
			_ = fs.Parse(args[1:])
			pid, err := uuid.Parse(*principal)
			if err != nil {
				return errors.New("--principal must be a principal id")
			}
			if err := svc.RemoveMember(ctx, *scope, pid); err != nil {
				return err
			}
			fmt.Println("revoked; access ends with the principal's next request")
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
