package security

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
	"github.com/team-memory/team-memory/internal/search"
)

// TestPortalDocumentationScreenshots runs a synthetic test drive against a
// temporary database and saves real Chromium screenshots for the user guides.
func TestPortalDocumentationScreenshots(t *testing.T) {
	if os.Getenv("TEAM_MEMORY_DOC_SCREENSHOTS") != "1" {
		t.Skip("set TEAM_MEMORY_DOC_SCREENSHOTS=1 (make docs-screenshots)")
	}
	w := newWorld(t)
	ctx := context.Background()
	// Remove adversarial fixture content from normal views; retain its history.
	for _, m := range w.mem {
		who := w.owner
		if m.OriginScope == "globex/vault" {
			who = w.globexOwner
		}
		if _, err := w.svc.Archive(ctx, who, memory.ArchiveInput{ID: m.ID, ExpectedStateVersion: m.StateVersion, Reason: "Prepare documentation demo"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range []memory.CreateInput{
		{Scope: "acme", Kind: "convention", Title: "Keep credentials out of source control", Content: "Store payment provider credentials in the deployment secret store. Use placeholders in examples and rotate any exposed credential.", Rationale: "All projects inherit this organization-wide convention."},
		{Scope: "acme/platform", Kind: "decision", Title: "Retry transient failures with backoff", Content: "Retry transient payment provider failures with exponential backoff and jitter. Bound retries with a deadline and log the final outcome.", Rationale: "Platform services need a consistent retry strategy."},
	} {
		m, err := w.svc.Create(ctx, w.owner, in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.svc.Approve(ctx, portal(w.owner), memory.ReviewInput{ID: m.ID, Revision: m.Revision, Note: "Synthetic demo guidance"}); err != nil {
			t.Fatal(err)
		}
	}

	// Create and revise the project proposal through real MCP calls. The SDK
	// test client's reported name is intentionally visible in provenance.
	cs := connect(t, w.serve(), w.token(w.agent))
	created, isErr := call(t, cs, "memory_create", map[string]any{
		"scope": "acme/platform/payments", "kind": "convention", "title": "Use idempotency keys for payment retries",
		"content":   "Send an idempotency key when creating a payment. Reuse the key when retrying the same operation.",
		"rationale": "A network timeout must not cause a duplicate charge.",
	})
	if isErr {
		t.Fatalf("create demo: %v", created)
	}
	// Resolve through the service instead of depending on the MCP envelope shape.
	results, err := w.srch.Search(ctx, w.agent, search.Input{Scope: "acme/platform/payments", Query: "idempotency", Mode: "fulltext"})
	if err != nil || len(results.Results) != 1 {
		t.Fatalf("find demo proposal: %v, %v", results, err)
	}
	id := results.Results[0].ID
	_, isErr = call(t, cs, "memory_update", map[string]any{
		"id": id.String(), "expected_revision": 1,
		"content":     "Send an idempotency key when creating a payment. Reuse the key when retrying the same operation.\n\nPersist the key with the payment attempt so a process restart also reuses it.",
		"change_note": "Clarify retry behavior after a process restart",
	})
	if isErr {
		t.Fatal("update demo proposal failed")
	}

	dir, err := filepath.Abs("../../e2e")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("npx", "--no-install", "playwright", "test", "--config", "documentation.config.ts")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PORTAL_URL="+w.servePortal(portalpkg.Config{}),
		"TOKEN_READER="+w.token(w.paymentsReader), "TOKEN_MAINT="+w.token(w.paymentsMaint), "DEMO_MEMORY_ID="+id.String())
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("documentation test drive: %v", err)
	}
	got := w.get(w.owner, id)
	if got.Trust != memory.TrustApproved || got.Revision != 2 {
		t.Fatalf("browser approval did not persist: revision %d, trust %s", got.Revision, got.Trust)
	}
}
