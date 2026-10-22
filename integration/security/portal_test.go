package security

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
)

func titleFor(path string) string { return "decision for " + path }

// TestPortalHierarchyVisibility signs in as each kind of person and opens
// every scope of the fixture tree. A page exists only for scopes the person
// can read; it lists the scope's own memories and, separately, those of its
// ancestors; no descendant's memory ever appears at a higher scope.
func TestPortalHierarchyVisibility(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	all := []string{"acme", "acme/platform", "acme/platform/payments", "acme/platform/billing", "acme/data", "acme/data/warehouse", "globex", "globex/vault"}
	ancestors := func(path string) []string {
		var out []string
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			out = append(out, strings.Join(parts[:i], "/"))
		}
		return out
	}
	cases := []struct {
		name     string
		who      memory.Principal
		readable []string // scopes whose page exists
		home     []string // scopes in the navigator
	}{
		{"cascading org admin", w.owner, all[:6], all[:6]},
		{"org-only member", w.orgOnly, []string{"acme"}, []string{"acme"}},
		{"team reader", w.teamReader, []string{"acme/platform"}, []string{"acme/platform"}},
		{"team maintainer", w.teamMaintainer, []string{"acme/platform"}, []string{"acme/platform"}},
		{"project reader", w.paymentsReader, []string{"acme/platform/payments"}, []string{"acme/platform/payments"}},
		{"project writer", w.paymentsWriter, []string{"acme/platform/payments"}, []string{"acme/platform/payments"}},
		{"project maintainer", w.paymentsMaint, []string{"acme/platform/payments"}, []string{"acme/platform/payments"}},
		{"sibling project writer", w.billingWriter, []string{"acme/platform/billing"}, []string{"acme/platform/billing"}},
		{"cascading member of sibling team", w.dataCascade, []string{"acme/data", "acme/data/warehouse"}, []string{"acme/data", "acme/data/warehouse"}},
		{"nobody", w.nobody, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := w.browser(url, c.who)
			home := b.get(pbase + "/")
			var shown []string
			for _, m := range pathCode.FindAllStringSubmatch(home.Body, -1) {
				shown = append(shown, m[1])
			}
			slices.Sort(shown)
			want := slices.Clone(c.home)
			slices.Sort(want)
			if !slices.Equal(shown, want) {
				t.Errorf("navigator shows %v, want %v", shown, want)
			}
			for _, name := range []string{"payments", "billing", "warehouse", "vault", "globex"} {
				if !slices.Contains(want, "x") && !strings.Contains(strings.Join(want, " "), name) && strings.Contains(home.Body, name) {
					t.Errorf("home page mentions hidden scope %q", name)
				}
			}
			for _, sc := range all {
				own := b.get(pbase + "/s/" + sc)
				if !slices.Contains(c.readable, sc) {
					if own.Status != http.StatusNotFound {
						t.Errorf("%s: status %d, want 404", sc, own.Status)
					}
					continue
				}
				if own.Status != http.StatusOK {
					t.Errorf("%s: status %d, want 200", sc, own.Status)
					continue
				}
				if got := titles(own.Body); !slices.Equal(got, []string{titleFor(sc)}) {
					t.Errorf("%s own tab lists %v, want only its own memory", sc, got)
				}
				inh := b.get(pbase + "/s/" + sc + "?tab=inherited")
				var wantInh []string
				for _, a := range ancestors(sc) {
					wantInh = append(wantInh, titleFor(a))
				}
				slices.Sort(wantInh)
				if got := titles(inh.Body); !slices.Equal(got, wantInh) {
					t.Errorf("%s inherited tab lists %v, want %v", sc, got, wantInh)
				}
				for _, other := range all {
					if strings.HasPrefix(other, sc+"/") && (own.has(titleFor(other)) || inh.has(titleFor(other))) {
						t.Errorf("%s page shows descendant %s", sc, other)
					}
					if strings.HasPrefix(other, "globex") && strings.HasPrefix(sc, "acme") && (own.has(other) || inh.has(other)) {
						t.Errorf("%s page shows other tenant", sc)
					}
				}
				if len(ancestors(sc)) > 0 && !inh.has("Inherited · read-only") {
					t.Errorf("%s: inherited memories are not labeled", sc)
				}
			}
		})
	}
}

