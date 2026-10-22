package security

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
)

// TestPortalBrowserEndToEnd drives the portal in real Chromium with
// Playwright (e2e/). It signs in, navigates down the tree, sees inherited
// memories, searches, opens history, approves a proposal, checks that the
// browser enforces the CSP, and then verifies the approval through MCP and
// the audit log. Set TEAM_MEMORY_E2E=1 (make test-e2e); it needs
// `npm ci && npx playwright install chromium` in e2e/ first.
func TestPortalBrowserEndToEnd(t *testing.T) {
	if os.Getenv("TEAM_MEMORY_E2E") == "" {
		t.Skip("set TEAM_MEMORY_E2E=1 to run the Playwright browser tests (make test-e2e)")
	}
	dir, err := filepath.Abs("../../e2e")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		t.Fatalf("e2e dependencies are not installed (cd e2e && npm ci && npx playwright install chromium): %v", err)
	}
	w := newWorld(t)
	ctx := context.Background()
	scope := "acme/platform/payments"
	prop := w.proposal(scope, w.agent, "Use idempotency keys")
	evil, err := w.svc.Create(ctx, w.owner, memory.CreateInput{Scope: "acme", Kind: "fact", Title: `<script>alert("t")</script>`,
		Content: `<img src=x onerror=alert(1)> [click](javascript:alert(2)) ![p](https://evil.example/x.png)`})
	if err != nil {
		t.Fatal(err)
	}
	// The maintainer opens the XSS memory at the organization, where they only read.
	url := w.servePortal(portalpkg.Config{})
	cmd := exec.Command("npx", "playwright", "test")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = append(os.Environ(), "PORTAL_URL="+url, "TOKEN_READER="+w.token(w.paymentsReader), "TOKEN_MAINT="+w.token(w.paymentsMaint),
		"HIDDEN_ID="+w.mem["acme/platform/billing"].ID.String(), "XSS_ID="+evil.ID.String())
	err = cmd.Run()
	t.Log("\n" + out.String())
	if err != nil {
		t.Fatalf("playwright: %v", err)
	}
	got := w.get(w.owner, prop.ID)
	if got.Trust != memory.TrustApproved {
		t.Fatalf("proposal trust = %s after the browser approval", got.Trust)
	}
	cs := connect(t, w.serve(), w.token(w.paymentsReader))
	res, isErr := call(t, cs, "memory_get", map[string]any{"id": prop.ID.String()})
	if isErr || !strings.Contains(strings.ToLower(jsonString(res)), "approved") {
		t.Errorf("MCP does not show the approval: %v", res)
	}
	entries, err := w.svc.AuditLog(ctx, portal(w.paymentsMaint), scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == "memory.approve" && e.TargetID == prop.ID.String() {
			return
		}
	}
	t.Error("no memory.approve audit event")
}
