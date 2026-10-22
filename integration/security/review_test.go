package security

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

// TestMCPCannotApprove: memory_approve requests review and never changes
// trust, whoever calls it, and no bearer-token caller (PAT or OAuth, human
// maintainer or agent) can complete a review, in Go or in SQL.
func TestMCPCannotApprove(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	url := w.serve()
	m := w.mem["acme/platform/payments"]
	for _, p := range []memory.Principal{w.owner, w.paymentsMaint, w.agent} {
		cs := connect(t, url, w.token(p))
		out, isErr := call(t, cs, "memory_approve", map[string]any{"id": m.ID.String(), "revision": 1})
		if isErr || out["approved"] != false || out["trust"] != "unverified" {
			t.Fatalf("%s memory_approve: %v", p.DisplayName, out)
		}
	}
	got, _ := w.svc.Get(ctx, w.owner, m.ID, "")
	if got.Trust != memory.TrustUnverified || got.Review != nil {
		t.Fatalf("memory_approve changed trust: %+v", got)
	}
	// Exactly one review request was recorded for the revision.
	if n := w.count(w.owner, "SELECT count(*) FROM memory_events WHERE memory_id = $1 AND event_type = 'review_requested'", m.ID); n != 1 {
		t.Fatalf("review requests: %d, want 1 (idempotent)", n)
	}
	for _, method := range []string{memory.AuthPAT, memory.AuthOAuth, ""} {
		p := w.paymentsMaint
		p.AuthMethod = method
		if _, err := w.svc.Approve(ctx, p, memory.ReviewInput{ID: m.ID, Revision: 1}); !is(err, memory.ErrBrowserReviewRequired) {
			t.Errorf("approve via %q: %v", method, err)
		}
		if _, err := w.svc.Reject(ctx, p, memory.ReviewInput{ID: m.ID, Revision: 1}); !is(err, memory.ErrBrowserReviewRequired) {
			t.Errorf("reject via %q: %v", method, err)
		}
	}
	// A service principal is never a reviewer, even with a portal session.
	if _, err := w.svc.Approve(ctx, portal(w.agent), memory.ReviewInput{ID: m.ID, Revision: 1}); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("agent approve: %v", err)
	}
	// The database refuses an agent's approval record, with or without a
	// portal session marker.
	for _, method := range []string{memory.AuthPAT, memory.AuthPortal} {
		err := w.asAppMethod(w.agent, method, func(tx pgx.Tx) error {
			ev := uuid.New()
			_, err := tx.Exec(ctx, `INSERT INTO memory_events (id, org_id, scope_id, memory_id, revision, event_type, actor_id)
				SELECT $2, org_id, scope_id, id, 1, 'approved', tm_principal() FROM memories WHERE id = $1`, m.ID, ev)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO approvals (org_id, scope_id, memory_id, revision, event_id, decision, reviewer_id)
				SELECT org_id, scope_id, id, 1, $2, 'approved', tm_principal() FROM memories WHERE id = $1`, m.ID, ev)
			return err
		})
		if err == nil {
			t.Errorf("SQL approval as agent via %s succeeded", method)
		}
	}
	err := w.asAppMethod(w.paymentsMaint, memory.AuthPAT, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO approvals (org_id, scope_id, memory_id, revision, event_id, decision, reviewer_id)
			SELECT m.org_id, m.scope_id, m.id, 1, e.id, 'approved', tm_principal()
			FROM memories m JOIN memory_events e ON e.memory_id = m.id WHERE m.id = $1 LIMIT 1`, m.ID)
		return err
	})
	if err == nil {
		t.Fatal("SQL approval by a human maintainer over a bearer token succeeded")
	}
}

