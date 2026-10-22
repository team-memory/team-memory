package security

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
	"github.com/team-memory/team-memory/internal/server"
)

const pbase = "/portal"

// servePortal runs the full server with the portal and returns its URL.
func (w *world) servePortal(cfg portalpkg.Config) string {
	w.t.Helper()
	cfg.RatePerSecond = -1
	if cfg.LoginRatePerMinute == 0 {
		cfg.LoginRatePerMinute = -1
	}
	srv := httptest.NewServer(server.New(server.Options{Pool: w.db.App, Search: w.srch, RatePerSecond: -1, Portal: cfg, Logger: testLogger()}))
	w.t.Cleanup(srv.Close)
	return srv.URL
}

// pbrowser is a minimal browser: it keeps cookies, never follows redirects
// by itself (tests read the Location), and remembers the latest CSRF token
// found in a page.
type pbrowser struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

type resp struct {
	Status int
	Body   string
	Header http.Header
}

func newPBrowser(t *testing.T, base string) *pbrowser {
	jar, _ := cookiejar.New(nil)
	return &pbrowser{t: t, base: base, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *pbrowser) do(req *http.Request) resp {
	b.t.Helper()
	r, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	body, _ := io.ReadAll(r.Body)
	if m := csrfField.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return resp{r.StatusCode, string(body), r.Header}
}

func (b *pbrowser) get(path string) resp {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.base+path, nil)
	return b.do(req)
}

// post submits a form. A "csrf" value is added from the last page unless
// the form sets one (use "-" to omit it).
func (b *pbrowser) post(path string, form url.Values, hdr ...string) resp {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	switch form.Get("csrf") {
	case "":
		form.Set("csrf", b.csrf)
	case "-":
		form.Del("csrf")
	}
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return b.do(req)
}

// login signs in with a personal access token.
func (b *pbrowser) login(token string) resp {
	b.t.Helper()
	b.get(pbase + "/login")
	return b.post(pbase+"/login/token", url.Values{"token": {token}})
}

func (w *world) browser(url string, p memory.Principal) *pbrowser {
	w.t.Helper()
	b := newPBrowser(w.t, url)
	r := b.login(w.token(p))
	if r.Status != http.StatusSeeOther {
		w.t.Fatalf("sign-in as %s: %d %s", p.DisplayName, r.Status, r.Body)
	}
	b.get(pbase + "/account") // pick up the session's CSRF token
	return b
}

var (
	cardTitle = regexp.MustCompile(`<h3><a href="/portal/m/([0-9a-f-]+)[^"]*">([^<]*)</a></h3>`)
	pathCode  = regexp.MustCompile(`<code class="path">([^<]+)</code>`)
)

// titles returns the memory titles listed on a page, sorted.
func titles(body string) []string {
	var out []string
	for _, m := range cardTitle.FindAllStringSubmatch(body, -1) {
		out = append(out, m[2])
	}
	sort.Strings(out)
	return out
}

func (r resp) has(s string) bool { return strings.Contains(r.Body, s) }

func testLogger() *slog.Logger {
	if os.Getenv("TEAM_MEMORY_E2E") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.DiscardHandler)
}