// strip removes the per-response noise (request ID, CSRF) from a page.
var noise = regexp.MustCompile(`Request ID: <code>[^<]*</code>|value="[A-Za-z0-9_-]{20,}"`)

func strip(s string) string { return noise.ReplaceAllString(s, "") }

// TestPortal404IsIndistinguishable: a scope or memory the person cannot see
// answers exactly like one that does not exist.
func TestPortal404IsIndistinguishable(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	b := w.browser(url, w.paymentsReader)
	ghostScope := b.get(pbase + "/s/acme/no-such-team")
	for _, hidden := range []string{"acme/platform/billing", "acme/data", "globex", "globex/vault"} {
		for _, prefix := range []string{"/s/", "/review/", "/members/"} {
			r := b.get(pbase + prefix + hidden)
			if r.Status != http.StatusNotFound || strip(r.Body) != strip(ghostScope.Body) {
				t.Errorf("%s%s: status %d, differs from a nonexistent scope", prefix, hidden, r.Status)
			}
		}
	}
	ghostMem := b.get(pbase + "/m/" + uuid.NewString())
	garbage := b.get(pbase + "/m/not-a-uuid")
	if ghostMem.Status != 404 || strip(garbage.Body) != strip(ghostMem.Body) {
		t.Errorf("garbage id differs from unknown id")
	}
	for _, hidden := range []string{"acme/platform/billing", "acme/data", "globex/vault"} {
		r := b.get(pbase + "/m/" + w.mem[hidden].ID.String())
		if r.Status != 404 || strip(r.Body) != strip(ghostMem.Body) {
			t.Errorf("hidden memory of %s: status %d, differs from unknown id", hidden, r.Status)
		}
	}
	// A visible scope with too little role is a plain 403 that names nothing.
	r := b.get(pbase + "/review/acme/platform/payments")
	if r.Status != http.StatusForbidden {
		t.Errorf("reader opening the review queue of its own scope: %d, want 403", r.Status)
	}
	// Counts and search never reveal hidden memories.
	s := b.get(pbase + "/search?scope=acme/platform/payments&q=zebracorn")
	for _, hidden := range []string{"billing", "warehouse", "vault", "acme/data"} {
		if s.has(hidden) {
			t.Errorf("search results mention hidden scope %q", hidden)
		}
	}
	if got := titles(s.Body); !slices.Equal(got, []string{titleFor("acme"), titleFor("acme/platform"), titleFor("acme/platform/payments")}) {
		t.Errorf("search found %v", got)
	}
}

func TestPortalRemovedMemberLosesAccessOnNextRequest(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	b := w.browser(url, w.teamReader)
	if r := b.get(pbase + "/s/acme/platform"); r.Status != 200 {
		t.Fatalf("before removal: %d", r.Status)
	}
	if r := b.get(pbase + "/m/" + w.mem["acme/platform"].ID.String()); r.Status != 200 {
		t.Fatalf("memory before removal: %d", r.Status)
	}
	if err := w.admin.RemoveMember(context.Background(), "acme/platform", w.teamReader.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/s/acme/platform", "/m/" + w.mem["acme/platform"].ID.String(), "/search?scope=acme/platform&q=zebracorn"} {
		if r := b.get(pbase + path); r.Status != http.StatusNotFound {
			t.Errorf("%s after removal: %d, want 404", path, r.Status)
		}
	}
	if r := b.get(pbase + "/"); !r.has("not a member of any scope") {
		t.Errorf("home after removal still lists scopes")
	}
}