// TestApprovalIsRevisionBound: approval records the exact revision, a new
// revision resets trust, a stale revision cannot be approved, and the
// review queue tracks it.
func TestApprovalIsRevisionBound(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	m := w.mem["acme/platform/payments"]
	if q, err := w.svc.ReviewQueue(ctx, w.paymentsMaint, "acme/platform/payments", 50); err != nil || len(q) != 1 || q[0].ID != m.ID {
		t.Fatalf("queue before review: %v %+v", err, q)
	}
	rev := portal(w.paymentsMaint)
	got, err := w.svc.Approve(ctx, rev, memory.ReviewInput{ID: m.ID, Revision: 1, Note: "checked against PAY-12"})
	if err != nil || got.Trust != memory.TrustApproved || got.Review == nil || got.Review.Reviewer != w.paymentsMaint.ID || got.Review.Revision != 1 {
		t.Fatalf("approve: %v %+v", err, got)
	}
	// Idempotent.
	if _, err := w.svc.Approve(ctx, rev, memory.ReviewInput{ID: m.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if n := w.count(w.owner, "SELECT count(*) FROM approvals WHERE memory_id = $1", m.ID); n != 1 {
		t.Fatalf("approvals: %d", n)
	}
	if q, _ := w.svc.ReviewQueue(ctx, w.paymentsMaint, "acme/platform/payments", 50); len(q) != 0 {
		t.Fatalf("approved memory still queued: %+v", q)
	}
	// Search reports trust and the approver.
	res, err := w.srch.Search(ctx, w.paymentsReader, search.Input{Scope: "acme/platform/payments", Query: "zebracorn"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range res.Results {
		if r.ID == m.ID {
			found = r.Trust == memory.TrustApproved && r.ApprovedBy != nil && *r.ApprovedBy == w.paymentsMaint.ID
		}
	}
	if !found {
		t.Fatalf("search lost approval info: %+v", res.Results)
	}
	// A writer's new revision resets trust and re-enters the queue.
	upd, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: strp("changed")})
	if err != nil || upd.Trust != memory.TrustUnverified || upd.Review != nil {
		t.Fatalf("update after approval: %v %+v", err, upd)
	}
	if q, _ := w.svc.ReviewQueue(ctx, w.paymentsMaint, "acme/platform/payments", 50); len(q) != 1 || q[0].Revision != 2 {
		t.Fatalf("new revision not queued: %+v", q)
	}
	// Approving the revision the reviewer saw (1) is a conflict now.
	var ce *memory.ConflictError
	if _, err := w.svc.Approve(ctx, rev, memory.ReviewInput{ID: m.ID, Revision: 1}); !asConflict(err, &ce) {
		t.Fatalf("stale approval: %v", err)
	}
	// Rejection is revision-bound, keeps the memory visible and unapproved,
	// and leaves the queue.
	rej, err := w.svc.Reject(ctx, rev, memory.ReviewInput{ID: m.ID, Revision: 2, Note: "unsupported"})
	if err != nil || rej.Trust != memory.TrustUnverified || rej.Review == nil || rej.Review.Decision != "rejected" {
		t.Fatalf("reject: %v %+v", err, rej)
	}
	if q, _ := w.svc.ReviewQueue(ctx, w.paymentsMaint, "acme/platform/payments", 50); len(q) != 0 {
		t.Fatalf("rejected revision still queued: %+v", q)
	}
	// Rejecting the approved revision withdraws the approval.
	if _, err := w.svc.Approve(ctx, rev, memory.ReviewInput{ID: m.ID, Revision: 2}); err != nil {
		t.Fatal(err)
	}
	if got, err := w.svc.Reject(ctx, rev, memory.ReviewInput{ID: m.ID, Revision: 2}); err != nil || got.Trust != memory.TrustUnverified {
		t.Fatalf("withdraw approval: %v %+v", err, got)
	}
	// History keeps every revision and every outcome as events.
	h, err := w.svc.History(ctx, w.paymentsReader, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range h.Events {
		types = append(types, e.Type)
	}
	want := []string{"created", "approved", "revised", "review_rejected", "approved", "review_rejected"}
	if len(types) != len(want) {
		t.Fatalf("events %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events %v, want %v", types, want)
		}
	}
}

// TestOverrides: an override can name only a readable memory owned by a
// strict ancestor; it takes effect only with an approval of the exact
// revision carrying it; it never affects siblings or ancestors; changing
// the overriding memory disables it.
func TestOverrides(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	orgRule := w.mem["acme"]
	create := func(p memory.Principal, scope string, targets ...uuid.UUID) (memory.Memory, error) {
		return w.svc.Create(ctx, p, memory.CreateInput{Scope: scope, Kind: "decision", Title: "local rule",
			Content: "zebracorn override in " + scope, Overrides: targets})
	}
	var ve *memory.ValidationError
	for name, target := range map[string]uuid.UUID{
		"sibling project memory": w.mem["acme/platform/billing"].ID,
		"own-scope memory":       w.mem["acme/platform/payments"].ID,
		"another org's memory":   w.mem["globex/vault"].ID,
		"another team's memory":  w.mem["acme/data"].ID,
		"nonexistent memory":     uuid.New(),
	} {
		if _, err := create(w.paymentsWriter, "acme/platform/payments", target); !asValidation(err, &ve) && !is(err, memory.ErrForbidden) {
			t.Errorf("override of %s accepted: %v", name, err)
		}
	}
	// An ancestor cannot override a descendant either.
	if _, err := create(w.owner, "acme", w.mem["acme/platform/payments"].ID); err == nil {
		t.Error("org memory overrode a project memory")
	}
	// The database refuses a link to an unreadable or non-ancestor target.
	own := w.mem["acme/platform/payments"]
	for _, target := range []uuid.UUID{w.mem["acme/platform/billing"].ID, w.mem["globex/vault"].ID} {
		err := w.asApp(w.paymentsWriter, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO revision_overrides (memory_id, revision, org_id, scope_id, overridden_memory_id)
				SELECT id, 1, org_id, scope_id, $2 FROM memories WHERE id = $1`, own.ID, target)
			return err
		})
		if err == nil {
			t.Errorf("SQL override link to %s accepted", target)
		}
	}

	ov, err := create(w.paymentsWriter, "acme/platform/payments", orgRule.ID)
	if err != nil {
		t.Fatal(err)
	}
	visible := func(p memory.Principal, scope string) bool {
		res, err := w.srch.Search(ctx, p, search.Input{Scope: scope, Query: "zebracorn", Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Results {
			if r.ID == orgRule.ID {
				return true
			}
		}
		return false
	}
	if g, _ := w.svc.Get(ctx, w.paymentsReader, ov.ID, ""); g.OverrideStatus != "proposed" || !visible(w.paymentsReader, "acme/platform/payments") {
		t.Fatalf("proposed override hid inherited guidance or was not labelled: %+v", g)
	}
	if _, err := w.svc.Approve(ctx, portal(w.paymentsMaint), memory.ReviewInput{ID: ov.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if g, _ := w.svc.Get(ctx, w.paymentsReader, ov.ID, ""); g.OverrideStatus != "approved" || visible(w.paymentsReader, "acme/platform/payments") {
		t.Fatalf("approved override not effective: %+v", g)
	}
	// Siblings and ancestors are unaffected.
	if !visible(w.billingWriter, "acme/platform/billing") || !visible(w.owner, "acme/platform") || !visible(w.owner, "acme") {
		t.Fatal("override in payments affected a sibling or an ancestor")
	}
	// A new revision disables it until reapproved.
	if _, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: ov.ID, ExpectedRevision: 1, Content: strp("zebracorn changed")}); err != nil {
		t.Fatal(err)
	}
	if !visible(w.paymentsReader, "acme/platform/payments") {
		t.Fatal("override stayed effective after its memory changed")
	}
	if _, err := w.svc.Approve(ctx, portal(w.paymentsMaint), memory.ReviewInput{ID: ov.ID, Revision: 2}); err != nil {
		t.Fatal(err)
	}
	cur, _ := w.svc.Get(ctx, w.paymentsMaint, ov.ID, "")
	if _, err := w.svc.Archive(ctx, w.paymentsMaint, memory.ArchiveInput{ID: ov.ID, ExpectedStateVersion: cur.StateVersion}); err != nil {
		t.Fatal(err)
	}
	if !visible(w.paymentsReader, "acme/platform/payments") {
		t.Fatal("archived override still effective")
	}
}

func asConflict(err error, ce **memory.ConflictError) bool { return errorsAs(err, ce) }

func asValidation(err error, ve **memory.ValidationError) bool { return errorsAs(err, ve) }
