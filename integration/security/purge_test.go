package security

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

// TestPurgeRunbookSQL runs deploy/operations/purge-memory.sql (the
// exceptional purge in docs/operations/secret-purge.md) as the table owner
// and checks that the text is gone everywhere it is stored, references
// stay intact, the memory leaves retrieval, the purge is audited, and the
// immutability triggers are active again.
func TestPurgeRunbookSQL(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	m, err := w.svc.Create(ctx, w.paymentsWriter, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact",
		Title: "Keys", Content: "benign first revision"})
	if err != nil {
		t.Fatal(err)
	}
	const secret = "AKIAQ3EGRV7XKZ2MPLWT"
	// A reviewer pasted it into a review note (notes are not scanned).
	if _, err := w.svc.Approve(ctx, portal(w.owner), memory.ReviewInput{ID: m.ID, Revision: 1, Note: "ok " + secret}); err != nil {
		t.Fatal(err)
	}
	// A revision written before a scanner rule existed (the service rejects it today).
	for _, q := range []string{
		`INSERT INTO memory_revisions (memory_id, revision, org_id, scope_id, kind, title, content, rationale, changed_by)
		 SELECT id, 2, org_id, scope_id, kind, 'Keys ` + secret + `', 'key ` + secret + `', 'see ` + secret + `', created_by FROM memories WHERE id = $1`,
		`UPDATE memories SET current_revision = 2 WHERE id = $1`,
	} {
		if _, err := w.db.Admin.Exec(ctx, q, m.ID); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := os.ReadFile("../../deploy/operations/purge-memory.sql")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, `\`) { // psql meta-commands
			continue
		}
		lines = append(lines, l)
	}
	sql := strings.NewReplacer(":'memory_id'", "'"+m.ID.String()+"'", ":'ticket'", "'TEST-1'").Replace(strings.Join(lines, "\n"))
	if _, err := w.db.Admin.Exec(ctx, sql); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var leaks int
	if err := w.db.Admin.QueryRow(ctx, `
SELECT (SELECT count(*) FROM memory_revisions WHERE memory_id = $1 AND (title || content || rationale || change_note) LIKE '%AKIA%')
     + (SELECT count(*) FROM approvals WHERE memory_id = $1 AND note LIKE '%AKIA%')
     + (SELECT count(*) FROM memory_revisions WHERE memory_id = $1 AND search @@ to_tsquery('english', 'akiaq3egrv7xkz2mplwt'))
     + (SELECT count(*) FROM memory_embeddings WHERE memory_id = $1)`, m.ID).Scan(&leaks); err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Fatalf("%d places still hold the purged text", leaks)
	}
	var revisions, events int
	var status string
	_ = w.db.Admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM memory_revisions WHERE memory_id = $1),
	  (SELECT count(*) FROM memory_events WHERE memory_id = $1), (SELECT status FROM memories WHERE id = $1)`, m.ID).Scan(&revisions, &events, &status)
	if revisions != 2 || events < 2 || status != "archived" {
		t.Fatalf("revisions %d events %d status %s", revisions, events, status)
	}
	var audited int
	_ = w.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'memory.purge' AND target_id = $1 AND details->>'authorization' = 'TEST-1'`, m.ID.String()).Scan(&audited)
	if audited != 1 {
		t.Fatal("purge not audited")
	}
	if _, err := w.db.Admin.Exec(ctx, `UPDATE memory_revisions SET content = 'x' WHERE memory_id = $1`, m.ID); err == nil {
		t.Fatal("immutability trigger is still disabled")
	}
	if _, err := w.db.Admin.Exec(ctx, `UPDATE approvals SET note = 'x' WHERE memory_id = $1`, m.ID); err == nil {
		t.Fatal("approvals trigger is still disabled")
	}
	// Searches no longer find it.
	res, err := w.srch.Search(ctx, w.paymentsWriter, search.Input{Scope: "acme/platform/payments", Query: secret})
	if err == nil && len(res.Results) > 0 {
		t.Fatalf("search still returns the purged memory: %+v", res.Results)
	}
}