// TestPortalStoredXSS stores hostile text in every field the portal renders
// and checks it never becomes markup.
func TestPortalStoredXSS(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	evilScope, err := w.admin.CreateScope(ctx, uuid.Nil, "acme", "team", "evil", `<script>alert("scope")</script>`)
	if err != nil {
		t.Fatal(err)
	}
	w.grant("acme/evil", w.owner, memory.RoleMaintainer, false)
	payload := `<img src=x onerror=alert(1)><script>alert(2)</script> [click](javascript:alert(3)) [d](data:text/html;base64,AAAA) ![pic](https://evil.example/track.png) [ok](https://example.com/ok) <a href="javascript:alert(4)">x</a> ` + "`<b>`"
	m, err := w.svc.Create(ctx, w.owner, memory.CreateInput{Scope: "acme/evil", Kind: "fact", Title: `<script>alert("title")</script>`,
		Content: payload, Rationale: payload, SourceURI: "https://example.com/a?x=<script>alert(5)</script>"})
	if err != nil {
		t.Fatal(err)
	}
	url := w.servePortal(portalpkg.Config{})
	b := w.browser(url, w.owner)
	pages := map[string]resp{
		"home":      b.get(pbase + "/"),
		"scope":     b.get(pbase + "/s/" + evilScope.Path),
		"inherited": b.get(pbase + "/s/" + evilScope.Path + "?tab=inherited"),
		"memory":    b.get(pbase + "/m/" + m.ID.String()),
		"queue":     b.get(pbase + "/review/" + evilScope.Path),
		"search":    b.get(pbase + "/search?scope=" + evilScope.Path + "&q=alert"),
		"members":   b.get(pbase + "/members/" + evilScope.Path),
	}
	bad := []string{"<script", "<img", `href="javascript`, `href="data:`, "<a href=\"javascript"}
	for name, r := range pages {
		if r.Status != 200 {
			t.Errorf("%s: status %d", name, r.Status)
			continue
		}
		low := strings.ToLower(r.Body)
		for _, s := range bad {
			if strings.Contains(low, s) {
				t.Errorf("%s: output contains %q", name, s)
			}
		}
		csp := r.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "script-src") {
			t.Errorf("%s: unexpected CSP %q", name, csp)
		}
		for h, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin"} {
			if r.Header.Get(h) != want {
				t.Errorf("%s: %s = %q", name, h, r.Header.Get(h))
			}
		}
	}
	mem := pages["memory"]
	if !mem.has("&lt;script&gt;") {
		t.Errorf("hostile text should appear escaped")
	}
	if !mem.has(`href="https://example.com/ok" rel="noopener noreferrer nofollow"`) {
		t.Errorf("a safe link should render with rel attributes")
	}
	if !mem.has("[image: pic]") || mem.has("<img") || mem.has(`src="https://evil`) {
		t.Errorf("images must not load")
	}
}

// TestPortalCSRF posts to every state-changing route with no token, a wrong
// token and a foreign origin. All are refused before anything happens.
func TestPortalCSRF(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	b := w.browser(url, w.owner)
	id := w.mem["acme/platform/payments"].ID.String()
	routes := []string{"/logout", "/account/link/corp", "/m/" + id + "/approve", "/m/" + id + "/reject", "/m/" + id + "/archive",
		"/m/" + id + "/supersede", "/m/" + id + "/promote", "/m/" + id + "/override",
		"/promotions/" + uuid.NewString() + "/accept", "/promotions/" + uuid.NewString() + "/reject", "/promotions/" + uuid.NewString() + "/cancel"}
	good := b.csrf
	for _, route := range routes {
		for name, r := range map[string]resp{
			"no token":      b.post(pbase+route, url2("csrf", "-")),
			"wrong token":   b.post(pbase+route, url2("csrf", "wrong")),
			"other origin":  b.post(pbase+route, url2("csrf", good), "Origin", "https://evil.example"),
			"cross-site":    b.post(pbase+route, url2("csrf", good), "Sec-Fetch-Site", "cross-site"),
			"null origin":   b.post(pbase+route, url2("csrf", good), "Origin", "null"),
			"other session": b.post(pbase+route, url2("csrf", w.browser(url, w.nobody).csrf)),
		} {
			if r.Status != http.StatusForbidden {
				t.Errorf("POST %s (%s): %d, want 403", route, name, r.Status)
			}
		}
	}
	// Still signed in: the rejected requests changed nothing.
	if r := b.get(pbase + "/account"); r.Status != 200 {
		t.Errorf("session lost after rejected posts: %d", r.Status)
	}
	// Sign-in forms need their own token.
	anon := newPBrowser(t, url)
	anon.get(pbase + "/login")
	for _, route := range []string{"/login/token", "/login/sso/corp"} {
		for _, origin := range []string{"https://evil.example", "null"} {
			if r := anon.post(pbase+route, url2("token", w.token(w.owner)), "Origin", origin); r.Status != http.StatusForbidden {
				t.Errorf("sign-in POST %s from origin %s: %d", route, origin, r.Status)
			}
		}
	}
	for _, route := range []string{"/login/token", "/login/sso/corp"} {
		if r := anon.post(pbase+route, url2("csrf", "-", "token", w.token(w.owner))); r.Status != http.StatusForbidden {
			t.Errorf("anonymous POST %s without token: %d", route, r.Status)
		}
	}
}

