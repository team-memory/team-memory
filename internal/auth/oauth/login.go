package oauth

import (
	"crypto/subtle"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/team-memory/team-memory/internal/auth"
)

const (
	csrfCookie  = "tm_oauth_csrf"
	loginCookie = "tm_oauth_login"
	loginTTL    = 10 * time.Minute
)

// pendingLogin is an upstream login in flight, keyed by the hash of its
// state value and bound to the browser that started it by loginCookie
// (browser holds the cookie value's hash).
type pendingLogin struct {
	authRequestID string
	provider      string
	nonce         string
	verifier      string
	browser       string
	expires       time.Time
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize {{.ClientName}} · Team Memory</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1rem;color:#1d1d1f;background:#fff}
h1{font-size:1.3rem} dl{display:grid;grid-template-columns:max-content 1fr;gap:.25rem 1rem}
dt{color:#555} dd{margin:0;word-break:break-all} .warn{background:#fff4e5;border:1px solid #f0c36d;padding:.75rem;border-radius:6px}
button{font:inherit;padding:.5rem 1rem;margin:.5rem .5rem 0 0;border-radius:6px;border:1px solid #888;background:#f5f5f7;cursor:pointer}
button.primary{background:#0b57d0;color:#fff;border-color:#0b57d0}
@media (prefers-color-scheme: dark){body{background:#111;color:#eee}dt{color:#aaa}button{background:#222;color:#eee}.warn{background:#3a2a10;border-color:#8a6d2b}}
</style></head><body>
<h1>{{.ClientName}} wants to use Team Memory as you</h1>
<p>If you continue, you will sign in with your organization's identity provider, and this application will be able to read and write memories with your permissions.</p>
<dl>
<dt>Application</dt><dd>{{.ClientName}}</dd>
<dt>Client ID</dt><dd>{{.ClientID}}</dd>
<dt>Returns to</dt><dd>{{.RedirectHost}}</dd>
<dt>Access</dt><dd>{{.Resource}}</dd>
</dl>
{{if .Loopback}}<p class="warn">This application receives the sign-in on your own computer ({{.RedirectHost}}). Continue only if you just started a sign-in from a tool you trust, such as an AI coding agent.</p>{{end}}
<form method="post" action="{{.Action}}">
<input type="hidden" name="authRequestID" value="{{.AuthRequestID}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
{{range .Providers}}<button class="primary" type="submit" name="provider" value="{{.ID}}">Continue with {{.DisplayName}}</button>{{end}}
<button type="submit" name="deny" value="1">Cancel</button>
</form>
</body></html>`))

type consentData struct {
	ClientName, ClientID, RedirectHost, Resource string
	Loopback                                     bool
	Action, AuthRequestID, CSRF                  string
	Providers                                    []Provider
}

func (s *Server) securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	// form-action must allow the upstream origins: browsers apply it to the
	// redirect that follows the form submission.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' "+strings.Join(s.upstreamOrigins, " "))
}

// consent shows which client is asking before any upstream login starts.
// The MCP authorization spec requires per-client consent from a server
// that, like this one, uses one static client ID at the upstream provider
// on behalf of dynamically registered clients.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	id := r.URL.Query().Get("authRequestID")
	a, err := s.store.request(r.Context(), id)
	if err != nil || a.done {
		http.Error(w, "This sign-in request has expired. Start again from your application.", http.StatusBadRequest)
		return
	}
	c, err := s.clients.get(r.Context(), a.clientID)
	if err != nil {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	csrf := randomString(24)
	http.SetCookie(w, s.cookie(csrfCookie, csrf, "/oauth/login"))
	ru, _ := url.Parse(a.redirectURI)
	name := c.name
	if name == "" {
		name = "An unnamed application"
	}
	_ = consentPage.Execute(w, consentData{
		ClientName: name, ClientID: c.id, RedirectHost: ru.Host, Resource: a.resource,
		Loopback: isLoopbackHost(ru.Hostname()), Action: "/oauth/login", AuthRequestID: a.id, CSRF: csrf,
		Providers: s.cfg.Providers,
	})
}

func (s *Server) cookie(name, value, path string) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: int(loginTTL / time.Second),
		HttpOnly: true, Secure: strings.HasPrefix(s.cfg.Issuer, "https://"), SameSite: http.SameSiteLaxMode,
	}
}

// approve handles the consent form: deny returns access_denied to the
// client; approve starts the upstream login.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	if o := r.Header.Get("Origin"); o != "" && o != s.cfg.Issuer {
		http.Error(w, "cross-origin form submission", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ck, err := r.Cookie(csrfCookie)
	form := r.PostForm.Get("csrf")
	if err != nil || form == "" || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(form)) != 1 {
		http.Error(w, "The sign-in form expired or was not submitted from this browser. Start again.", http.StatusForbidden)
		return
	}
	a, err := s.store.request(r.Context(), r.PostForm.Get("authRequestID"))
	if err != nil || a.done {
		http.Error(w, "This sign-in request has expired. Start again from your application.", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("deny") != "" {
		s.finishWithError(w, r, a, "access_denied", "the user cancelled the sign-in")
		return
	}
	up, ok := s.upstreams[r.PostForm.Get("provider")]
	if !ok {
		http.Error(w, "unknown identity provider", http.StatusBadRequest)
		return
	}
	state, nonce, verifier, browser := randomString(24), randomString(24), randomString(32), randomString(24)
	target, err := up.authCodeURL(state, nonce, verifier, false)
	if err != nil {
		s.logger.Error("upstream login unavailable", "provider", up.cfg.ID, "err", err)
		http.Error(w, "The identity provider is unavailable.", http.StatusServiceUnavailable)
		return
	}
	if err := s.store.state.putLogin(r.Context(), hashToken(state), &pendingLogin{
		authRequestID: a.id, provider: up.cfg.ID, nonce: nonce, verifier: verifier,
		browser: hashToken(browser), expires: s.now().Add(loginTTL),
	}); err != nil {
		if !errors.Is(err, errCapacity) {
			s.logger.Error("store pending login", "err", err)
		}
		http.Error(w, "too many sign-ins in progress", http.StatusServiceUnavailable)
		return
	}
	http.SetCookie(w, s.cookie(loginCookie, browser, "/oauth/callback/"))
	http.Redirect(w, r, target, http.StatusFound)
}

// callback completes the upstream login, maps the identity to a principal
// and hands control back to the authorization server, which redirects to
// the MCP client with a Team Memory authorization code.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	q := r.URL.Query()
	pl, perr := s.store.state.takeLogin(r.Context(), hashToken(q.Get("state")))
	ck, cerr := r.Cookie(loginCookie)
	if perr != nil || pl.provider != r.PathValue("provider") || cerr != nil ||
		subtle.ConstantTimeCompare([]byte(hashToken(ck.Value)), []byte(pl.browser)) != 1 {
		http.Error(w, "This sign-in is unknown, expired, or was started in another browser. Start again from your application.", http.StatusBadRequest)
		return
	}
	a, err := s.store.request(r.Context(), pl.authRequestID)
	if err != nil || a.done {
		http.Error(w, "This sign-in request has expired. Start again from your application.", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		s.logger.Info("upstream login refused", "provider", pl.provider, "error", e)
		s.finishWithError(w, r, a, "access_denied", "the identity provider did not complete the sign-in")
		return
	}
	up := s.upstreams[pl.provider]
	id, err := up.exchange(r.Context(), q.Get("code"), pl.verifier, pl.nonce)
	if err != nil {
		s.logger.Warn("upstream login failed", "provider", pl.provider, "err", err)
		s.auditSignIn(r, uuid.Nil, up.cfg.Issuer, signInReason(err))
		if errors.Is(err, errPolicy) {
			s.finishWithError(w, r, a, "access_denied", "your account is not allowed to use this Team Memory deployment")
			return
		}
		s.finishWithError(w, r, a, "server_error", "sign-in with the identity provider failed")
		return
	}
	p, created, err := s.identities.Resolve(r.Context(), id, up.cfg.autoProvision())
	if err != nil {
		s.logger.Warn("principal mapping failed", "provider", pl.provider, "err", err)
		s.auditSignIn(r, uuid.Nil, id.Issuer, signInReason(err))
		if errors.Is(err, auth.ErrUnknownIdentity) || errors.Is(err, auth.ErrPrincipalDisabled) {
			s.finishWithError(w, r, a, "access_denied", "your account is not enabled for this Team Memory deployment")
			return
		}
		s.finishWithError(w, r, a, "server_error", "sign-in failed")
		return
	}
	if created {
		s.logger.Info("principal provisioned", "principal", p.ID, "provider", pl.provider)
	}
	if up.cfg.SyncGroupRoles {
		if err := s.identities.SyncGroupRoles(r.Context(), p.ID, id); err != nil {
			// Fail closed: a sign-in whose grants cannot be brought up to
			// date (possibly revoked at the IdP) does not proceed.
			s.logger.Error("group role sync failed", "provider", pl.provider, "err", err)
			s.finishWithError(w, r, a, "server_error", "sign-in failed")
			return
		}
	}
	s.auditSignIn(r, p.ID, id.Issuer, "ok")
	if err := s.store.complete(r.Context(), a.id, p.ID.String(), id.Issuer, s.now()); err != nil {
		http.Error(w, "This sign-in request has expired. Start again from your application.", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, op.AuthCallbackURL(s.provider)(r.Context(), a.id), http.StatusFound)
}

func (s *Server) auditSignIn(r *http.Request, principal uuid.UUID, issuer, reason string) {
	if s.audit != nil {
		s.audit.RecordSignIn(r.Context(), principal, issuer, reason)
	}
}

// signInReason maps a sign-in failure to a fixed audit code.
func signInReason(err error) string {
	switch {
	case errors.Is(err, errPolicy):
		return "policy"
	case errors.Is(err, auth.ErrUnknownIdentity):
		return "unknown_identity"
	case errors.Is(err, auth.ErrPrincipalDisabled):
		return "principal_disabled"
	}
	return "upstream_error"
}

// finishWithError redirects an error to the client. The redirect URI was
// validated against the client's registration by /oauth/authorize.
func (s *Server) finishWithError(w http.ResponseWriter, r *http.Request, a *authRequest, code, desc string) {
	_ = s.store.DeleteAuthRequest(r.Context(), a.id)
	u, err := url.Parse(a.redirectURI)
	if err != nil {
		http.Error(w, desc, http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if a.state != "" {
		q.Set("state", a.state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
