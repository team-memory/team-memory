package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
)

func strp(s string) *string { return &s }

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	writer := tr.member(t, "Writer", "acme/platform/payments", memory.RoleWriter, false)

	m, err := tr.svc.Create(ctx, writer, memory.CreateInput{
		Scope: "acme/platform/payments", Kind: "decision", Title: "Use PostgreSQL for payments-api",
		Content: "payments-api stores ledger data in PostgreSQL.", Rationale: "ACID transactions",
		SourceURI: "https://issues.example.com/PAY-12",
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision != 1 || m.Trust != memory.TrustCited || m.Status != memory.StatusActive || m.Inherited {
		t.Fatalf("unexpected new memory: %+v", m)
	}
	if m.OriginScope != "acme/platform/payments" || m.CreatedByName != "Writer" {
		t.Fatalf("bad provenance: %+v", m)
	}

	// Update with the correct expected revision.
	m2, err := tr.svc.Update(ctx, writer, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1,
		Content: strp("payments-api stores ledger and refund data in PostgreSQL."), SourceURI: strp(""), ChangeNote: "add refunds"})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Revision != 2 || m2.Trust != memory.TrustUnverified || m2.Title != m.Title {
		t.Fatalf("unexpected revision 2: %+v", m2)
	}

	// Stale expected revision returns a typed conflict.
	_, err = tr.svc.Update(ctx, writer, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Title: strp("stale")})
	var ce *memory.ConflictError
	if !errors.As(err, &ce) || ce.CurrentRevision != 2 || ce.ExpectedRevision != 1 {
		t.Fatalf("want conflict at revision 2, got %v", err)
	}

	// Keyword search finds the current revision only.
	res, err := tr.svc.Search(ctx, writer, memory.SearchInput{Scope: "acme/platform/payments", Query: "refund"})
	if err != nil || len(res) != 1 || res[0].ID != m.ID || res[0].Revision != 2 {
		t.Fatalf("search: %v %+v", err, res)
	}
	if res, _ := tr.svc.Search(ctx, writer, memory.SearchInput{Scope: "acme/platform/payments", Query: "nonexistentword"}); len(res) != 0 {
		t.Fatalf("unexpected hits: %+v", res)
	}

	// History shows both immutable revisions and both events.
	h, err := tr.svc.History(ctx, writer, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Revisions) != 2 || h.Revisions[0].Revision != 1 || h.Revisions[1].Revision != 2 {
		t.Fatalf("history revisions: %+v", h.Revisions)
	}
	if !strings.Contains(h.Revisions[0].Content, "ledger data") || h.Revisions[0].SourceURI == "" {
		t.Fatalf("revision 1 content changed: %+v", h.Revisions[0])
	}
	if len(h.Events) != 2 || h.Events[0].Type != "created" || h.Events[1].Type != "revised" {
		t.Fatalf("history events: %+v", h.Events)
	}

	// Supersede creates a linked replacement in the same scope.
	old, repl, err := tr.svc.Supersede(ctx, writer, memory.SupersedeInput{ID: m.ID, ExpectedRevision: 2, Reason: "upgraded to 17",
		Replacement: memory.CreateInput{Title: "Use PostgreSQL 17 for payments-api", Content: "Upgraded to PostgreSQL 17."}})
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != memory.StatusSuperseded || old.SupersededBy == nil || *old.SupersededBy != repl.ID {
		t.Fatalf("supersede link: %+v", old)
	}
	if repl.Kind != memory.KindDecision || repl.OriginScope != "acme/platform/payments" {
		t.Fatalf("replacement: %+v", repl)
	}
	h, err = tr.svc.History(ctx, writer, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := h.Events[len(h.Events)-1]
	if last.Type != "superseded" || last.Snapshot["reason"] != "upgraded to 17" || last.Snapshot["replacement"] != repl.ID.String() {
		t.Fatalf("superseded event: %+v", last)
	}
	if _, err := tr.svc.Update(ctx, writer, memory.UpdateInput{ID: m.ID, ExpectedRevision: 2, Title: strp("x")}); !errors.Is(err, memory.ErrNotActive) {
		t.Fatalf("update superseded: want ErrNotActive, got %v", err)
	}
	res, _ = tr.svc.Search(ctx, writer, memory.SearchInput{Scope: "acme/platform/payments", Query: "PostgreSQL"})
	if len(res) != 1 || res[0].ID != repl.ID {
		t.Fatalf("search after supersede should return only the replacement: %+v", res)
	}

	// Archive requires maintainer and the current state version.
	if _, err := tr.svc.Archive(ctx, writer, memory.ArchiveInput{ID: repl.ID, ExpectedStateVersion: repl.StateVersion}); !errors.Is(err, memory.ErrForbidden) {
		t.Fatalf("writer archive: want forbidden, got %v", err)
	}
	if _, err := tr.svc.Archive(ctx, tr.owner, memory.ArchiveInput{ID: repl.ID, ExpectedStateVersion: repl.StateVersion + 5}); !errors.As(err, &ce) {
		t.Fatalf("archive stale state version: want conflict, got %v", err)
	}
	arch, err := tr.svc.Archive(ctx, tr.owner, memory.ArchiveInput{ID: repl.ID, ExpectedStateVersion: repl.StateVersion, Reason: "obsolete"})
	if err != nil || arch.Status != memory.StatusArchived {
		t.Fatalf("archive: %v %+v", err, arch)
	}
	if rec, _ := tr.svc.Recent(ctx, writer, "acme/platform/payments", nil, 10); len(rec) != 0 {
		t.Fatalf("archived memory still in recent: %+v", rec)
	}

	// Review requests are a stage 04 contract.
	if err := tr.svc.RequestReview(ctx, tr.owner, m.ID, 2); !errors.Is(err, memory.ErrNotImplemented) {
		t.Fatalf("RequestReview: %v", err)
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	cases := []memory.CreateInput{
		{Scope: "acme", Kind: "rumor", Title: "t", Content: "c"},
		{Scope: "acme", Kind: "fact", Title: "", Content: "c"},
		{Scope: "acme", Kind: "fact", Title: "t", Content: "   "},
		{Scope: "acme", Kind: "fact", Title: "t", Content: strings.Repeat("x", memory.MaxContentLen+1)},
		{Scope: "acme", Kind: "fact", Title: "t", Content: "c", SourceURI: "javascript:alert(1)"},
		{Scope: "acme", Kind: "fact", Title: "t", Content: "c", SourceURI: "/relative/path"},
		{Scope: "acme", Kind: "fact", Title: "t", Content: "c", SourceURI: "https://user:pw@example.com/x"},
	}
	for i, in := range cases {
		var ve *memory.ValidationError
		if _, err := tr.svc.Create(ctx, tr.owner, in); !errors.As(err, &ve) {
			t.Errorf("case %d: want validation error, got %v", i, err)
		}
	}
	if _, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/missing", Kind: "fact", Title: "t", Content: "c"}); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("missing scope: %v", err)
	}
}