func url2(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestPortalSessions(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	ctx := context.Background()
	cookieOf := func(b *pbrowser) string {
		for _, c := range b.c.Jar.Cookies(mustURL(url + pbase + "/")) {
			if strings.Contains(c.Name, "tm_portal") && c.Name != "tm_portal_csrf" {
				return c.Value
			}
		}
		return ""
	}
	t.Run("unauthenticated requests go to sign-in", func(t *testing.T) {
		b := newPBrowser(t, url)
		r := b.get(pbase + "/s/acme")
		if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), pbase+"/login") {
			t.Errorf("got %d %q", r.Status, r.Header.Get("Location"))
		}
	})
	t.Run("a planted session id is not adopted (fixation)", func(t *testing.T) {
		b := newPBrowser(t, url)
		planted := "attacker-chosen-session-id-0123456789"
		b.c.Jar.SetCookies(mustURL(url+pbase+"/"), []*http.Cookie{{Name: "tm_portal", Value: planted, Path: pbase}})
		b.login(w.token(w.owner))
		got := cookieOf(b)
		if got == "" || got == planted {
			t.Fatalf("session id after login = %q", got)
		}
		attacker := newPBrowser(t, url)
		attacker.c.Jar.SetCookies(mustURL(url+pbase+"/"), []*http.Cookie{{Name: "tm_portal", Value: planted, Path: pbase}})
		if r := attacker.get(pbase + "/s/acme"); r.Status != http.StatusSeeOther {
			t.Errorf("planted id works after login: %d", r.Status)
		}
	})
	t.Run("logout invalidates the session on the server", func(t *testing.T) {
		b := w.browser(url, w.owner)
		old := cookieOf(b)
		if r := b.post(pbase+"/logout", nil); r.Status != http.StatusSeeOther {
			t.Fatalf("logout: %d", r.Status)
		}
		replay := newPBrowser(t, url)
		replay.c.Jar.SetCookies(mustURL(url+pbase+"/"), []*http.Cookie{{Name: "tm_portal", Value: old, Path: pbase}})
		if r := replay.get(pbase + "/s/acme"); r.Status != http.StatusSeeOther {
			t.Errorf("a copied cookie still works after logout: %d", r.Status)
		}
	})
	t.Run("idle and absolute timeouts", func(t *testing.T) {
		b := w.browser(url, w.owner)
		if _, err := w.db.Admin.Exec(ctx, `UPDATE portal_sessions SET last_seen_at = now() - interval '2 hours'`); err != nil {
			t.Fatal(err)
		}
		if r := b.get(pbase + "/s/acme"); r.Status != http.StatusSeeOther {
			t.Errorf("idle-expired session accepted: %d", r.Status)
		}
		b = w.browser(url, w.owner)
		if _, err := w.db.Admin.Exec(ctx, `UPDATE portal_sessions SET expires_at = now() - interval '1 minute'`); err != nil {
			t.Fatal(err)
		}
		if r := b.get(pbase + "/s/acme"); r.Status != http.StatusSeeOther {
			t.Errorf("absolute-expired session accepted: %d", r.Status)
		}
	})
	t.Run("sessions stay with their person", func(t *testing.T) {
		a, c := w.browser(url, w.orgOnly), w.browser(url, w.dataCascade)
		if r := a.get(pbase + "/s/acme/data"); r.Status != 404 {
			t.Errorf("org-only member reached data: %d", r.Status)
		}
		if r := c.get(pbase + "/s/acme/data"); r.Status != 200 {
			t.Errorf("data member cannot reach data: %d", r.Status)
		}
		// Person A's token in person C's request is refused.
		if r := a.post(pbase+"/logout", url2("csrf", c.csrf)); r.Status != http.StatusForbidden {
			t.Errorf("another session's CSRF token accepted: %d", r.Status)
		}
	})
	t.Run("a disabled principal's session ends", func(t *testing.T) {
		p := w.principal("Soon disabled", "human")
		w.grant("acme", p, memory.RoleReader, false)
		b := w.browser(url, p)
		if r := b.get(pbase + "/s/acme"); r.Status != 200 {
			t.Fatalf("before: %d", r.Status)
		}
		if _, err := w.db.Admin.Exec(ctx, `UPDATE principals SET disabled_at = now() WHERE id = $1`, p.ID); err != nil {
			t.Fatal(err)
		}
		if r := b.get(pbase + "/s/acme"); r.Status != http.StatusSeeOther {
			t.Errorf("disabled principal still served: %d", r.Status)
		}
	})
	t.Run("bad tokens and cookie flags", func(t *testing.T) {
		b := newPBrowser(t, url)
		b.get(pbase + "/login")
		if r := b.post(pbase+"/login/token", url2("token", "tmpat_not-a-real-token")); r.Status != http.StatusUnauthorized {
			t.Errorf("bad token: %d", r.Status)
		}
		if r := b.post(pbase+"/login/token", url2("token", w.token(w.agent))); r.Status != http.StatusUnauthorized {
			t.Errorf("service principal signed in: %d", r.Status)
		}
		r := newPBrowser(t, url)
		r.get(pbase + "/login")
		resp := r.post(pbase+"/login/token", url2("token", w.token(w.owner)))
		var sawSession bool
		for _, c := range resp.Header.Values("Set-Cookie") {
			if strings.HasPrefix(c, "tm_portal=") {
				sawSession = true
				if !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Lax") || !strings.Contains(c, "Path=/portal") {
					t.Errorf("session cookie flags: %s", c)
				}
			}
		}
		if !sawSession {
			t.Error("no session cookie set")
		}
		if strings.Contains(resp.Header.Get("Location"), "tmpat_") {
			t.Error("token appears in a URL")
		}
	})
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// memState reads a memory's lifecycle columns as the database owner.
func (w *world) memState(id uuid.UUID) [4]string {
	w.t.Helper()
	var rev, ver int64
	var trust, status string
	if err := w.db.Admin.QueryRow(context.Background(), `SELECT current_revision, state_version, trust, status FROM memories WHERE id = $1`, id).
		Scan(&rev, &ver, &trust, &status); err != nil {
		w.t.Fatal(err)
	}
	return [4]string{intStr(rev), intStr(ver), trust, status}
}

