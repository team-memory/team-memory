package portal

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
)

// page is the data every template receives.
type page struct {
	Title     string
	Base      string
	CSRF      string
	User      string
	Flash     string
	FlashKind string
	RequestID string
	Data      any
	// Scope context for the search box and navigation.
	ScopePath string
}

var flashes = map[string]string{
	"approved":   "Approved. This exact revision is now trusted.",
	"rejected":   "Rejected. The memory stays visible as unapproved knowledge until it is archived.",
	"archived":   "Archived. It no longer appears in default views.",
	"superseded": "Superseded. The replacement enters the review queue.",
	"promoted":   "Promoted to the parent scope. The original is superseded and the parent memory is approved.",
	"requested":  "Promotion requested. Reviewers of the parent scope will see this exact snapshot in their queue.",
	"accepted":   "Accepted. The memory now lives in this scope and is inherited by everything below.",
	"prejected":  "Promotion request rejected. It has no effect.",
	"cancelled":  "Promotion request cancelled.",
	"override":   "Override proposed as a new revision. It takes effect only after a maintainer approves that exact revision; until then both memories are shown.",
	"linked":     "SSO connected. You can now sign in with your identity provider; your memories and roles stay with this account.",
	"signedout":  "You have been signed out.",
}

var errFlashes = map[string]string{
	"consent":        "Approved, but the promotion was not requested: you must tick the consent box to share the snapshot with the parent scope's reviewers.",
	"promote_failed": "Approved, but the promotion could not be completed (the memory or your authority changed). Try Promote again from this page.",
}

