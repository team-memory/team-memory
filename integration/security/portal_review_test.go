package security

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
)

func revisionField(m memory.Memory) map[string]string {
	return map[string]string{"revision": itoa(m.Revision), "state_version": i64(m.StateVersion)}
}

func itoa(n int) string  { return intStr(int64(n)) }
func i64(n int64) string { return intStr(n) }
func intStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func form(m map[string]string) url.Values {
	out := url.Values{}
	for k, v := range m {
		out.Set(k, v)
	}
	return out
}

// proposal creates an unverified memory the way an agent would.
func (w *world) proposal(scope string, who memory.Principal, title string) memory.Memory {
	w.t.Helper()
	m, err := w.svc.Create(context.Background(), who, memory.CreateInput{Scope: scope, Kind: "convention", Title: title,
		Content: "Proposed: " + title + " (keyword quokka)"})
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func TestPortalApproveFlow(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	ctx := context.Background()
	scope := "acme/platform/payments"
	prop := w.proposal(scope, w.agent, "Use idempotency keys")

	maint := w.browser(url, w.paymentsMaint)
	queue := maint.get(pbase + "/review/" + scope)
	if queue.Status != 200 || !queue.has("Use idempotency keys") {
		t.Fatalf("queue: %d, lacks the proposal", queue.Status)
	}
	page := maint.get(pbase + "/m/" + prop.ID.String())
	if !page.has("reviewing <strong>revision 1</strong>") || !page.has("not team policy") {
		t.Fatalf("review page does not show the exact revision and the unverified warning")
	}

	// Readers and writers get no action and the server refuses them.
	for name, who := range map[string]memory.Principal{"reader": w.paymentsReader, "writer": w.paymentsWriter} {
		b := w.browser(url, who)
		p := b.get(pbase + "/m/" + prop.ID.String())
		if p.has("/approve") || p.has("Maintainer review") {
			t.Errorf("%s sees review controls", name)
		}
		for _, action := range []string{"approve", "reject", "archive"} {
			if r := b.post(pbase+"/m/"+prop.ID.String()+"/"+action, form(map[string]string{"revision": "1", "state_version": "1", "note": "x", "reason": "x"})); r.Status != http.StatusForbidden {
				t.Errorf("%s %s: %d, want 403", name, action, r.Status)
			}
		}
	}

	// A new revision after the page was loaded: the approval is refused with a
	// clear message and nothing is approved.
	newRev, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: prop.ID, ExpectedRevision: 1, Scope: scope,
		Content: ptr("Proposed: changed after you looked (quokka)")})
	if err != nil {
		t.Fatal(err)
	}
	r := maint.post(pbase+"/m/"+prop.ID.String()+"/approve", form(map[string]string{"revision": "1"}))
	if r.Status != http.StatusConflict || !r.has("Someone changed this memory") {
		t.Fatalf("stale approval: %d", r.Status)
	}
	if got, _ := w.svc.Get(ctx, w.owner, prop.ID, ""); got.Trust == memory.TrustApproved {
		t.Fatal("stale approval was applied")
	}

	// Approve the current revision; double submit is a no-op.
	for i := 0; i < 2; i++ {
		f := revisionField(newRev)
		f["note"] = "looks right"
		r = maint.post(pbase+"/m/"+prop.ID.String()+"/approve", form(f))
		if r.Status != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "ok=approved") {
			t.Fatalf("approve #%d: %d %s", i+1, r.Status, r.Body)
		}
	}
	// MCP search now shows it approved, with the approver.
	cs := connect(t, w.serve(), w.token(w.paymentsReader))
	out, isErr := call(t, cs, "memory_search", map[string]any{"query": "quokka", "scope": scope})
	if isErr {
		t.Fatalf("search: %v", out)
	}
	var found bool
	for _, x := range out["results"].([]any) {
		res := x.(map[string]any)
		if res["id"] == prop.ID.String() {
			found = true
			if res["trust"] != "approved" || res["approved_by"] != w.paymentsMaint.ID.String() {
				t.Errorf("MCP sees trust=%v approved_by=%v", res["trust"], res["approved_by"])
			}
		}
	}
	if !found {
		t.Fatal("approved memory not found through MCP")
	}
	// The audit log records it.
	entries, err := w.svc.AuditLog(ctx, portal(w.paymentsMaint), scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	var audited int
	for _, e := range entries {
		if e.Action == "memory.approve" && e.TargetID == prop.ID.String() && e.Outcome == "success" {
			audited++
		}
	}
	if audited == 0 {
		t.Errorf("no memory.approve audit event")
	}
	// Reject needs a note.
	other := w.proposal(scope, w.agent, "Another proposal")
	if r := maint.post(pbase+"/m/"+other.ID.String()+"/reject", form(map[string]string{"revision": "1"})); r.Status != http.StatusBadRequest {
		t.Errorf("reject without a note: %d", r.Status)
	}
	if r := maint.post(pbase+"/m/"+other.ID.String()+"/reject", form(map[string]string{"revision": "1", "note": "too vague"})); r.Status != http.StatusSeeOther {
		t.Errorf("reject: %d", r.Status)
	}
}