// TestPortalEndpointsHierarchy is the portal counterpart of
// TestHierarchyIsolationThroughMCP: ten attacker positions try every portal
// endpoint against every memory and scope. Whatever a position cannot read
// must answer 404 and mention nothing about it, and no action may change it.
func TestPortalEndpointsHierarchy(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	scopes := []string{"acme", "acme/platform", "acme/platform/payments", "acme/platform/billing", "acme/data", "acme/data/warehouse", "globex", "globex/vault"}
	readable := map[string][]string{
		"owner":          {"acme", "acme/platform", "acme/platform/payments", "acme/platform/billing", "acme/data", "acme/data/warehouse"},
		"orgOnly":        {"acme"},
		"teamReader":     {"acme", "acme/platform"},
		"teamMaintainer": {"acme", "acme/platform"},
		"paymentsReader": {"acme", "acme/platform", "acme/platform/payments"},
		"paymentsMaint":  {"acme", "acme/platform", "acme/platform/payments"},
		"billingWriter":  {"acme", "acme/platform", "acme/platform/billing"},
		"dataCascade":    {"acme", "acme/data", "acme/data/warehouse"},
		"globexOwner":    {"globex", "globex/vault"},
		"nobody":         nil,
	}
	people := map[string]memory.Principal{"owner": w.owner, "orgOnly": w.orgOnly, "teamReader": w.teamReader, "teamMaintainer": w.teamMaintainer,
		"paymentsReader": w.paymentsReader, "paymentsMaint": w.paymentsMaint, "billingWriter": w.billingWriter, "dataCascade": w.dataCascade,
		"globexOwner": w.globexOwner, "nobody": w.nobody}
	for name, who := range people {
		t.Run(name, func(t *testing.T) {
			b := w.browser(url, who)
			probes := 0
			for _, sc := range scopes {
				if slices.Contains(readable[name], sc) {
					continue
				}
				m, ok := w.mem[sc]
				if !ok { // globex holds no memory of its own in the fixture
					m = w.mem["globex/vault"]
				}
				before := w.memState(m.ID)
				for _, get := range []string{"/m/" + m.ID.String(), "/s/" + sc, "/s/" + sc + "?tab=inherited", "/review/" + sc, "/members/" + sc, "/search?scope=" + sc + "&q=zebracorn"} {
					r := b.get(pbase + get)
					probes++
					if r.Status != http.StatusNotFound {
						t.Errorf("GET %s: %d, want 404", get, r.Status)
					}
					if r.has(m.Title) && ok || r.has("<code>"+sc+"</code>") {
						t.Errorf("GET %s: the 404 mentions %q", get, sc)
					}
				}
				id := m.ID.String()
				for _, post := range []string{"approve", "reject", "archive", "supersede", "promote", "override"} {
					r := b.post(pbase+"/m/"+id+"/"+post, form(map[string]string{"revision": "1", "state_version": "1", "note": "x", "reason": "x",
						"consent": "1", "title": "x", "content": "x", "kind": "fact", "overridden": w.mem["acme"].ID.String()}))
					probes++
					if r.Status != http.StatusNotFound {
						t.Errorf("POST %s/%s: %d, want 404", sc, post, r.Status)
					}
				}
				if after := w.memState(m.ID); after != before {
					t.Errorf("memory of %s changed by an attack: %v -> %v", sc, before, after)
				}
			}
			for _, post := range []string{"accept", "reject", "cancel"} {
				if r := b.post(pbase+"/promotions/"+uuid.NewString()+"/"+post, nil); r.Status != http.StatusNotFound {
					t.Errorf("promotion %s on an unknown request: %d", post, r.Status)
				}
			}
			if name != "owner" && probes < 40 {
				t.Fatalf("only %d probes ran", probes)
			}
		})
	}
	// Positive control: the same endpoints answer where access exists.
	b := w.browser(url, w.paymentsMaint)
	for _, get := range []string{"/m/" + w.mem["acme/platform/payments"].ID.String(), "/s/acme/platform/payments", "/review/acme/platform/payments", "/members/acme/platform/payments"} {
		if r := b.get(pbase + get); r.Status != 200 {
			t.Fatalf("control GET %s: %d", get, r.Status)
		}
	}
}

