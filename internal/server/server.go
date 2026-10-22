// Package server assembles the HTTP surface of `team-memory serve`: the MCP
// endpoint, health and readiness probes, and, when SSO is configured, the
// OAuth authorization server facade.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/auth/oauth"
	"github.com/team-memory/team-memory/internal/mcp"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/portal"
	"github.com/team-memory/team-memory/internal/ratelimit"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/telemetry"
)

// Options configures the server.
type Options struct {
	// Pool must connect as a member of team_memory_app (see
	// postgres.CheckRuntimeRole): every request runs under RLS.
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
	// RatePerSecond and RateBurst limit MCP requests per principal
	// (defaults 20/s, burst 60). A negative rate disables the limit.
	RatePerSecond float64
	RateBurst     int
	// MaxBodyBytes caps MCP request bodies (default mcp.DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// Portal configures the web portal. DisablePortal turns it off.
	Portal        portal.Config
	DisablePortal bool
	// Metrics, when set, records HTTP, MCP tool and readiness metrics.
	Metrics *telemetry.Metrics
	// TrustedProxies are the networks of reverse proxies (the ingress)
	// whose X-Forwarded-For is believed (TEAM_MEMORY_TRUSTED_PROXIES).
	// Rate limits and audit events then see the real client address.
	TrustedProxies []netip.Prefix
}

// Server is the assembled handler.
type Server struct {
	handler http.Handler
	sso     *oauth.Server
	portal  *portal.Portal
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
	failures := auth.NewFailureAuditor(opts.Pool, opts.Logger)
	rate, burst := opts.RatePerSecond, opts.RateBurst
	switch {
	case rate == 0:
		rate = 20
	case rate < 0:
		rate = 0
	}
	if burst == 0 {
		burst = 60
	}
	mcpOpts := mcp.Options{DefaultScope: opts.DefaultScope, Logger: opts.Logger, MaxBodyBytes: opts.MaxBodyBytes,
		Limiter: ratelimit.New(rate, burst), OnAuthFailure: failures.Record}
	if opts.Metrics != nil {
		mcpOpts.Observer = opts.Metrics
	}

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
	s.ready = &readiness{pool: opts.Pool, sso: s.sso, ssoConfigured: opts.SSOConfigPath != "", ssoErr: ssoErr, metrics: opts.Metrics}

	mux := http.NewServeMux()
	svc := memory.NewService(opts.Pool)
	svc.Logger = opts.Logger
	mux.Handle("/mcp", s.ready.gate(mcp.Handler(svc, opts.Search, chain, mcpOpts)))
	if s.sso != nil {
		oauthMux := http.NewServeMux()
		s.sso.Register(oauthMux)
		mux.Handle("/oauth/", s.ready.gate(oauthMux))
		mux.Handle("/.well-known/", s.ready.gate(oauthMux))
	} else if opts.SSOConfigPath != "" {
		mux.Handle("/oauth/", s.ready.gate(http.NotFoundHandler()))
		mux.Handle("/.well-known/", s.ready.gate(http.NotFoundHandler()))
	}
	if !opts.DisablePortal {
		pcfg := opts.Portal
		if pcfg.PublicURL == "" {
			pcfg.PublicURL = opts.PublicURL
		}
		popts := portal.Options{Config: pcfg, Pool: opts.Pool, Memory: svc, Search: opts.Search,
			Identities: auth.NewIdentities(opts.Pool), PATs: pats, Failures: failures, Logger: opts.Logger}
		if s.sso != nil {
			popts.SSO = s.sso
		}
		pt, err := portal.New(popts)
		if err != nil {
			opts.Logger.Error("portal disabled: invalid configuration", "err", err)
		} else {
			pm := http.NewServeMux()
			pt.Register(pm)
			prefix := pt.BasePath() + "/"
			mux.Handle(prefix, s.ready.gate(pm))
			if pt.BasePath() != "" {
				mux.Handle(pt.BasePath(), s.ready.gate(pm))
			}
			s.portal = pt
			if s.sso != nil {
				s.sso.SetPortal(pt)
			}
		}
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", s.ready.serve)
	base := ""
	if s.portal != nil {
		base = s.portal.BasePath()
		if base == "" {
			base = "/"
		}
	}
	h := withRequestContext(opts.TrustedProxies, mux)
	if opts.Metrics != nil {
		h = opts.Metrics.HTTPMiddleware(base, h)
	}
	s.handler = telemetry.HTTPMiddleware(base, h)
	return s
}

// Drain makes readiness fail from now on, so load balancers stop sending
// new requests before the listener shuts down.
func (s *Server) Drain() { s.ready.draining.Store(true) }

// withRequestContext gives every request an ID (a valid incoming
// X-Request-Id, else the trace id, else a random one), echoes it in the
// response, and attaches it and the client address to the context for logs
// and audit events. Using the trace id ties audit rows, logs and traces
// together without a lookup.
func withRequestContext(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		traceID := telemetry.TraceID(r.Context())
		if traceID != "" {
			w.Header().Set("X-Trace-Id", traceID)
		}
		if !validRequestID(id) {
			id = traceID
		}
		if id == "" {
			id = uuid.NewString()
		}
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("request.id", id))
		w.Header().Set("X-Request-Id", id)
		addr := ClientAddr(r, trusted)
		ctx := memory.WithRequestID(r.Context(), id)
		ctx = auth.WithClientAddr(ctx, addr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ClientAddr returns the request's client address: the TCP peer, or, when
// the peer is a trusted proxy, the rightmost X-Forwarded-For address that is
// not itself a trusted proxy. Addresses a client wrote further left are
// never believed.
func ClientAddr(r *http.Request, trusted []netip.Prefix) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	isTrusted := func(s string) bool {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return false
		}
		for _, p := range trusted {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	if len(trusted) == 0 || !isTrusted(peer) {
		return peer
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		a, err := netip.ParseAddr(h)
		if err != nil {
			return peer // malformed chain: fall back to the proxy itself
		}
		if !isTrusted(h) {
			return a.Unmap().String()
		}
	}
	return peer
}

// ParseTrustedProxies parses a comma-separated list of CIDRs or addresses.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
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
	if o.Pool == nil {
		o.Pool = opts.Pool
	}
	if o.Audit == nil {
		o.Audit = auth.NewFailureAuditor(opts.Pool, opts.Logger)
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
	if s.portal != nil {
		s.portal.Start(ctx)
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

	metrics  *telemetry.Metrics
	draining atomic.Bool

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

func (r *readiness) check(ctx context.Context) (err error) {
	if r.metrics != nil {
		defer func() { r.metrics.SetReady(err == nil) }()
	}
	if r.draining.Load() {
		return errors.New("shutting down")
	}
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
