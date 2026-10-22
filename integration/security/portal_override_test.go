package security

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
)

func TestPortalOverrides(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	url := w.servePortal(portalpkg.Config{})
	proj := "acme/platform/payments"

	org, err := w.svc.Create(ctx, w.owner, memory.CreateInput{Scope: "acme", Kind: "convention", Title: "Use Postgres 16", Content: "All services use Postgres 16."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Approve(ctx, portal(w.owner), memory.ReviewInput{ID: org.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	mine, err := w.svc.Create(ctx, w.paymentsWriter, memory.CreateInput{Scope: proj, Kind: "convention", Title: "Use Postgres 17 for payments", Content: "Payments runs Postgres 17."})
	if err != nil {
		t.Fatal(err)
	}
	var model string
	if err := w.db.Admin.QueryRow(ctx, `SELECT name FROM embedding_models LIMIT 1`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Admin.Exec(ctx, `INSERT INTO conflict_candidates (org_id, scope_id, memory_id, revision, candidate_scope_id, candidate_memory_id, candidate_revision, model, similarity)
		SELECT m.org_id, m.scope_id, m.id, 1, o.scope_id, o.id, 1, $3, 0.93 FROM memories m, memories o WHERE m.id = $1 AND o.id = $2`,
		mine.ID, org.ID, model); err != nil {
		t.Fatal(err)
	}
	inherited := func(b *pbrowser, scope string) resp { return b.get(pbase + "/s/" + scope + "?tab=inherited") }
	has := func(r resp, title string) bool { return strings.Contains(strings.Join(titles(r.Body), "|"), title) }

	writer := w.browser(url, w.paymentsWriter)
	page := writer.get(pbase + "/m/" + mine.ID.String())
	if !page.has("Similar memories in other scopes") || !page.has("Mark as overriding") {
		t.Fatal("the conflict is not shown with a Mark as overriding action to a writer")
	}
	if r := w.browser(url, w.paymentsReader).get(pbase + "/m/" + mine.ID.String()); r.has("Mark as overriding") {
		t.Error("a reader is offered Mark as overriding")
	}
	reader := w.browser(url, w.paymentsReader)
	if r := reader.post(pbase+"/m/"+mine.ID.String()+"/override", form(map[string]string{"revision": "1", "overridden": org.ID.String()})); r.Status != http.StatusForbidden {
		t.Errorf("reader override: %d", r.Status)
	}

	// An inherited memory nothing overrides carries no "Overridden" note.
	if r := inherited(reader, proj); r.has("Overridden at") || r.has("hidden because") {
		t.Fatal("a plain inherited memory is labeled overridden")
	}
	// Drafting the link hides nothing and shows the proposal.
	r := writer.post(pbase+"/m/"+mine.ID.String()+"/override", form(map[string]string{"revision": "1", "overridden": org.ID.String()}))
	if r.Status != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "ok=override") {
		t.Fatalf("mark override: %d %s", r.Status, r.Body)
	}
	if !has(inherited(reader, proj), "Use Postgres 16") {
		t.Error("a draft override hid the inherited memory")
	}
	if p := writer.get(pbase + "/m/" + mine.ID.String()); !p.has("Proposed override, not in effect") {
		t.Error("the proposal is not labeled")
	}

	// The maintainer approves the exact revision: now it takes effect here and below.
	maint := w.browser(url, w.paymentsMaint)
	cur := w.get(w.owner, mine.ID)
	if cur.Revision != 2 {
		t.Fatalf("revision = %d", cur.Revision)
	}
	if r := maint.post(pbase+"/m/"+mine.ID.String()+"/approve", form(revisionField(cur))); r.Status != http.StatusSeeOther {
		t.Fatalf("approve: %d", r.Status)
	}
	inh := inherited(reader, proj)
	if has(inh, "Use Postgres 16") || !inh.has("hidden because an approved memory in a lower scope overrides") {
		t.Errorf("approved override does not collapse the inherited memory: %v", titles(inh.Body))
	}
	if shown := reader.get(pbase + "/s/" + proj + "?tab=inherited&overridden=1"); !has(shown, "Use Postgres 16") || strings.Count(shown.Body, "Overridden at") != 1 {
		t.Error("the toggle does not reveal the overridden memory")
	}
	// A filter that hides the overriding memory must not resurrect the overridden one.
	if f := reader.get(pbase + "/s/" + proj + "?tab=inherited&kind=decision"); has(f, "Use Postgres 16") {
		t.Error("filtering out the overriding memory un-collapsed the overridden one")
	}
	if own := reader.get(pbase + "/s/" + proj); !own.has("Overrides 1 inherited memory here: lowest scope wins") {
		t.Error("the overriding memory lacks its note")
	}
	if !has(inherited(w.browser(url, w.billingWriter), "acme/platform/billing"), "Use Postgres 16") {
		t.Error("a sibling project lost the inherited memory")
	}
	if org := w.browser(url, w.owner).get(pbase + "/s/acme"); !has(org, "Use Postgres 16") {
		t.Error("the organization's view changed")
	}

	// An update resets approval and disables the override.
	if _, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: mine.ID, ExpectedRevision: 2, Scope: proj, Content: ptr("Payments runs Postgres 17.1.")}); err != nil {
		t.Fatal(err)
	}
	if !has(inherited(reader, proj), "Use Postgres 16") {
		t.Error("an updated override still hides the inherited memory")
	}
	// Re-approve, then archive: archival disables it too.
	cur = w.get(w.owner, mine.ID)
	maint.post(pbase+"/m/"+mine.ID.String()+"/approve", form(revisionField(cur)))
	if has(inherited(reader, proj), "Use Postgres 16") {
		t.Fatal("re-approval did not restore the override")
	}
	cur = w.get(w.owner, mine.ID)
	if r := maint.post(pbase+"/m/"+mine.ID.String()+"/archive", form(map[string]string{"state_version": i64(cur.StateVersion), "reason": "obsolete"})); r.Status != http.StatusSeeOther {
		t.Fatalf("archive: %d", r.Status)
	}
	if !has(inherited(reader, proj), "Use Postgres 16") {
		t.Error("an archived override still hides the inherited memory")
	}
}

// TestPortalScopePageLimitsAreSeparate: a busy ancestor must not push the
// scope's own memories off its page.
func TestPortalScopePageLimitsAreSeparate(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	url := w.servePortal(portalpkg.Config{})
	for i := 0; i < 110; i++ {
		if _, err := w.svc.Create(ctx, w.owner, memory.CreateInput{Scope: "acme", Kind: "fact", Title: "org note " + intStr(int64(i)), Content: "noise"}); err != nil {
			t.Fatal(err)
		}
	}
	b := w.browser(url, w.paymentsReader)
	own := b.get(pbase + "/s/acme/platform/payments")
	if got := titles(own.Body); len(got) != 1 || got[0] != titleFor("acme/platform/payments") || own.has("Showing the most recent") {
		t.Errorf("own tab: %v", got)
	}
	inh := b.get(pbase + "/s/acme/platform/payments?tab=inherited")
	if got := len(titles(inh.Body)); got != memory.MaxLimit || !inh.has("most recent 50 inherited") {
		t.Errorf("inherited tab lists %d (want exactly the limit)", got)
	}
	// The navigator indents with classes, which the CSP allows (no style attributes).
	if home := w.browser(url, w.owner).get(pbase + "/"); home.has(`style="`) || !home.has(`class="depth-`) {
		t.Errorf("navigator uses inline styles")
	}
}
