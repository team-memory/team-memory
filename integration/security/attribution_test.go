package security

import (
	"context"
	"encoding/json"
	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClientAttributionIsolationAndHistory(t *testing.T) {
	w := newWorld(t)
	a := memory.Attribution{Channel: "mcp", ClientName: "<script>client</script>", ClientVersion: "1", AuthMethod: "pat", CredentialID: "private-token-reference", RequestID: "client-request"}
	ctx := memory.WithAttribution(context.Background(), a)
	m, err := w.svc.Create(ctx, w.paymentsWriter, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "client provenance", Content: "body"})
	if err != nil {
		t.Fatal(err)
	}
	a.ClientName = "second-client"
	a.ClientVersion = "2"
	updated, err := w.svc.Update(memory.WithAttribution(context.Background(), a), w.paymentsWriter, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: strp("updated")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CreatedVia.ClientName != m.CreatedVia.ClientName || updated.RevisedVia.ClientName != "second-client" {
		t.Fatalf("lost revision attribution: %+v", updated)
	}
	h, err := w.svc.History(ctx, w.paymentsReader, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Revisions) != 2 || h.Revisions[0].Attribution.ClientVersion != "1" || h.Revisions[1].Attribution.ClientVersion != "2" {
		t.Fatalf("history: %+v", h)
	}
	b, _ := json.Marshal(h)
	if strings.Contains(string(b), "private-token-reference") {
		t.Fatal("credential reference leaked to reader")
	}
	since := time.Now().Add(-time.Hour)
	until := time.Now().Add(time.Hour)
	f := memory.AuditFilter{Client: "second-client", Action: "memory.update", Actor: w.paymentsWriter.ID.String(), Target: m.ID.String(), Outcome: "success", Since: &since, Until: &until}
	rows, err := w.svc.FilteredAuditLog(ctx, w.paymentsMaint, "acme/platform/payments", 50, f)
	if err != nil || len(rows) != 1 {
		t.Fatalf("filtered audit: %v %+v", err, rows)
	}
	b, _ = json.Marshal(rows)
	if !strings.Contains(string(b), "private-token-reference") {
		t.Fatal("credential reference missing from audit")
	}
	f.Client = "absent"
	rows, err = w.svc.FilteredAuditLog(ctx, w.paymentsMaint, "acme/platform/payments", 50, f)
	if err != nil || len(rows) != 0 {
		t.Fatalf("filter ignored: %v %+v", err, rows)
	}
	base := w.servePortal(portalpkg.Config{})
	maint := w.browser(base, w.paymentsMaint)
	page := maint.get(pbase + "/audit/acme/platform/payments?target=" + m.ID.String())
	if page.Status != 200 || !strings.Contains(page.Body, "second-client") || strings.Contains(page.Body, "<script>client") {
		t.Fatalf("audit rendering: %d %s", page.Status, page.Body)
	}
	page = maint.get(pbase + "/audit/acme/platform/payments?client=" + url.QueryEscape("absent"))
	if page.Status != 200 || !strings.Contains(page.Body, "No matching audit events") {
		t.Fatal("UI filter failed")
	}
	page = maint.get(pbase + "/m/" + m.ID.String())
	if page.Status != 200 || !strings.Contains(page.Body, "second-client") || strings.Contains(page.Body, "<script>client") || strings.Contains(page.Body, "private-token-reference") {
		t.Fatalf("provenance rendering: %d %s", page.Status, page.Body)
	}
	for _, tc := range []struct {
		p      memory.Principal
		status int
	}{{w.paymentsReader, 403}, {w.paymentsWriter, 403}, {w.billingWriter, 404}} {
		page = w.browser(base, tc.p).get(pbase + "/audit/acme/platform/payments")
		if page.Status != tc.status {
			t.Fatalf("audit access: %d want %d", page.Status, tc.status)
		}
	}
}