// Revision rows (and events, closure rows, audit rows) are append-only even
// for the table owner.
func TestRevisionsImmutable(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme", "fact", "Immutable", "original")

	stmts := map[string]string{
		"update revision":   "UPDATE memory_revisions SET content = 'tampered' WHERE memory_id = $1",
		"delete revision":   "DELETE FROM memory_revisions WHERE memory_id = $1",
		"truncate revision": "TRUNCATE memory_revisions CASCADE",
		"update event":      "UPDATE memory_events SET event_type = 'approved' WHERE memory_id = $1",
		"delete event":      "DELETE FROM memory_events WHERE memory_id = $1",
		"delete audit":      "DELETE FROM audit_events WHERE target_id = $1::text",
		"truncate events":   "TRUNCATE memory_events CASCADE",
		"truncate audit":    "TRUNCATE audit_events",
		"truncate override": "TRUNCATE revision_overrides",
		"truncate approval": "TRUNCATE approvals",
		"truncate closure":  "TRUNCATE scope_closure",
		"truncate scopes":   "TRUNCATE scopes CASCADE",
		"delete closure":    "DELETE FROM scope_closure",
	}
	for name, sql := range stmts {
		var args []any
		if strings.Contains(sql, "$1") {
			args = append(args, m.ID)
		}
		if _, err := tr.pool.Exec(ctx, sql, args...); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: want append-only rejection, got %v", name, err)
		}
	}
	h, err := tr.svc.History(ctx, tr.owner, m.ID)
	if err != nil || h.Revisions[0].Content != "original" {
		t.Fatalf("revision changed: %v %+v", err, h.Revisions)
	}
}

func TestConcurrentUpdatesOneWins(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme", "fact", "Race", "v1")
	const n = 8
	errs := make(chan error, n)
	for i := range n {
		go func() {
			_, err := tr.svc.Update(ctx, tr.owner, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: strp("v2-" + uuid.NewString()[:4] + string(rune('a'+i)))})
			errs <- err
		}()
	}
	ok, conflicts := 0, 0
	for range n {
		var ce *memory.ConflictError
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.As(err, &ce):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("want 1 success and %d conflicts, got %d and %d", n-1, ok, conflicts)
	}
}
