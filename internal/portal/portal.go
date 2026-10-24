// Package portal is the Team Memory web portal: server-rendered HTML for
// browsing, searching and reviewing memories across the scope tree. It is an
// adapter over internal/memory and internal/search, like the MCP adapter:
// every request runs in the signed-in principal's transaction (so row-level
// security decides what exists), and the portal never reads tables itself
// except its own session tables. See docs/decisions/portal.md.
package portal

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/auth/oauth"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/ratelimit"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/telemetry"
)

//go:embed templates/*.html static/* public/*.html
var assets embed.FS

// SSO is the part of the OAuth facade the portal uses to sign people in and
// to connect SSO. Nil means solo mode (token sign-in only).
type SSO interface {
	Providers() []oauth.ProviderInfo
	PortalAuthURL(provider, state, nonce, verifier string, forceLogin bool) (string, error)
	PortalExchange(ctx context.Context, provider, code, verifier, nonce string) (auth.ExternalIdentity, error)
	PortalResolve(ctx context.Context, provider string, id auth.ExternalIdentity) (auth.ExternalIdentityResult, error)
	Origins() []string
	Ready() error
}

// Config is the portal's deployment configuration.
type Config struct {
	// BasePath is where the portal is mounted: "/portal" by default, "/"
	// for the root of its own hostname.
	BasePath string
	// PublicURL is the origin people use (https://memory.example.com). It
	// sets the Secure cookie attribute and the Origin check. Empty (local
	// development) trusts the request's Host header and uses insecure
	// cookies.
	PublicURL string
	// IdleTimeout ends a session unused for this long (default 30m);
	// AbsoluteTimeout ends it regardless (default 12h).
	IdleTimeout, AbsoluteTimeout time.Duration
	// SourceSchemes are extra URL schemes allowed in links, beside https,
	// http and mailto (for example "jira").
	SourceSchemes []string
	// FormActionOrigins and CSPExtra extend the Content-Security-Policy.
	// Upstream identity provider origins are added automatically.
	FormActionOrigins []string
	CSPExtra          string
	// RatePerSecond and Burst limit requests per session (defaults 10/s,
	// burst 40); a negative rate disables the limit.
	RatePerSecond float64
	Burst         int
	// LoginRatePerMinute limits sign-in attempts per client address (default
	// 12, burst 10); negative disables the limit.
	LoginRatePerMinute int
	// TokenSignIn selects who may sign in with a personal access token:
	//   "auto" (default): solo mode only (no SSO configured). With SSO,
	//     nobody: SSO policy (MFA, domain and group rules, deprovisioning)
	//     cannot be bypassed.
	//   "owner": additionally, with SSO, the organization owner while they
	//     have no linked SSO identity. This is how the owner reaches
	//     Connect SSO; it closes by itself once they have linked one.
	//   "all": with SSO, any human without a linked identity (discouraged).
	//   "off": never.
	TokenSignIn string
}

// Validate rejects settings that would otherwise silently fall back.
func (c Config) Validate() error {
	switch c.TokenSignIn {
	case "", "auto", "owner", "all", "off":
	default:
		return fmt.Errorf("portal token sign-in %q must be auto, owner, all or off", c.TokenSignIn)
	}
	if c.BasePath != "" && c.BasePath != "/" && (!strings.HasPrefix(c.BasePath, "/") || strings.ContainsAny(c.BasePath, "?#\"'<> ")) {
		return fmt.Errorf("portal base path %q must start with / and contain no special characters", c.BasePath)
	}
	return nil
}

// Options carries the portal's dependencies.
type Options struct {
	Config        Config
	Pool          *pgxpool.Pool
	Memory        *memory.Service
	Search        *search.Service
	Identities    *auth.Identities
	PATs          *auth.PATs
	Failures      *auth.FailureAuditor
	SSO           SSO
	Logger        *slog.Logger
	Now           func() time.Time
	ServerVersion string
}

// Portal serves the web UI.
type Portal struct {
	cfg      Config
	base     string
	secure   bool
	origin   string
	o        Options
	sess     *sessions
	render   *renderer
	pages    map[string]*template.Template
	limiter  *ratelimit.Limiter
	loginLim *ratelimit.Limiter
	log      *slog.Logger
	csp      string
}

