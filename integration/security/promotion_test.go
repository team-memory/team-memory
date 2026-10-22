package security

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

// promotable returns an approved payments memory ready for promotion.
func (w *world) promotable(t *testing.T) memory.Memory {
	t.Helper()
	ctx := context.Background()
	m, err := w.svc.Create(ctx, w.paymentsWriter, memory.CreateInput{Scope: "acme/platform/payments", Kind: "convention",
		Title: "Idempotency keys", Content: "every write endpoint takes an idempotency key", Rationale: "retries",
		SourceURI: "https://issues.example.com/PAY-7"})
	if err != nil {
		t.Fatal(err)
	}
	// Revision 2 exists so the source history holds content that must not
	// reach the parent's audience.
	if _, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1,
		Rationale: strp("retries; see private incident INC-99")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: m.ID, ExpectedRevision: 2, Rationale: strp("retries")}); err != nil {
		t.Fatal(err)
	}
	if m, err = w.svc.Approve(ctx, portal(w.paymentsMaint), memory.ReviewInput{ID: m.ID, Revision: 3}); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestPromotionConsentAndVisibility walks the accepted promotion contract
// end to end and attacks every step.
func TestPromotionConsentAndVisibility(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	src := w.promotable(t)
	in := memory.PromotionInput{ID: src.ID, Revision: 3, Rationale: "applies to the whole team"}

	// Agents and bearer-token calls cannot submit, writers cannot submit,
	// and a stale or unapproved revision cannot be submitted.
	if _, err := w.svc.SubmitPromotion(ctx, w.agent, in); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("agent submit: %v", err)
	}
	if _, err := w.svc.SubmitPromotion(ctx, portal(w.agent), in); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("agent submit with a portal marker: %v", err)
	}
	if _, err := w.svc.SubmitPromotion(ctx, w.paymentsMaint, in); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("PAT submit: %v", err)
	}
	if _, err := w.svc.SubmitPromotion(ctx, portal(w.paymentsWriter), in); !is(err, memory.ErrForbidden) {
		t.Fatalf("writer submit: %v", err)
	}
	var ce *memory.ConflictError
	if _, err := w.svc.SubmitPromotion(ctx, portal(w.paymentsMaint), memory.PromotionInput{ID: src.ID, Revision: 2}); !asConflict(err, &ce) {
		t.Fatalf("stale revision submit: %v", err)
	}
	unapproved, _ := w.svc.Create(ctx, w.paymentsWriter, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "draft", Content: "draft"})
	var ve *memory.ValidationError
	if _, err := w.svc.SubmitPromotion(ctx, portal(w.paymentsMaint), memory.PromotionInput{ID: unapproved.ID, Revision: 1}); !asValidation(err, &ve) {
		t.Fatalf("unapproved submit: %v", err)
	}
	// Skipping a level is impossible in SQL too: the destination must be
	// the parent.
	err := w.asAppMethod(w.paymentsMaint, memory.AuthPortal, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO promotion_requests (org_id, source_memory_id, source_revision, source_scope_id,
			destination_scope_id, submitted_by, snapshot)
			SELECT m.org_id, m.id, 3, m.scope_id, (SELECT id FROM scopes WHERE path = 'acme'), tm_principal(), '{}'
			FROM memories m WHERE m.id = $1`, src.ID)
		return err
	})
	if err == nil {
		t.Fatal("SQL promotion request skipping a level accepted")
	}

	req, err := w.svc.SubmitPromotion(ctx, portal(w.paymentsMaint), in)
	if err != nil {
		t.Fatal(err)
	}
	if req.DestinationScope != w.scopes["acme/platform"].ID || req.Status != "pending" {
		t.Fatalf("request: %+v", req)
	}
	again, err := w.svc.SubmitPromotion(ctx, portal(w.paymentsMaint), in)
	if err != nil || again.ID != req.ID {
		t.Fatalf("double submit is not idempotent: %v %v %v", err, again.ID, req.ID)
	}

	// The destination maintainer (no grant on payments) sees the snapshot,
	// and only the snapshot.
	dest := portal(w.teamMaintainer)
	view, err := w.svc.GetPromotion(ctx, dest, req.ID)
	if err != nil || view.Snapshot.Title != "Idempotency keys" || view.Snapshot.Rationale != "retries" {
		t.Fatalf("destination view: %v %+v", err, view)
	}
	if view.SourceMemoryID != nil || view.SourceScopeID != nil {
		t.Fatalf("destination view exposes source identifiers: %+v", view)
	}
	if _, err := w.svc.Get(ctx, w.teamMaintainer, src.ID, ""); !is(err, memory.ErrNotFound) {
		t.Fatalf("destination reviewer reads the source memory: %v", err)
	}
	if _, err := w.svc.History(ctx, w.teamMaintainer, src.ID); !is(err, memory.ErrNotFound) {
		t.Fatalf("destination reviewer reads the source history: %v", err)
	}
	if _, _, err := w.svc.Supersede(ctx, w.teamMaintainer, memory.SupersedeInput{ID: w.mem["acme/platform/payments"].ID,
		ExpectedRevision: 1, Replacement: memory.CreateInput{Title: "x", Content: "y"}}); !is(err, memory.ErrNotFound) {
		t.Fatalf("destination reviewer superseded a child memory: %v", err)
	}
	if n := w.count(w.teamMaintainer, "SELECT count(*) FROM memories WHERE scope_id = $1", w.scopes["acme/platform/payments"].ID); n != 0 {
		t.Fatalf("destination reviewer sees %d child memories through SQL", n)
	}
	// Outsiders do not see the request at all.
	for _, p := range []memory.Principal{w.billingWriter, w.orgOnly, w.paymentsReader} {
		if _, err := w.svc.GetPromotion(ctx, p, req.ID); !is(err, memory.ErrNotFound) {
			t.Fatalf("%s sees the promotion request: %v", p.DisplayName, err)
		}
	}

	// Finalization: not by the source maintainer, not by an agent or a
	// bearer token, not by a writer of the destination.
	if _, err := w.svc.FinalizePromotion(ctx, portal(w.paymentsMaint), req.ID, ""); !is(err, memory.ErrForbidden) {
		t.Fatalf("source maintainer finalized: %v", err)
	}
	if _, err := w.svc.FinalizePromotion(ctx, w.teamMaintainer, req.ID, ""); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("bearer-token finalize: %v", err)
	}
	// The definer function refuses a caller without destination authority.
	err = w.asAppMethod(w.paymentsMaint, memory.AuthPortal, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT tm_promotion_finalize_source($1, $2)", req.ID, src.ID)
		return err
	})
	if err == nil {
		t.Fatal("source maintainer drove the source supersession directly")
	}

	res, err := w.svc.FinalizePromotion(ctx, dest, req.ID, "accepted for the team")
	if err != nil {
		t.Fatal(err)
	}
	if res.OriginScope != "acme/platform" || res.Trust != memory.TrustApproved || res.Revision != 1 || res.PromotedFrom != nil {
		t.Fatalf("promoted memory (provenance must be redacted for the team audience): %+v", res)
	}
	// Idempotent.
	if again, err := w.svc.FinalizePromotion(ctx, dest, req.ID, ""); err != nil || again.ID != res.ID {
		t.Fatalf("finalize twice: %v %+v", err, again)
	}
	// Atomic: the source is superseded by the new parent memory.
	old, err := w.svc.Get(ctx, w.paymentsReader, src.ID, "")
	if err != nil || old.Status != memory.StatusSuperseded || old.SupersededBy == nil || *old.SupersededBy != res.ID {
		t.Fatalf("source after promotion: %v %+v", err, old)
	}
	h, _ := w.svc.History(ctx, w.paymentsReader, src.ID)
	if last := h.Events[len(h.Events)-1]; last.Type != "superseded" || last.Snapshot["reason"] != "promotion" {
		t.Fatalf("source event: %+v", last)
	}
	// Source readers see the promoted memory's provenance; the team does not.
	if m, _ := w.svc.Get(ctx, w.paymentsReader, res.ID, ""); m.PromotedFrom == nil || *m.PromotedFrom != src.ID {
		t.Fatalf("payments reader lost provenance: %+v", m)
	}
	// The new audience reads exactly the approved snapshot, one revision,
	// and none of the source history.
	th, err := w.svc.History(ctx, w.teamReader, res.ID)
	if err != nil || len(th.Revisions) != 1 || th.Revisions[0].Rationale != "retries" {
		t.Fatalf("team history: %v %+v", err, th.Revisions)
	}
	for _, r := range th.Revisions {
		if r.Rationale != "retries" {
			t.Fatal("source history reached the team")
		}
	}
	// Nor does the new audience learn the name of a scope it cannot read:
	// a sibling project's writer inherits the memory, not payments' path.
	bh, err := w.svc.History(ctx, w.billingWriter, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(bh); strings.Contains(string(b), "payments") {
		t.Fatalf("the source scope's name reached a reader who cannot see it: %s", b)
	}
	// Search at the team now finds it; billing inherits it; the org does not.
	found := func(p memory.Principal, scope string) bool {
		out, err := w.srch.Search(ctx, p, search.Input{Scope: scope, Query: "idempotency"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range out.Results {
			if r.ID == res.ID {
				return true
			}
		}
		return false
	}
	if !found(w.teamReader, "acme/platform") || !found(w.billingWriter, "acme/platform/billing") || found(w.orgOnly, "acme") {
		t.Fatal("promoted memory audience is wrong")
	}
}

// TestPromotionStaleRejectedCancelled: requests whose source changed, whose
// submitter lost authority, or that were rejected or cancelled have no
// effect.
func TestPromotionStaleRejectedCancelled(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	dest := portal(w.teamMaintainer)
	submit := func() (memory.Memory, memory.PromotionRequest) {
		src := w.promotable(t)
		req, err := w.svc.SubmitPromotion(ctx, portal(w.paymentsMaint), memory.PromotionInput{ID: src.ID, Revision: 3})
		if err != nil {
			t.Fatal(err)
		}
		return src, req
	}
	unchanged := func(src memory.Memory) {
		t.Helper()
		m, err := w.svc.Get(ctx, w.paymentsMaint, src.ID, "")
		if err != nil || m.Status != memory.StatusActive {
			t.Fatalf("source changed by a failed promotion: %v %+v", err, m)
		}
		if n := w.count(w.teamMaintainer, "SELECT count(*) FROM memories WHERE promoted_from_memory_id = $1", src.ID); n != 0 {
			t.Fatalf("a failed promotion left %d parent memories", n)
		}
	}

	// Stale: a new source revision after submission.
	src, req := submit()
	if _, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: src.ID, ExpectedRevision: 3, Content: strp("changed")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.FinalizePromotion(ctx, dest, req.ID, ""); !is(err, memory.ErrPromotionStale) {
		t.Fatalf("stale finalize: %v", err)
	}
	if v, _ := w.svc.GetPromotion(ctx, dest, req.ID); v.Status != "stale" {
		t.Fatalf("stale request status: %s", v.Status)
	}
	unchanged(src)

	// Stale: the submitter's authority was revoked.
	src, req = submit()
	if err := w.admin.RemoveMember(ctx, "acme/platform/payments", w.paymentsMaint.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.FinalizePromotion(ctx, dest, req.ID, ""); !is(err, memory.ErrPromotionStale) {
		t.Fatalf("finalize after submitter revoked: %v", err)
	}
	w.grant("acme/platform/payments", w.paymentsMaint, memory.RoleMaintainer, false)
	unchanged(src)

	// Rejected and cancelled requests cannot be finalized.
	src, req = submit()
	if _, err := w.svc.RejectPromotion(ctx, w.teamMaintainer, req.ID); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("bearer reject: %v", err)
	}
	if _, err := w.svc.RejectPromotion(ctx, portal(w.paymentsMaint), req.ID); !is(err, memory.ErrForbidden) {
		t.Fatalf("source maintainer rejected: %v", err)
	}
	if _, err := w.svc.RejectPromotion(ctx, dest, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.FinalizePromotion(ctx, dest, req.ID, ""); !is(err, memory.ErrPromotionClosed) {
		t.Fatalf("finalize rejected: %v", err)
	}
	unchanged(src)
	// A rejected request does not block a fresh submission of the revision.
	if _, err := w.svc.SubmitPromotion(ctx, portal(w.paymentsMaint), memory.PromotionInput{ID: src.ID, Revision: 3}); err != nil {
		t.Fatalf("resubmit after rejection: %v", err)
	}

	src, req = submit()
	if _, err := w.svc.CancelPromotion(ctx, w.teamMaintainer, req.ID); !is(err, memory.ErrForbidden) {
		t.Fatalf("destination cancelled: %v", err)
	}
	if _, err := w.svc.CancelPromotion(ctx, w.paymentsMaint, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.FinalizePromotion(ctx, dest, req.ID, ""); !is(err, memory.ErrPromotionClosed) {
		t.Fatalf("finalize cancelled: %v", err)
	}
	unchanged(src)

	// The request row itself cannot be forged into accepted state.
	_, req = submit()
	err := w.asAppMethod(w.teamMaintainer, memory.AuthPortal, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE promotion_requests SET status = 'accepted', decided_by = tm_principal(), decided_at = now() WHERE id = $1", req.ID)
		return err
	})
	if err == nil {
		t.Fatal("promotion request forged to accepted")
	}
}

// TestPromoteDirect: one person with authority on both levels may submit
// and accept in one explicit browser action; an agent may not.
func TestPromoteDirect(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	src := w.promotable(t)
	if _, err := w.svc.PromoteDirect(ctx, portal(w.agent), memory.PromotionInput{ID: src.ID, Revision: 3}); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("agent promote: %v", err)
	}
	m, err := w.svc.PromoteDirect(ctx, portal(w.owner), memory.PromotionInput{ID: src.ID, Revision: 3})
	if err != nil || m.OriginScope != "acme/platform" {
		t.Fatalf("direct promotion: %v %+v", err, m)
	}
	// Promotion moves one level only: the next step needs a new request.
	if m.PromotedFrom == nil {
		t.Fatal("owner (reader of the source) should see provenance")
	}
}
