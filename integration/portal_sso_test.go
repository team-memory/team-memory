package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
)

// portalUser drives the portal in a cookie-keeping client that follows
// redirects by hand, so tests can stop at (and replay) the IdP callback.
type portalUser struct {
	t      *testing.T
	env    *ssoEnv
	c      *http.Client
	csrf   string
	callbk string // last /oauth/callback URL seen
	// stop makes do() return at the IdP callback instead of following it.
	stop bool
}

func newPortalUser(t *testing.T, env *ssoEnv) *portalUser {
	jar, _ := cookiejar.New(nil)
	return &portalUser{t: t, env: env, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// do sends a request and follows redirects. It records the callback URL.
func (u *portalUser) do(req *http.Request) (int, string, *url.URL) {
	u.t.Helper()
	for hops := 0; hops < 12; hops++ {
		resp, err := u.c.Do(req)
		if err != nil {
			u.t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if m := csrfRe.FindSubmatch(body); m != nil {
			u.csrf = string(m[1])
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc, _ := resp.Location()
			if strings.HasPrefix(loc.Path, "/oauth/callback/") {
				u.callbk = loc.String()
				if u.stop {
					return resp.StatusCode, string(body), loc
				}
			}
			req, _ = http.NewRequest(http.MethodGet, loc.String(), nil)
			continue
		}
		return resp.StatusCode, string(body), resp.Request.URL
	}
	u.t.Fatal("too many redirects")
	return 0, "", nil
}

func (u *portalUser) get(path string) (int, string, *url.URL) {
	req, _ := http.NewRequest(http.MethodGet, u.env.url+path, nil)
	return u.do(req)
}

func (u *portalUser) post(path string, form url.Values) (int, string, *url.URL) {
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", u.csrf)
	req, _ := http.NewRequest(http.MethodPost, u.env.url+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return u.do(req)
}

func (u *portalUser) signInSSO() (int, string, *url.URL) {
	u.get("/portal/login")
	return u.post("/portal/login/sso/corp", nil)
}

func (u *portalUser) signInToken(tok string) (int, string, *url.URL) {
	u.get("/portal/login")
	return u.post("/portal/login/token", url.Values{"token": {tok}})
}

func identityOwner(t *testing.T, env *ssoEnv, issuer, subject string) (string, bool) {
	t.Helper()
	var id string
	err := env.tr.db.Admin.QueryRow(context.Background(), `SELECT principal_id::text FROM principal_identities WHERE issuer = $1 AND subject = $2`, issuer, subject).Scan(&id)
	return id, err == nil
}

func TestPortalSSOSignIn(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	env.idp.loginAs(map[string]any{"sub": "u-one", "email": "one@example.com", "email_verified": true, "name": "U One"})
	u := newPortalUser(t, env)
	status, body, final := u.signInSSO()
	if status != 200 || final.Path != "/portal/" || !strings.Contains(body, "not a member of any scope") {
		t.Fatalf("SSO sign-in: %d %s\n%s", status, final, body)
	}
	if _, ok := identityOwner(t, env, env.idp.srv.URL, "u-one"); !ok {
		t.Fatal("principal was not provisioned")
	}
	// A provisioned person has no roles: the owner's scopes are invisible.
	if s, _, _ := u.get("/portal/s/acme"); s != 404 {
		t.Errorf("provisioned principal reached acme: %d", s)
	}
	// Email domain policy applies in the portal too.
	env.idp.loginAs(map[string]any{"sub": "u-bad", "email": "bad@elsewhere.com", "email_verified": true})
	s, body, _ := newPortalUser(t, env).signInSSO()
	if s != http.StatusForbidden || !strings.Contains(body, "not allowed") {
		t.Errorf("disallowed domain: %d", s)
	}
	// A replayed callback is refused and creates no session.
	replay := newPortalUser(t, env)
	replay.c.Jar = u.c.Jar
	req, _ := http.NewRequest(http.MethodGet, u.callbk, nil)
	if s, _, _ := replay.do(req); s != http.StatusBadRequest {
		t.Errorf("replayed callback: %d, want 400", s)
	}
	// So is a callback completed in another browser, while the state is still unused.
	env.idp.loginAs(map[string]any{"sub": "u-one", "email": "one@example.com", "email_verified": true})
	first := newPortalUser(t, env)
	first.stop = true
	first.signInSSO()
	if first.callbk == "" {
		t.Fatal("did not reach the callback")
	}
	other := newPortalUser(t, env)
	req, _ = http.NewRequest(http.MethodGet, first.callbk, nil)
	if s, _, _ := other.do(req); s != http.StatusBadRequest {
		t.Errorf("callback in a different browser: %d, want 400", s)
	}
	if _, body, final := other.get("/portal/"); final.Path != "/portal/login" || strings.Contains(body, "Scopes") {
		t.Errorf("the other browser got a session: %s", final)
	}
}

func TestPortalConnectSSO(t *testing.T) {
	ssoPortalConfig = portalpkg.Config{TokenSignIn: "owner"}
	t.Cleanup(func() { ssoPortalConfig = portalpkg.Config{} })
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	ctx := context.Background()
	owner := env.tr.owner
	tok, err := auth.NewPATs(env.tr.db.Admin).Issue(ctx, owner.ID, "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mem := env.tr.create(t, owner, "acme/platform/payments", "decision", "Owner memory", "history that must stay attached")

	// Team mode with the owner opt-in: only the owner, only until SSO is linked.
	u := newPortalUser(t, env)
	if s, _, _ := u.signInToken(tok.Secret); s != 200 {
		t.Fatalf("owner token sign-in with SSO configured: %d", s)
	}
	// Email alone links nothing: an upstream user with the owner's email is a new principal.
	env.idp.loginAs(map[string]any{"sub": "imposter", "email": "owner@example.com", "email_verified": true})
	im := newPortalUser(t, env)
	im.signInSSO()
	if id, _ := identityOwner(t, env, env.idp.srv.URL, "imposter"); id == owner.ID.String() {
		t.Fatal("matching email linked the identity to the owner")
	}
	if s, _, _ := im.get("/portal/s/acme"); s != 404 {
		t.Errorf("imposter reached the owner's scope: %d", s)
	}

	// Without a fresh login (no auth_time) nothing is linked.
	env.idp.loginAs(map[string]any{"sub": "owner-sub", "email": "owner@example.com", "email_verified": true})
	u.get("/portal/account")
	s, body, final := u.post("/portal/account/link/corp", nil)
	if final.Query().Get("err") != "stale" || s != 200 || !strings.Contains(body, "did not confirm a fresh sign-in") {
		t.Fatalf("link without auth_time: %d %s", s, final)
	}
	if _, ok := identityOwner(t, env, env.idp.srv.URL, "owner-sub"); ok {
		t.Fatal("stale login linked")
	}

	// Fresh login proving the upstream identity: linked to the same principal.
	env.idp.loginAs(map[string]any{"sub": "owner-sub", "email": "owner@example.com", "email_verified": true, "auth_time": time.Now().Unix()})
	u.get("/portal/account")
	s, body, final = u.post("/portal/account/link/corp", nil)
	if s != 200 || final.Query().Get("ok") != "linked" || !strings.Contains(body, "SSO connected") {
		t.Fatalf("connect SSO: %d %s\n%s", s, final, body)
	}
	if id, ok := identityOwner(t, env, env.idp.srv.URL, "owner-sub"); !ok || id != owner.ID.String() {
		t.Fatalf("identity linked to %q, want the owner %s", id, owner.ID)
	}
	// The replayed link callback does nothing.
	req, _ := http.NewRequest(http.MethodGet, u.callbk, nil)
	if s, _, _ := u.do(req); s != http.StatusBadRequest {
		t.Errorf("replayed link callback: %d", s)
	}

	// Now SSO signs in as the same principal, with ownership and history intact.
	sso := newPortalUser(t, env)
	if s, _, final := sso.signInSSO(); s != 200 || final.Path != "/portal/" {
		t.Fatalf("SSO sign-in after linking: %d %s", s, final)
	}
	if s, body, _ := sso.get("/portal/m/" + mem.ID.String()); s != 200 || !strings.Contains(body, "Owner memory") {
		t.Errorf("ownership/history lost after linking: %d", s)
	}
	if s, _, _ := sso.get("/portal/review/acme"); s != 200 {
		t.Errorf("owner is not a maintainer after SSO sign-in: %d", s)
	}
	// Token sign-in is now closed for this principal: SSO is the way in.
	if s, body, _ := newPortalUser(t, env).signInToken(tok.Secret); s != http.StatusUnauthorized || !strings.Contains(body, "single sign-on") {
		t.Errorf("token sign-in after linking: %d", s)
	}
	// The token still works for MCP (PATs are unchanged).
	if _, _, err := auth.NewPATs(env.tr.db.Admin).Authenticate(ctx, tok.Secret); err != nil {
		t.Errorf("PAT broke: %v", err)
	}

	// An identity already linked to the owner cannot be linked to another principal.
	other := env.tr.member(t, "Other maintainer", "acme/platform", memory.RoleMaintainer, false)
	otherTok, _ := auth.NewPATs(env.tr.db.Admin).Issue(ctx, other.ID, "other", time.Hour)
	if s, _, _ := newPortalUser(t, env).signInToken(otherTok.Secret); s != http.StatusUnauthorized {
		t.Fatalf("a non-owner signed in with a token in team mode: %d", s)
	}
	// A second person reaches the link flow through SSO, as a provisioned principal.
	env.idp.loginAs(map[string]any{"sub": "second", "email": "second@example.com", "email_verified": true})
	o := newPortalUser(t, env)
	if s, _, _ := o.signInSSO(); s != 200 {
		t.Fatalf("second SSO sign-in: %d", s)
	}
	o.get("/portal/account")
	env.idp.loginAs(map[string]any{"sub": "owner-sub", "email": "owner@example.com", "email_verified": true, "auth_time": time.Now().Unix()})
	s, body, final = o.post("/portal/account/link/corp", nil)
	if final.Query().Get("err") != "linked_elsewhere" || !strings.Contains(body, "already linked to a different") {
		t.Fatalf("conflicting link: %d %s", s, final)
	}
	if id, _ := identityOwner(t, env, env.idp.srv.URL, "owner-sub"); id != owner.ID.String() {
		t.Fatalf("identity moved to %s", id)
	}

	// Link requires CSRF: a forged cross-site POST starts nothing.
	o.get("/portal/account")
	form := url.Values{"csrf": {"forged"}}
	req, _ = http.NewRequest(http.MethodPost, env.url+"/portal/account/link/corp", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if s, _, _ := o.do(req); s != http.StatusForbidden {
		t.Errorf("link without CSRF token: %d", s)
	}
}

// TestPortalTokenSignInDefaultsClosedWithSSO: configuring SSO closes token
// sign-in for everyone unless the operator opts in, and a mistyped setting is
// rejected (Config.Validate) instead of leaving it open.
func TestPortalTokenSignInDefaultsClosedWithSSO(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	tok, _ := auth.NewPATs(env.tr.db.Admin).Issue(context.Background(), env.tr.owner.ID, "owner", time.Hour)
	if s, body, _ := newPortalUser(t, env).signInToken(tok.Secret); s != http.StatusUnauthorized || !strings.Contains(body, "not available") {
		t.Errorf("owner token sign-in with SSO and default settings: %d", s)
	}
}