// New builds the portal.
func New(o Options) (*Portal, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := o.Config
	if err := c.Validate(); err != nil {
		return nil, err
	}
	switch c.BasePath {
	case "":
		c.BasePath = "/portal"
	case "/":
		c.BasePath = ""
	}
	c.BasePath = strings.TrimRight(c.BasePath, "/")
	if c.BasePath != "" && (!strings.HasPrefix(c.BasePath, "/") || strings.ContainsAny(c.BasePath, "?#\"'<> ")) {
		return nil, fmt.Errorf("portal base path %q must start with / and contain no special characters", c.BasePath)
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 30 * time.Minute
	}
	if c.AbsoluteTimeout <= 0 {
		c.AbsoluteTimeout = 12 * time.Hour
	}
	rate, burst := c.RatePerSecond, c.Burst
	switch {
	case rate == 0:
		rate = 10
	case rate < 0:
		rate = 0
	}
	if burst == 0 {
		burst = 40
	}
	loginRate := float64(c.LoginRatePerMinute) / 60
	switch {
	case c.LoginRatePerMinute == 0:
		loginRate = 0.2
	case c.LoginRatePerMinute < 0:
		loginRate = 0
	}
	p := &Portal{cfg: c, base: c.BasePath, o: o, log: o.Logger, render: newRenderer(c.SourceSchemes),
		limiter: ratelimit.New(rate, burst), loginLim: ratelimit.New(loginRate, 10),
		sess: &sessions{pool: o.Pool, idle: c.IdleTimeout, absolute: c.AbsoluteTimeout, now: o.Now}}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("portal public URL %q is not a valid origin", c.PublicURL)
		}
		p.origin = u.Scheme + "://" + u.Host
		p.secure = u.Scheme == "https"
	}
	p.csp = p.buildCSP()
	if err := p.parseTemplates(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Portal) buildCSP() string {
	form := []string{"'self'"}
	form = append(form, p.cfg.FormActionOrigins...)
	if p.o.SSO != nil {
		// Browsers apply form-action to the redirect that follows a form
		// submission, and sign-in redirects to the identity provider.
		form = append(form, p.o.SSO.Origins()...)
	}
	csp := "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; " +
		"form-action " + strings.Join(form, " ")
	if p.cfg.CSPExtra != "" {
		csp += "; " + p.cfg.CSPExtra
	}
	return csp
}

// Start launches the periodic cleanup of expired sessions.
func (p *Portal) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.sess.sweep(ctx)
			}
		}
	}()
}

// BasePath returns the mount path ("" for the root).
func (p *Portal) BasePath() string { return p.base }

// Register mounts the portal on mux.
func (p *Portal) Register(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, p.wrap(fn)) }
	b := p.base
	h("GET "+b+"/static/{file}", p.static)
	h("GET "+b+"/login", p.loginPage)
	h("POST "+b+"/login/token", p.loginToken)
	h("POST "+b+"/login/sso/{provider}", p.loginSSO)
	h("POST "+b+"/logout", p.authed(p.logout))
	h("GET "+b+"/account", p.authed(p.account))
	h("POST "+b+"/account/link/{provider}", p.authed(p.linkSSO))
	h("GET "+b+"/search", p.authed(p.searchPage))
	h("GET "+b+"/s/{path...}", p.authed(p.scopePage))
	h("GET "+b+"/members/{path...}", p.authed(p.membersPage))
	h("GET "+b+"/audit/{path...}", p.authed(p.auditPage))
	h("GET "+b+"/review/{path...}", p.authed(p.reviewPage))
	h("GET "+b+"/m/{id}", p.authed(p.memoryPage))
	h("POST "+b+"/m/{id}/approve", p.authed(p.approve))
	h("POST "+b+"/m/{id}/reject", p.authed(p.reject))
	h("POST "+b+"/m/{id}/archive", p.authed(p.archive))
	h("POST "+b+"/m/{id}/supersede", p.authed(p.supersede))
	h("POST "+b+"/m/{id}/promote", p.authed(p.promote))
	h("POST "+b+"/m/{id}/override", p.authed(p.markOverride))
	h("GET "+b+"/promotions/{id}", p.authed(p.promotionPage))
	h("POST "+b+"/promotions/{id}/accept", p.authed(p.promotionAccept))
	h("POST "+b+"/promotions/{id}/reject", p.authed(p.promotionReject))
	h("POST "+b+"/promotions/{id}/cancel", p.authed(p.promotionCancel))
	if b == "" {
		h("GET /{$}", p.authed(p.home))
	} else {
		h("GET "+b+"/{$}", p.authed(p.home))
		h("GET "+b, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, b+"/", http.StatusFound) })
	}
	if b != "" {
		// Unknown paths under the base path get the same page as hidden ones.
		h(b+"/", func(w http.ResponseWriter, r *http.Request) { p.notFound(w, r) })
	}
}

// --- request plumbing ----------------------------------------------------

// wrap applies headers, panic recovery and the global per-client limit.
func (p *Portal) wrap(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				p.log.Error("portal panic", "panic", fmt.Sprint(rec), "request_id", memory.RequestID(r.Context()))
				p.errorPage(w, r, http.StatusInternalServerError, "Something went wrong", "The request failed. Quote the request ID if you report it.")
			}
		}()
		p.securityHeaders(w)
		next(w, r)
	})
}

