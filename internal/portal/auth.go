package portal

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/auth/oauth"
	"github.com/team-memory/team-memory/internal/memory"
)

const (
	preCSRFCookie  = "tm_portal_csrf"
	loginCookie    = "tm_portal_login"
	callbackCookie = "/oauth/callback/"
)

type loginData struct {
	Next      string
	Providers []oauth.ProviderInfo
	Token     bool
	Error     string
	CSRF      string
}

// loginPage shows the sign-in form: SSO buttons when configured, and a
// personal-access-token field when token sign-in applies.
func (p *Portal) loginPage(w http.ResponseWriter, r *http.Request) {
	p.renderLogin(w, r, http.StatusOK, "")
}

func (p *Portal) renderLogin(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	csrf := randomToken(24)
	p.setCookie(w, preCSRFCookie, csrf, p.cookiePath(), int(loginStateTTL.Seconds()))
	d := loginData{Next: p.safeNext(r.FormValue("next")), Token: p.cfg.TokenSignIn != "off", Error: errMsg, CSRF: csrf}
	if p.o.SSO != nil {
		d.Providers = p.o.SSO.Providers()
	}
	pg := page{Title: "Sign in", Base: p.base, CSRF: csrf, Data: d, RequestID: memory.RequestID(r.Context())}
	if r.URL.Query().Get("ok") == "signedout" {
		pg.Flash, pg.FlashKind = flashes["signedout"], "ok"
	}
	if p.o.SSO != nil {
		if err := p.o.SSO.Ready(); err != nil {
			d.Providers = nil
			d.Error = "Single sign-on is configured but not available right now. Ask an administrator to check the server."
			pg.Data = d
		}
	}
	if err := p.execute(w, status, "login.html", pg); err != nil {
		p.fail(w, r, err)
	}
}

// preAuthOK validates the CSRF cookie/field pair of the sign-in forms and
// the request origin.
func (p *Portal) preAuthOK(r *http.Request) bool {
	if err := p.checkOrigin(r); err != nil {
		p.log.Warn("sign-in form rejected", "reason", err.Error(), "request_id", memory.RequestID(r.Context()))
		return false
	}
	if r.ParseForm() != nil {
		return false
	}
	ck, err := r.Cookie(preCSRFCookie)
	got := r.PostForm.Get("csrf")
	ok := err == nil && got != "" && subtle.ConstantTimeCompare([]byte(ck.Value), []byte(got)) == 1
	if !ok {
		p.log.Warn("sign-in form rejected", "reason", "csrf cookie/field mismatch", "has_cookie", err == nil, "has_field", got != "",
			"request_id", memory.RequestID(r.Context()))
	}
	return ok
}

func (p *Portal) clientKey(r *http.Request) string { return auth.ClientAddr(r.Context()) }

// startSession replaces any session the browser presented with a new one.
func (p *Portal) startSession(w http.ResponseWriter, r *http.Request, principal uuid.UUID, method string) error {
	if ck, err := r.Cookie(p.cookieName()); err == nil {
		_ = p.sess.revoke(r.Context(), hashID(ck.Value))
	}
	cookie, _, err := p.sess.create(r.Context(), principal, method)
	if err != nil {
		return err
	}
	p.setCookie(w, p.cookieName(), cookie, p.cookiePath(), int(p.cfg.AbsoluteTimeout.Seconds()))
	p.setCookie(w, preCSRFCookie, "", p.cookiePath(), -1)
	return nil
}

// tokenSignInAllowed applies Config.TokenSignIn. With SSO configured the
// default is closed, so a personal access token never bypasses the identity
// provider's policy; "owner" opens it for the organization owner until they
// have linked an identity.
func (p *Portal) tokenSignInAllowed(r *http.Request, pr memory.Principal) bool {
	mode := p.cfg.TokenSignIn
	if mode == "" {
		mode = "auto"
	}
	switch {
	case mode == "off":
		return false
	case p.o.SSO == nil:
		return true
	case mode == "auto":
		return false
	}
	ids, err := p.o.Identities.List(r.Context(), pr.ID)
	if err != nil || len(ids) > 0 {
		return false
	}
	return mode == "all" || p.o.Memory.IsOrgOwner(r.Context(), pr)
}

