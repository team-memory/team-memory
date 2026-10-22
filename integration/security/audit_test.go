package security

import (
	"context"
	"strings"
	"testing"

	"github.com/team-memory/team-memory/internal/memory"
)

// TestAuditTrail: every sensitive action leaves an audit event with the
// principal and request ID, never memory content; maintainers read only
// their scopes' events; denials are recorded.
func TestAuditTrail(t *testing.T) {
	w := newWorld(t)
	ctx := memory.WithRequestID(context.Background(), "req-audit-1")
	m, err := w.svc.Create(ctx, w.paymentsWriter, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact",
		Title: "audit me", Content: "distinctive-audit-content"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: strp("distinctive-audit-content v2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.RequestReview(ctx, w.paymentsMaint, m.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Approve(ctx, portal(w.paymentsMaint), memory.ReviewInput{ID: m.ID, Revision: 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.svc.Supersede(ctx, w.paymentsMaint, memory.SupersedeInput{ID: m.ID, ExpectedRevision: 2,
		Replacement: memory.CreateInput{Title: "audit me", Content: "distinctive-replacement-text"}}); err != nil {
		t.Fatal(err)
	}
	// Denials: a writer approving, a reader writing, an outsider probing.
	_, _ = w.svc.Approve(ctx, w.paymentsWriter, memory.ReviewInput{ID: m.ID, Revision: 2})
	_, _ = w.svc.Create(ctx, w.paymentsReader, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "t", Content: "c"})
	_, _ = w.svc.Get(ctx, w.billingWriter, m.ID, "")

	rows, err := w.db.Admin.Query(context.Background(),
		"SELECT action, outcome, coalesce(actor_id::text, ''), coalesce(request_id, ''), details::text FROM audit_events WHERE request_id = 'req-audit-1'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var action, outcome, actor, rid, details string
		if err := rows.Scan(&action, &outcome, &actor, &rid, &details); err != nil {
			t.Fatal(err)
		}
		seen[action+"/"+outcome] = true
		if actor == "" {
			t.Errorf("%s has no actor", action)
		}
		if strings.Contains(details, "distinctive-audit-content") || strings.Contains(details, "distinctive-replacement-text") {
			t.Errorf("%s audit details contain memory content: %s", action, details)
		}
	}
	for _, want := range []string{"memory.create/success", "memory.update/success", "memory.review_request/success",
		"memory.approve/success", "memory.supersede/success", "memory.approve/denied", "memory.create/denied", "memory.get/denied"} {
		if !seen[want] {
			t.Errorf("no audit event %s (saw %v)", want, seen)
		}
	}

	// Maintainers read their own scope's audit; others read nothing.
	if log, err := w.svc.AuditLog(ctx, w.paymentsMaint, "acme/platform/payments", 50); err != nil || len(log) == 0 {
		t.Fatalf("maintainer audit read: %v %d", err, len(log))
	}
	if _, err := w.svc.AuditLog(ctx, w.paymentsWriter, "acme/platform/payments", 50); !is(err, memory.ErrForbidden) {
		t.Fatalf("writer audit read: %v", err)
	}
	if _, err := w.svc.AuditLog(ctx, w.billingWriter, "acme/platform/payments", 50); !is(err, memory.ErrNotFound) {
		t.Fatalf("outsider audit read: %v", err)
	}
	if n := w.count(w.billingWriter, "SELECT count(*) FROM audit_events"); n != 0 {
		t.Fatalf("billing writer sees %d audit rows through SQL", n)
	}
}