// TestPortalRedirectTargets: ?next= can only lead inside the portal, also
// when the portal is mounted at the root.
func TestPortalRedirectTargets(t *testing.T) {
	w := newWorld(t)
	for _, base := range []string{"/", "/portal"} {
		url := w.servePortal(portalpkg.Config{BasePath: base})
		prefix := strings.TrimRight(base, "/")
		for _, next := range []string{"/\t/evil.example", "//evil.example", "/\\evil.example", "https://evil.example", "/x\r\nSet-Cookie: a=b", prefix + "/../mcp", "/\x01/evil.example"} {
			b := newPBrowser(t, url)
			b.get(prefix + "/login")
			r := b.post(prefix+"/login/token", url2("token", w.token(w.owner), "next", next))
			loc := r.Header.Get("Location")
			if r.Status != http.StatusSeeOther || loc != prefix+"/" {
				t.Errorf("base %q next %q: %d Location %q, want %q", base, next, r.Status, loc, prefix+"/")
			}
		}
		b := newPBrowser(t, url)
		b.get(prefix + "/login")
		if r := b.post(prefix+"/login/token", url2("token", w.token(w.owner), "next", prefix+"/s/acme?tab=inherited")); r.Header.Get("Location") != prefix+"/s/acme?tab=inherited" {
			t.Errorf("base %q: a legitimate next was rewritten to %q", base, r.Header.Get("Location"))
		}
	}
}

func TestPortalConfigValidation(t *testing.T) {
	for _, bad := range []portalpkg.Config{{TokenSignIn: "disabled"}, {TokenSignIn: "false"}, {BasePath: "portal"}, {BasePath: "/a b"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	for _, ok := range []portalpkg.Config{{}, {TokenSignIn: "owner"}, {BasePath: "/"}, {BasePath: "/tm/portal", TokenSignIn: "off"}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}