func (p *Portal) loginToken(w http.ResponseWriter, r *http.Request) {
	if !p.preAuthOK(r) {
		p.renderLogin(w, r, http.StatusForbidden, "The sign-in form expired. Try again.")
		return
	}
	if ok, _ := p.loginLim.Allow(p.clientKey(r)); !ok {
		p.renderLogin(w, r, http.StatusTooManyRequests, "Too many sign-in attempts. Wait a minute and try again.")
		return
	}
	deny := func(reason, msg string) {
		if p.o.Failures != nil {
			p.o.Failures.Record(r.Context(), "portal_token", reason)
		}
		p.renderLogin(w, r, http.StatusUnauthorized, msg)
	}
	pr, _, err := p.o.PATs.Authenticate(r.Context(), r.PostForm.Get("token"))
	if err != nil {
		deny("invalid_token", "That token was not accepted.")
		return
	}
	if pr.Kind != "human" {
		deny("not_human", "That token belongs to a service account, which cannot use the portal.")
		return
	}
	if !p.tokenSignInAllowed(r, pr) {
		deny("token_signin_closed", "Token sign-in is not available for this account. Use single sign-on, or ask the owner.")
		return
	}
	if err := p.startSession(w, r, pr.ID, methodToken); err != nil {
		p.fail(w, r, err)
		return
	}
	if p.o.Failures != nil {
		p.o.Failures.RecordSignIn(r.Context(), pr.ID, "personal_access_token", "ok")
	}
	http.Redirect(w, r, p.safeNext(r.PostForm.Get("next")), http.StatusSeeOther)
}

// beginUpstream stores the login state and redirects to the provider.
func (p *Portal) beginUpstream(w http.ResponseWriter, r *http.Request, provider, purpose, sessionHash string) error {
	if p.o.SSO == nil {
		return memory.ErrNotFound
	}
	state := oauth.PortalStatePrefix + randomToken(24)
	nonce, verifier, browser := randomToken(24), randomToken(32), randomToken(24)
	target, err := p.o.SSO.PortalAuthURL(provider, state, nonce, verifier, purpose == "link")
	if err != nil {
		if errors.Is(err, oauth.ErrUnknownProvider) {
			return memory.ErrNotFound
		}
		p.log.Error("upstream login unavailable", "provider", provider, "err", err)
		return errors.New("identity provider unavailable")
	}
	if err := p.sess.putLogin(r.Context(), state, loginState{Provider: provider, Purpose: purpose, Nonce: nonce,
		Verifier: verifier, BrowserHash: hashID(browser), SessionHash: sessionHash}); err != nil {
		return err
	}
	p.setCookie(w, loginCookie, browser, callbackCookie, int(loginStateTTL.Seconds()))
	http.Redirect(w, r, target, http.StatusFound)
	return nil
}

func (p *Portal) loginSSO(w http.ResponseWriter, r *http.Request) {
	if !p.preAuthOK(r) {
		p.renderLogin(w, r, http.StatusForbidden, "The sign-in form expired. Try again.")
		return
	}
	if ok, _ := p.loginLim.Allow(p.clientKey(r)); !ok {
		p.renderLogin(w, r, http.StatusTooManyRequests, "Too many sign-in attempts. Wait a minute and try again.")
		return
	}
	if err := p.beginUpstream(w, r, r.PathValue("provider"), "login", ""); err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			p.notFound(w, r)
			return
		}
		p.renderLogin(w, r, http.StatusServiceUnavailable, "The identity provider is unavailable. Try again shortly.")
	}
}

// UpstreamCallback completes a portal upstream login (sign-in or Connect
// SSO). It is reached through the OAuth facade's single registered callback
// URL, for states that carry oauth.PortalStatePrefix.
func (p *Portal) UpstreamCallback(w http.ResponseWriter, r *http.Request, provider string) {
	p.securityHeaders(w)
	q := r.URL.Query()
	l, err := p.sess.takeLogin(r.Context(), q.Get("state"))
	ck, cerr := r.Cookie(loginCookie)
	p.setCookie(w, loginCookie, "", callbackCookie, -1)
	if err != nil || cerr != nil || l.Provider != provider || subtle.ConstantTimeCompare([]byte(hashID(ck.Value)), []byte(l.BrowserHash)) != 1 {
		p.renderLogin(w, r, http.StatusBadRequest, "That sign-in is unknown, expired, already used, or was started in another browser. Start again.")
		return
	}
	if q.Get("error") != "" {
		p.renderLogin(w, r, http.StatusForbidden, "The identity provider did not complete the sign-in.")
		return
	}
	id, err := p.o.SSO.PortalExchange(r.Context(), provider, q.Get("code"), l.Verifier, l.Nonce)
	if err != nil {
		p.log.Warn("portal upstream login failed", "provider", provider, "err", err)
		if p.o.Failures != nil {
			p.o.Failures.RecordSignIn(r.Context(), uuid.Nil, provider, oauth.SignInReason(err))
		}
		if oauth.PolicyError(err) {
			p.renderLogin(w, r, http.StatusForbidden, "Your account is not allowed to use this Team Memory deployment.")
			return
		}
		p.renderLogin(w, r, http.StatusBadGateway, "Sign-in with the identity provider failed.")
		return
	}
	if l.Purpose == "link" {
		p.finishLink(w, r, l, id)
		return
	}
	res, err := p.o.SSO.PortalResolve(r.Context(), provider, id)
	if err != nil {
		p.log.Warn("portal principal mapping failed", "provider", provider, "err", err)
		if p.o.Failures != nil {
			p.o.Failures.RecordSignIn(r.Context(), uuid.Nil, id.Issuer, oauth.SignInReason(err))
		}
		p.renderLogin(w, r, http.StatusForbidden, "Your account is not enabled for this Team Memory deployment.")
		return
	}
	if res.Principal.Kind != "human" {
		p.renderLogin(w, r, http.StatusForbidden, "Your account is not enabled for this Team Memory deployment.")
		return
	}
	if err := p.startSession(w, r, res.Principal.ID, methodSSO); err != nil {
		p.fail(w, r, err)
		return
	}
	if p.o.Failures != nil {
		p.o.Failures.RecordSignIn(r.Context(), res.Principal.ID, id.Issuer, "ok")
	}
	http.Redirect(w, r, p.base+"/", http.StatusSeeOther)
}

