// Package server assembles the HTTP surface of `team-memory serve`: the MCP
// endpoint, health and readiness probes, and, when SSO is configured, the
// OAuth authorization server facade.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/auth/oauth"
	"github.com/team-memory/team-memory/internal/mcp"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

// Options configures the server.
type Options struct {
	Pool         *pgxpool.Pool
	Search       *search.Service
	Logger       *slog.Logger
	DefaultScope string
	// SSOConfigPath enables SSO when set (TEAM_MEMORY_SSO_CONFIG). PublicURL
	// is the default issuer (TEAM_MEMORY_PUBLIC_URL).
	SSOConfigPath string
	PublicURL     string
	// OAuth overrides the facade's dependencies (tests).
	OAuth oauth.Options
}

// Server is the assembled handler.
type Server struct {
	handler http.Handler
	sso     *oauth.Server
	ready   *readiness
}

// New builds the server. It does not fail on SSO problems: a missing or
// invalid SSO configuration or secret leaves the server not ready, and
// every MCP and OAuth request is refused with 503 until it is fixed. There
// is no fallback to PAT-only or unauthenticated operation once SSO is
// configured.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{}
	pats := auth.NewPATs(opts.Pool)
	chain := auth.Chain{PATs: pats}
	mcpOpts := mcp.Options{DefaultScope: opts.DefaultScope, Logger: opts.Logger}

	var ssoErr error
	if opts.SSOConfigPath != "" {
		s.sso, ssoErr = buildSSO(opts)
		if ssoErr != nil {
			opts.Logger.Error("SSO is configured but not usable; the server stays not ready", "err", ssoErr)
		} else {
			chain.OAuth = s.sso.Verifier()
			mcpOpts.ResourceMetadataURL = s.sso.ResourceMetadataURL()
		}
	}
	s.ready = &readiness{pool: opts.Pool, sso: s.sso, ssoConfigured: opts.SSOConfigPath != "", ssoErr: ssoErr}

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.ready.gate(mcp.Handler(memory.NewService(opts.Pool), opts.Search, chain, mcpOpts)))
	if s.sso != nil {
		oauthMux := http.NewServeMux()
		s.sso.Register(oauthMux)
		mux.Handle("/oauth/", s.ready.gate(oauthMux))
		mux.Handle("/.well-known/", s.ready.gate(oauthMux))
	} else if opts.SSOConfigPath != "" {
		mux.Handle("/oauth/", s.ready.gate(http.NotFoundHandler()))
		mux.Handle("/.well-known/", s.ready.gate(http.NotFoundHandler()))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", s.ready.serve)
	s.handler = mux
	return s
}

func buildSSO(opts Options) (*oauth.Server, error) {
	cfg, err := oauth.LoadConfig(opts.SSOConfigPath, opts.PublicURL)
	if err != nil {
		return nil, err
	}
	o := opts.OAuth
	if o.Identities == nil {
		o.Identities = auth.NewIdentities(opts.Pool)
	}
	if o.Logger == nil {
		o.Logger = opts.Logger
	}
	return oauth.New(cfg, o)
}

// Start begins background work (upstream discovery).
func (s *Server) Start(ctx context.Context) {
	if s.sso != nil {
		s.sso.Start(ctx)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Ready reports readiness, as /readyz does.
func (s *Server) Ready(ctx context.Context) error { return s.ready.check(ctx) }

// readiness combines the database and SSO checks. The SSO result is cached
// briefly because the gate consults it on every request.
type readiness struct {
	pool          *pgxpool.Pool
	sso           *oauth.Server
	ssoConfigured bool
	ssoErr        error

	mu      sync.Mutex
	checked time.Time // last successful SSO check
}

func (r *readiness) ssoStatus() error {
	if !r.ssoConfigured {
		return nil
	}
	if r.ssoErr != nil {
		return r.ssoErr
	}
	// Only success is cached, so a server becomes ready as soon as it can
	// and a failure is never hidden by a stale success for more than 2s.
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.checked.IsZero() && time.Since(r.checked) < 2*time.Second {
		return nil
	}
	r.checked = time.Time{}
	if err := r.sso.Ready(); err != nil {
		return err
	}
	r.checked = time.Now()
	return nil
}

func (r *readiness) check(ctx context.Context) error {
	var errs []error
	if err := r.pool.Ping(ctx); err != nil {
		errs = append(errs, errors.New("database unavailable"))
	}
	if err := r.ssoStatus(); err != nil {
		errs = append(errs, fmt.Errorf("sso: %w", err))
	}
	return errors.Join(errs...)
}

func (r *readiness) serve(w http.ResponseWriter, req *http.Request) {
	if err := r.check(req.Context()); err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// gate refuses requests while SSO is configured but not usable, so a broken
// SSO setup can never serve requests with weaker authentication.
func (r *readiness) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := r.ssoStatus(); err != nil {
			http.Error(w, "service not ready: SSO configuration is incomplete; see /readyz and the server log", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, req)
	})
}