func (p *Portal) funcs() template.FuncMap {
	return template.FuncMap{
		"md":       p.render.Markdown,
		"link":     p.render.Link,
		"u":        func(parts ...string) string { return p.base + strings.Join(parts, "") },
		"esc":      url.QueryEscape,
		"scopeURL": func(sp string) string { return p.base + "/s/" + sp },
		"ago":      ago,
		"ts":       func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"short":    func(id uuid.UUID) string { return id.String()[:8] },
		"indent":   func(depth int) string { return fmt.Sprintf("%drem", depth*1) },
		"add":      func(a, b int) int { return a + b },
		"join":     strings.Join,
		"has": func(list []string, s string) bool {
			for _, x := range list {
				if x == s {
					return true
				}
			}
			return false
		},
		"nm": func(names map[uuid.UUID]string, id any) string {
			switch x := id.(type) {
			case uuid.UUID:
				return names[x]
			case *uuid.UUID:
				if x != nil {
					return names[*x]
				}
			}
			return ""
		},
		"trustLabel": trustLabel,
		"first":      func(n int, s string) string { return clip(s, n) },
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func trustLabel(t memory.Trust) string {
	switch t {
	case memory.TrustApproved:
		return "Approved by a maintainer"
	case memory.TrustCited:
		return "Cited, not verified: a source link was supplied but never checked"
	}
	return "Unverified: nobody has vouched for this"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 90*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
	return t.UTC().Format("2006-01-02")
}

func (p *Portal) parseTemplates() error {
	p.pages = map[string]*template.Template{}
	entries, err := fs.ReadDir(assets, "templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" {
			continue
		}
		t, err := template.New(name).Funcs(p.funcs()).ParseFS(assets, "templates/layout.html", "templates/"+name)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		p.pages[name] = t
	}
	return nil
}

func (p *Portal) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if path.Base(name) != name || !strings.HasSuffix(name, ".css") {
		p.notFound(w, r)
		return
	}
	b, err := assets.ReadFile("static/" + name)
	if err != nil {
		p.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// show renders a page with status 200.
func (p *Portal) show(q *req, name, title string, data any) error {
	return p.showStatus(q, http.StatusOK, name, title, data)
}

func (p *Portal) showStatus(q *req, status int, name, title string, data any) error {
	pg := page{Title: title, Base: p.base, CSRF: q.sess.CSRF, User: q.p.DisplayName, Data: data,
		RequestID: memory.RequestID(q.r.Context())}
	if code := q.r.URL.Query().Get("ok"); code != "" {
		if msg, ok := flashes[code]; ok {
			pg.Flash, pg.FlashKind = msg, "ok"
		}
	}
	if code := q.r.URL.Query().Get("err"); code != "" {
		if msg, ok := errFlashes[code]; ok {
			pg.Flash, pg.FlashKind = msg, "err"
		}
	}
	if sp := q.r.URL.Query().Get("scope"); sp != "" {
		pg.ScopePath = sp
	}
	return p.execute(q.w, status, name, pg)
}

func (p *Portal) execute(w http.ResponseWriter, status int, name string, pg page) error {
	t, ok := p.pages[name]
	if !ok {
		return fmt.Errorf("no template %s", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", pg); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
	return nil
}

// errorPage renders an error without any request-specific detail.
func (p *Portal) errorPage(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	pg := page{Title: title, Base: p.base, RequestID: memory.RequestID(r.Context()),
		Data: map[string]any{"Status": status, "Message": msg}}
	if err := p.execute(w, status, "error.html", pg); err != nil {
		http.Error(w, title, status)
	}
}

// notFound is the one page for everything the viewer cannot see: unknown
// paths, hidden scopes and hidden memory IDs are indistinguishable.
func (p *Portal) notFound(w http.ResponseWriter, r *http.Request) {
	p.errorPage(w, r, http.StatusNotFound, "Not found", "This page does not exist, or you do not have access to it.")
}

// fail maps a service error to a page. Messages never include identifiers or
// scope names from the failed lookup.
func (p *Portal) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		conflict *memory.ConflictError
		valid    *memory.ValidationError
		secret   *memory.SecretError
	)
	switch {
	case errors.Is(err, memory.ErrNotFound):
		p.notFound(w, r)
	case errors.Is(err, memory.ErrForbidden), errors.Is(err, memory.ErrInherited), errors.Is(err, memory.ErrBrowserReviewRequired):
		p.errorPage(w, r, http.StatusForbidden, "Not allowed", "Your role does not allow this action here.")
	case errors.As(err, &conflict):
		p.errorPage(w, r, http.StatusConflict, "This memory changed",
			"Someone changed this memory after you opened it, so your action was not applied. Reload the memory, review the new revision, and try again.")
	case errors.Is(err, memory.ErrPromotionStale):
		p.errorPage(w, r, http.StatusConflict, "Request is stale",
			"The source memory changed or lost its approval since this request was submitted. Resubmit it from the source scope.")
	case errors.Is(err, memory.ErrPromotionClosed):
		p.errorPage(w, r, http.StatusConflict, "Request is closed", "This promotion request was already decided or withdrawn.")
	case errors.Is(err, memory.ErrNotActive):
		p.errorPage(w, r, http.StatusConflict, "Memory is not active", "This memory is archived or superseded.")
	case errors.As(err, &secret):
		p.errorPage(w, r, http.StatusUnprocessableEntity, "Possible secret detected", secret.Error())
	case errors.As(err, &valid):
		p.errorPage(w, r, http.StatusBadRequest, "Invalid input", valid.Error())
	case errors.Is(err, memory.ErrRateLimited):
		p.errorPage(w, r, http.StatusTooManyRequests, "Slow down", "Too many requests. Wait a moment and try again.")
	default:
		p.log.Error("portal request failed", "err", err, "path", r.URL.Path, "request_id", memory.RequestID(r.Context()))
		p.errorPage(w, r, http.StatusInternalServerError, "Something went wrong", "The request failed. Quote the request ID if you report it.")
	}
}

func (p *Portal) redirect(q *req, to string, ok string) error {
	if ok != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "ok=" + ok
	}
	http.Redirect(q.w, q.r, to, http.StatusSeeOther)
	return nil
}