// finishLink attaches the freshly proven upstream identity to the principal
// of the session that started the flow. Both identities are proven now: the
// session by its cookie-bound state, the upstream by a forced re-login.
func (p *Portal) finishLink(w http.ResponseWriter, r *http.Request, l loginState, id auth.ExternalIdentity) {
	back := p.base + "/account"
	sess, err := p.sess.byHash(r.Context(), l.SessionHash, false)
	if err != nil {
		p.renderLogin(w, r, http.StatusUnauthorized, "Your portal session ended before the identity provider answered. Sign in again.")
		return
	}
	pr, err := p.o.Identities.Principal(r.Context(), sess.Principal)
	if err != nil {
		p.renderLogin(w, r, http.StatusUnauthorized, "Sign in again.")
		return
	}
	pr.AuthMethod = memory.AuthPortal
	// The login must have happened after the link was started (a provider
	// that ignored prompt=login would hand back an older sign-in).
	if !id.AuthTime.IsZero() && id.AuthTime.Before(l.Started.Add(-time.Minute)) {
		http.Redirect(w, r, back+"?err=stale", http.StatusSeeOther)
		return
	}
	err = p.o.Identities.LinkFromPortal(r.Context(), pr, id, p.o.Now())
	switch {
	case err == nil:
		http.Redirect(w, r, back+"?ok=linked", http.StatusSeeOther)
	case errors.Is(err, auth.ErrIdentityLinked):
		http.Redirect(w, r, back+"?err=linked_elsewhere", http.StatusSeeOther)
	case errors.Is(err, auth.ErrStaleLogin):
		http.Redirect(w, r, back+"?err=stale", http.StatusSeeOther)
	default:
		p.log.Error("link identity", "err", err)
		http.Redirect(w, r, back+"?err=failed", http.StatusSeeOther)
	}
}

func (p *Portal) logout(w http.ResponseWriter, r *http.Request, q *req) error {
	if err := p.sess.revoke(r.Context(), q.sess.Hash); err != nil {
		return err
	}
	p.clearSessionCookie(w)
	http.Redirect(w, r, p.base+"/login?ok=signedout", http.StatusSeeOther)
	return nil
}

var accountErrors = map[string]string{
	"linked_elsewhere": "That identity is already linked to a different Team Memory account, so nothing was changed.",
	"stale":            "Your identity provider did not confirm a fresh sign-in. Try Connect again and enter your credentials when asked.",
	"failed":           "Connecting SSO failed. Nothing was changed.",
}

type accountData struct {
	Identities []auth.LinkedIdentity
	Providers  []oauth.ProviderInfo
	Method     string
	Error      string
	Kind       string
}

func (p *Portal) account(w http.ResponseWriter, r *http.Request, q *req) error {
	ids, err := p.o.Identities.List(r.Context(), q.p.ID)
	if err != nil {
		return err
	}
	d := accountData{Identities: ids, Method: q.sess.Method, Kind: q.p.Kind, Error: accountErrors[r.URL.Query().Get("err")]}
	if p.o.SSO != nil && p.o.SSO.Ready() == nil {
		d.Providers = p.o.SSO.Providers()
	}
	return p.show(q, "account.html", "Account", d)
}

func (p *Portal) linkSSO(w http.ResponseWriter, r *http.Request, q *req) error {
	if q.p.Kind != "human" {
		return memory.ErrForbidden
	}
	if ok, _ := p.loginLim.Allow("link|" + q.sess.Hash); !ok {
		return memory.ErrRateLimited
	}
	err := p.beginUpstream(w, r, r.PathValue("provider"), "link", q.sess.Hash)
	if err != nil && !errors.Is(err, memory.ErrNotFound) {
		http.Redirect(w, r, p.base+"/account?err=failed", http.StatusSeeOther)
		return nil
	}
	return err
}