func ptr[T any](v T) *T { return &v }

// promoter is a maintainer of both payments and platform (cascading from the
// team), so one browser action can complete a promotion.
func (w *world) promoter() memory.Principal {
	return w.member("Team admin", "acme/platform", memory.RoleMaintainer, true)
}

func (w *world) approved(scope string, maker, reviewer memory.Principal, title string) memory.Memory {
	w.t.Helper()
	m := w.proposal(scope, maker, title)
	if _, err := w.svc.Approve(context.Background(), portal(reviewer), memory.ReviewInput{ID: m.ID, Revision: m.Revision}); err != nil {
		w.t.Fatal(err)
	}
	return w.get(w.owner, m.ID)
}

func (w *world) get(p memory.Principal, id uuid.UUID) memory.Memory {
	w.t.Helper()
	m, err := w.svc.Get(context.Background(), p, id, "")
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

var promoRe = regexp.MustCompile(`/portal/promotions/([0-9a-f-]{36})`)

func TestPortalPromotion(t *testing.T) {
	w := newWorld(t)
	url := w.servePortal(portalpkg.Config{})
	ctx := context.Background()
	src := "acme/platform/payments"

	t.Run("one person with both authorities promotes in one action", func(t *testing.T) {
		admin := w.promoter()
		m := w.proposal(src, w.agent, "Retry with backoff")
		b := w.browser(url, admin)
		page := b.get(pbase + "/m/" + m.ID.String())
		if !page.has("Approve and promote to") || !page.has("completes in one step") {
			t.Fatal("approve-and-promote option missing")
		}
		// No consent, no promotion.
		f := revisionField(m)
		f["promote"] = "1"
		r := b.post(pbase+"/m/"+m.ID.String()+"/approve", form(f))
		if !strings.Contains(r.Header.Get("Location"), "err=consent") {
			t.Fatalf("promotion without consent: %s", r.Header.Get("Location"))
		}
		if got := w.get(w.owner, m.ID); got.Status != memory.StatusActive || got.Trust != memory.TrustApproved {
			t.Fatalf("after approve-only: %+v", got)
		}
		// Explicit consent completes it.
		m = w.get(w.owner, m.ID)
		f = revisionField(m)
		f["consent"] = "1"
		r = b.post(pbase+"/m/"+m.ID.String()+"/promote", form(f))
		if r.Status != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "ok=promoted") {
			t.Fatalf("promote: %d %s", r.Status, r.Body)
		}
		if got := w.get(w.owner, m.ID); got.Status != memory.StatusSuperseded || got.SupersededBy == nil {
			t.Fatalf("source not superseded: %+v", got)
		}
		// Double submit lands on the same result: no error, no second copy.
		r = b.post(pbase+"/m/"+m.ID.String()+"/promote", form(f))
		if r.Status != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "ok=promoted") {
			t.Errorf("double submit: %d %s", r.Status, r.Header.Get("Location"))
		}
		if n := w.count(w.owner, `SELECT count(*) FROM memories WHERE promoted_from_memory_id = $1`, m.ID); n != 1 {
			t.Errorf("%d promoted copies after a double submit", n)
		}
		// Team reader (not a project member) reads it; project members see it inherited, once.
		team := w.browser(url, w.teamReader).get(pbase + "/s/acme/platform")
		if !strings.Contains(strings.Join(titles(team.Body), "|"), "Retry with backoff") {
			t.Errorf("team reader does not see the promoted memory: %v", titles(team.Body))
		}
		proj := w.browser(url, w.paymentsReader)
		own := proj.get(pbase + "/s/" + src)
		inh := proj.get(pbase + "/s/" + src + "?tab=inherited")
		if strings.Contains(strings.Join(titles(own.Body), "|"), "Retry with backoff") {
			t.Errorf("duplicate at the project: %v", titles(own.Body))
		}
		n := 0
		for _, x := range titles(inh.Body) {
			if x == "Retry with backoff" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("project sees the promoted memory %d times as inherited", n)
		}
		if !strings.Contains(inh.Body, "Retry with backoff") || !inh.has("Inherited · read-only") {
			t.Error("promoted memory is not labeled inherited")
		}
	})

	t.Run("approve and promote with consent is one browser action, and a repeat is harmless", func(t *testing.T) {
		admin := w.promoter()
		m := w.proposal(src, w.agent, "Timeouts everywhere")
		b := w.browser(url, admin)
		f := revisionField(m)
		f["promote"], f["consent"] = "1", "1"
		for i := 0; i < 2; i++ {
			r := b.post(pbase+"/m/"+m.ID.String()+"/approve", form(f))
			if r.Status != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "ok=promoted") {
				t.Fatalf("approve+promote #%d: %d %s", i+1, r.Status, r.Header.Get("Location"))
			}
		}
		if got := w.get(w.owner, m.ID); got.Status != memory.StatusSuperseded {
			t.Errorf("source status %s", got.Status)
		}
		if n := w.count(w.owner, `SELECT count(*) FROM memories WHERE promoted_from_memory_id = $1`, m.ID); n != 1 {
			t.Errorf("%d promoted copies", n)
		}
	})

	t.Run("a forged dest_maintainer field does not skip the parent's review", func(t *testing.T) {
		m := w.approved(src, w.agent, w.paymentsMaint, "Forged field")
		f := revisionField(m)
		f["consent"], f["dest_maintainer"] = "1", "1"
		r := w.browser(url, w.paymentsMaint).post(pbase+"/m/"+m.ID.String()+"/promote", form(f))
		if !promoRe.MatchString(r.Header.Get("Location")) || !strings.Contains(r.Header.Get("Location"), "ok=requested") {
			t.Fatalf("got %d %s", r.Status, r.Header.Get("Location"))
		}
		if got := w.get(w.owner, m.ID); got.Status != memory.StatusActive {
			t.Errorf("source promoted without destination authority: %s", got.Status)
		}
	})

	t.Run("membership counts are shown only to those who may list members", func(t *testing.T) {
		m := w.approved(src, w.agent, w.paymentsMaint, "Counts")
		f := revisionField(m)
		f["consent"] = "1"
		id := promoRe.FindStringSubmatch(w.browser(url, w.paymentsMaint).post(pbase+"/m/"+m.ID.String()+"/promote", form(f)).Header.Get("Location"))
		if id == nil {
			t.Fatal("no request")
		}
		if r := w.browser(url, w.paymentsMaint).get(pbase + "/promotions/" + id[1]); r.Status != 200 || r.has("direct member") {
			t.Errorf("source-only maintainer: %d, sees a member count=%v", r.Status, r.has("direct member"))
		}
		if r := w.browser(url, w.teamMaintainer).get(pbase + "/promotions/" + id[1]); !r.has("direct member") {
			t.Errorf("destination maintainer does not see the member count")
		}
		if r := w.browser(url, w.teamReader).get(pbase + "/promotions/" + id[1]); r.Status != http.StatusNotFound {
			t.Errorf("destination reader opened a request: %d", r.Status)
		}
	})

	t.Run("source-only maintainer files a request the team accepts", func(t *testing.T) {
		m := w.approved(src, w.agent, w.paymentsMaint, "Log request IDs")
		b := w.browser(url, w.paymentsMaint)
		page := b.get(pbase + "/m/" + m.ID.String())
		if !page.has("becomes a request in their queue") && !page.has("It becomes a request in their queue") {
			t.Fatal("request wording missing")
		}
		f := revisionField(m)
		f["consent"] = "1"
		r := b.post(pbase+"/m/"+m.ID.String()+"/promote", form(f))
		loc := r.Header.Get("Location")
		id := promoRe.FindStringSubmatch(loc)
		if r.Status != http.StatusSeeOther || id == nil {
			t.Fatalf("request: %d %s", r.Status, loc)
		}
		// The submitter can read the source, so still active.
		if got := w.get(w.owner, m.ID); got.Status != memory.StatusActive {
			t.Fatalf("source changed before acceptance: %s", got.Status)
		}
		// The team's queue lists it; a destination-only reviewer sees the snapshot
		// but no way into the source.
		tm := w.browser(url, w.teamMaintainer)
		q := tm.get(pbase + "/review/acme/platform")
		if !q.has("Log request IDs") {
			t.Fatal("request not in the team queue")
		}
		pp := tm.get(pbase + "/promotions/" + id[1])
		if pp.Status != 200 || !pp.has("Log request IDs") || pp.has(m.ID.String()) || pp.has("/m/"+m.ID.String()) {
			t.Fatalf("destination view: %d, leaks source id=%v", pp.Status, pp.has(m.ID.String()))
		}
		if r := tm.get(pbase + "/m/" + m.ID.String()); r.Status != 404 {
			t.Errorf("destination reviewer reads source memory: %d", r.Status)
		}
		// Accept, twice.
		for i := 0; i < 2; i++ {
			r := tm.post(pbase+"/promotions/"+id[1]+"/accept", nil)
			if r.Status != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "ok=accepted") {
				t.Fatalf("accept #%d: %d %s", i+1, r.Status, r.Body)
			}
		}
		if got := w.get(w.owner, m.ID); got.Status != memory.StatusSuperseded {
			t.Errorf("source not superseded: %s", got.Status)
		}
		n := w.count(w.owner, `SELECT count(*) FROM memories WHERE promoted_from_memory_id = $1`, m.ID)
		if n != 1 {
			t.Errorf("%d promoted copies", n)
		}
	})

	t.Run("readers and writers have no action and the server refuses", func(t *testing.T) {
		m := w.approved(src, w.agent, w.paymentsMaint, "Use structured logs")
		for name, who := range map[string]memory.Principal{"reader": w.paymentsReader, "writer": w.paymentsWriter} {
			b := w.browser(url, who)
			if p := b.get(pbase + "/m/" + m.ID.String()); p.has("/promote") {
				t.Errorf("%s sees a promote form", name)
			}
			f := revisionField(m)
			f["consent"] = "1"
			if r := b.post(pbase+"/m/"+m.ID.String()+"/promote", form(f)); r.Status != http.StatusForbidden && r.Status != http.StatusNotFound {
				t.Errorf("%s promote: %d", name, r.Status)
			}
		}
	})

	t.Run("a stale request cannot be accepted", func(t *testing.T) {
		m := w.approved(src, w.agent, w.paymentsMaint, "Pin dependencies")
		b := w.browser(url, w.paymentsMaint)
		f := revisionField(m)
		f["consent"] = "1"
		id := promoRe.FindStringSubmatch(b.post(pbase+"/m/"+m.ID.String()+"/promote", form(f)).Header.Get("Location"))
		if id == nil {
			t.Fatal("no request")
		}
		if _, err := w.svc.Update(ctx, w.paymentsMaint, memory.UpdateInput{ID: m.ID, ExpectedRevision: m.Revision, Scope: src, Content: ptr("changed")}); err != nil {
			t.Fatal(err)
		}
		r := w.browser(url, w.teamMaintainer).post(pbase+"/promotions/"+id[1]+"/accept", nil)
		if r.Status != http.StatusConflict || !r.has("stale") {
			t.Errorf("stale accept: %d", r.Status)
		}
	})

	t.Run("revoked source consent blocks acceptance", func(t *testing.T) {
		pm := w.member("Leaving maintainer", src, memory.RoleMaintainer, false)
		m := w.approved(src, w.agent, pm, "Rotate keys quarterly")
		b := w.browser(url, pm)
		f := revisionField(m)
		f["consent"] = "1"
		id := promoRe.FindStringSubmatch(b.post(pbase+"/m/"+m.ID.String()+"/promote", form(f)).Header.Get("Location"))
		if id == nil {
			t.Fatal("no request")
		}
		if err := w.admin.RemoveMember(ctx, src, pm.ID); err != nil {
			t.Fatal(err)
		}
		r := w.browser(url, w.teamMaintainer).post(pbase+"/promotions/"+id[1]+"/accept", nil)
		if r.Status != http.StatusConflict {
			t.Errorf("accept after the submitter lost authority: %d", r.Status)
		}
	})
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