func (p *Portal) securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", p.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	// not no-referrer: browsers send "Origin: null" on same-origin form posts
	// under that policy, which the Origin check would have to accept.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cache-Control", "no-store")
	if p.secure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (p *Portal) cookieName() string {
	if p.secure {
		return "__Secure-tm_portal"
	}
	return "tm_portal"
}

func (p *Portal) cookiePath() string {
	if p.base == "" {
		return "/"
	}
	return p.base
}

func (p *Portal) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: p.secure, SameSite: http.SameSiteLaxMode})
}

func (p *Portal) clearSessionCookie(w http.ResponseWriter) {
	p.setCookie(w, p.cookieName(), "", p.cookiePath(), -1)
}

// authed requires a live session and a principal that is still enabled,
// then enforces CSRF on state-changing methods.
func (p *Portal) authed(next func(http.ResponseWriter, *http.Request, *req) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(p.cookieName())
		if err != nil {
			p.toLogin(w, r)
			return
		}
		sess, err := p.sess.lookup(r.Context(), ck.Value)
		if err != nil {
			if !errors.Is(err, ErrNoSession) {
				p.log.Error("session lookup", "err", err)
			}
			p.clearSessionCookie(w)
			p.toLogin(w, r)
			return
		}
		if ok, wait := p.limiter.Allow(sess.Hash); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
			p.errorPage(w, r, http.StatusTooManyRequests, "Slow down", "Too many requests. Wait a moment and try again.")
			return
		}
		pr, err := p.o.Identities.Principal(r.Context(), sess.Principal)
		if err != nil {
			// Disabled or removed principal: end the session.
			_ = p.sess.revoke(r.Context(), sess.Hash)
			p.clearSessionCookie(w)
			p.toLogin(w, r)
			return
		}
		pr.AuthMethod = memory.AuthPortal
		r = r.WithContext(memory.WithAttribution(r.Context(), memory.Attribution{Channel: "portal", AuthMethod: memory.AuthPortal, RequestID: memory.RequestID(r.Context()), ServerVersion: p.o.ServerVersion, TraceID: telemetry.TraceID(r.Context())}))
		rq := &req{r: r, w: w, sess: sess, p: pr}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := p.checkOrigin(r); err != nil || !csrfOK(r, sess.CSRF) {
				p.log.Warn("csrf rejected", "path", r.URL.Path, "request_id", memory.RequestID(r.Context()))
				p.errorPage(w, r, http.StatusForbidden, "Request not accepted", "This form was not submitted from a page we served to you, or it expired. Go back, reload the page and try again.")
				return
			}
		}
		if err := next(w, r, rq); err != nil {
			p.fail(w, r, err)
		}
	}
}

func csrfOK(r *http.Request, want string) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	got := r.PostForm.Get("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// checkOrigin rejects cross-origin form submissions. The CSRF token is the
// primary defense; this is the second layer.
func (p *Portal) checkOrigin(r *http.Request) error {
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return errors.New("cross-site request")
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return nil
	}
	if p.origin != "" {
		if o != p.origin {
			return errors.New("origin mismatch")
		}
		return nil
	}
	u, err := url.Parse(o)
	if err != nil || u.Host != r.Host {
		return errors.New("origin mismatch")
	}
	return nil
}

func (p *Portal) toLogin(w http.ResponseWriter, r *http.Request) {
	target := p.base + "/login"
	if r.Method == http.MethodGet && r.URL.Path != p.base+"/login" {
		if next := r.URL.RequestURI(); strings.HasPrefix(next, p.base+"/") || p.base == "" {
			target += "?next=" + url.QueryEscape(next)
		}
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeNext accepts only plain paths inside the portal: no control
// characters (browsers strip tabs and newlines inside URLs, so "/\t/host"
// would become "//host"), no scheme or host, no dot segments.
func (p *Portal) safeNext(next string) string {
	fallback := p.base + "/"
	if next == "" || len(next) > 2048 || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return fallback
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return fallback
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") && u.Path != "/" {
		return fallback
	}
	if p.base != "" && !strings.HasPrefix(u.Path, p.base+"/") {
		return fallback
	}
	return next
}

// req is an authenticated request.
type req struct {
	r    *http.Request
	w    http.ResponseWriter
	sess Session
	p    memory.Principal
}

func (q *req) uuidParam(name string) (uuid.UUID, error) {
	id, err := uuid.Parse(q.r.PathValue(name))
	if err != nil {
		return uuid.Nil, memory.ErrNotFound
	}
	return id, nil
}
