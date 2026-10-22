package integration

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/search"
)

// setEmbedding replaces a memory's stored vector with the fake embedding of
// other text, simulating a semantic match that shares no keywords.
func setEmbedding(t *testing.T, tr *tree, id uuid.UUID, text string) {
	t.Helper()
	v := embeddings.FakeVector(text, 768)
	if _, err := tr.db.Admin.Exec(context.Background(),
		`UPDATE memory_embeddings SET embedding = $2 WHERE memory_id = $1`, id, pgvector.NewVector(v)); err != nil {
		t.Fatal(err)
	}
}

// approve approves the memory's current revision as the owner in a browser
// review session (the trusted review service the stage 05 portal calls).
func approve(t *testing.T, tr *tree, id uuid.UUID) {
	t.Helper()
	m, err := tr.svc.Get(context.Background(), tr.owner, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.svc.Approve(context.Background(), portal(tr.owner), memory.ReviewInput{ID: id, Revision: m.Revision}); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
}

func ids(rs []search.Result) []uuid.UUID {
	out := make([]uuid.UUID, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func byID(rs []search.Result, id uuid.UUID) (search.Result, bool) {
	for _, r := range rs {
		if r.ID == id {
			return r, true
		}
	}
	return search.Result{}, false
}

func TestHybridSearchFusesRetrievers(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	p := "acme/platform/payments"
	both := tr.create(t, tr.owner, p, "decision", "Ledger database", "the ledger uses postgres with serializable isolation")
	ftsOnly := tr.create(t, tr.owner, p, "fact", "Ledger owners", "the payments team owns the ledger service")
	semantic := tr.create(t, tr.owner, p, "lesson", "Money rounding", "store amounts as integer minor units")
	tr.create(t, tr.owner, p, "fact", "Unrelated", "the office coffee machine is on floor three")

	// Before the worker runs, search is full-text only and says so.
	res, err := tr.search.Search(ctx, tr.owner, search.Input{Scope: p, Query: "ledger postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.Vector.PendingEmbeddings != 4 || res.Results[0].ID != both.ID || len(res.Results[0].Retrievers) != 1 {
		t.Fatalf("before embedding: %+v %+v", res.Meta, res.Results)
	}

	tr.drain(t)
	setEmbedding(t, tr, ftsOnly.ID, "the office coffee machine is on floor three")
	setEmbedding(t, tr, semantic.ID, "ledger postgres")
	res, err = tr.search.Search(ctx, tr.owner, search.Input{Scope: p, Query: "ledger postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta.Mode != "hybrid" || res.Meta.Vector.Status != search.VectorOK || res.Meta.Vector.PendingEmbeddings != 0 {
		t.Fatalf("meta: %+v", res.Meta)
	}
	got := ids(res.Results)
	if len(got) != 3 || got[0] != both.ID {
		t.Fatalf("the result both retrievers agree on should lead: %+v", res.Results)
	}
	want := map[uuid.UUID][]string{
		both.ID:     {search.RetrieverFullText, search.RetrieverVector},
		ftsOnly.ID:  {search.RetrieverFullText},
		semantic.ID: {search.RetrieverVector},
	}
	for id, retrievers := range want {
		r, ok := byID(res.Results, id)
		if !ok || !slices.Equal(r.Retrievers, retrievers) {
			t.Fatalf("%s: retrievers %v, want %v", r.Title, r.Retrievers, retrievers)
		}
		if r.Score <= 0 || r.Score > 1 {
			t.Fatalf("score out of range: %+v", r)
		}
		if slices.Contains(retrievers, search.RetrieverVector) != (r.VectorSimilarity != nil) {
			t.Fatalf("vector similarity reported inconsistently: %+v", r)
		}
	}
	for i := 1; i < len(res.Results); i++ {
		if res.Results[i].Score > res.Results[i-1].Score*(1+search.PrecedenceMargin) {
			t.Fatalf("ordering contradicts scores beyond the precedence margin: %+v", res.Results)
		}
	}

	// Single-retriever modes for evaluation.
	fts, _ := tr.search.Search(ctx, tr.owner, search.Input{Scope: p, Query: "ledger postgres", Mode: search.ModeFullText})
	if fts.Meta.Mode != "fulltext_only" || slices.Contains(ids(fts.Results), semantic.ID) {
		t.Fatalf("fulltext mode: %+v", fts)
	}
	vec, _ := tr.search.Search(ctx, tr.owner, search.Input{Scope: p, Query: "ledger postgres", Mode: search.ModeVector})
	if vec.Meta.Mode != "vector_only" || slices.Contains(ids(vec.Results), ftsOnly.ID) {
		t.Fatalf("vector mode: %+v", vec)
	}

	// Kind filter applies to both retrievers.
	res, _ = tr.search.Search(ctx, tr.owner, search.Input{Scope: p, Query: "ledger postgres", Kinds: []string{"lesson"}})
	if got := ids(res.Results); len(got) != 1 || got[0] != semantic.ID {
		t.Fatalf("kind filter: %+v", res.Results)
	}
}

func TestSearchProvenanceAndScopeChain(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	org := tr.create(t, tr.owner, "acme", "convention", "Logging format", "services log JSON lines")
	team := tr.create(t, tr.owner, "acme/platform", "convention", "Logging fields", "logs carry a trace id")
	proj, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "decision",
		Title: "Logging library", Content: "payments logs JSON with slog", SourceURI: "https://issues.example.com/PAY-9"})
	if err != nil {
		t.Fatal(err)
	}
	sibling := tr.create(t, tr.owner, "acme/platform/billing", "convention", "Billing logging", "billing logs JSON too")
	tr.drain(t)

	res := tr.find(t, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "logs JSON"})
	if _, ok := byID(res, sibling.ID); ok {
		t.Fatal("sibling scope memory in results")
	}
	for _, c := range []struct {
		id        uuid.UUID
		origin    string
		inherited bool
		depth     int
	}{{org.ID, "acme", true, 0}, {team.ID, "acme/platform", true, 1}, {proj.ID, "acme/platform/payments", false, 2}} {
		r, ok := byID(res, c.id)
		if !ok || r.OriginScope != c.origin || r.Inherited != c.inherited || r.Precedence != c.depth {
			t.Fatalf("provenance of %s: %+v", c.origin, r)
		}
	}
	r, _ := byID(res, proj.ID)
	if r.Trust != memory.TrustCited || r.SourceURI == "" || r.Revision != 1 || r.UpdatedAt.IsZero() || r.Content == "" {
		t.Fatalf("trust and source fields: %+v", r)
	}
	// Descendants never appear: from the org scope only org memories match.
	res = tr.find(t, tr.owner, search.Input{Scope: "acme", Query: "logs JSON"})
	if got := ids(res); len(got) != 1 || got[0] != org.ID {
		t.Fatalf("org-level search saw descendants: %+v", res)
	}
}

func TestSupersededExcludedByDefault(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	p := "acme/platform/payments"
	old := tr.create(t, tr.owner, p, "decision", "Queue choice", "payments uses RabbitMQ for refunds")
	tr.drain(t)
	_, repl, err := tr.svc.Supersede(ctx, tr.owner, memory.SupersedeInput{ID: old.ID, ExpectedRevision: 1,
		Replacement: memory.CreateInput{Title: "Queue choice", Content: "payments uses Kafka for refunds"}})
	if err != nil {
		t.Fatal(err)
	}
	tr.drain(t)
	for _, mode := range []search.Mode{search.ModeHybrid, search.ModeFullText, search.ModeVector} {
		res := tr.find(t, tr.owner, search.Input{Scope: p, Query: "payments refunds queue", Mode: mode})
		if _, ok := byID(res, old.ID); ok {
			t.Fatalf("%s: superseded memory returned by default", mode)
		}
		if _, ok := byID(res, repl.ID); !ok {
			t.Fatalf("%s: replacement missing", mode)
		}
		res = tr.find(t, tr.owner, search.Input{Scope: p, Query: "payments refunds queue", Mode: mode, IncludeSuperseded: true})
		r, ok := byID(res, old.ID)
		if !ok || r.Status != memory.StatusSuperseded {
			t.Fatalf("%s: include_superseded: %+v", mode, res)
		}
	}
	// Archived memories never come back.
	cur, _ := tr.svc.Get(ctx, tr.owner, repl.ID, "")
	if _, err := tr.svc.Archive(ctx, tr.owner, memory.ArchiveInput{ID: repl.ID, ExpectedStateVersion: cur.StateVersion}); err != nil {
		t.Fatal(err)
	}
	if res := tr.find(t, tr.owner, search.Input{Scope: p, Query: "payments refunds queue", IncludeSuperseded: true}); len(res) != 1 || res[0].ID != old.ID {
		t.Fatalf("archived memory returned: %+v", res)
	}
}

func TestEffectiveOverridesHideOnlyForDescendantReaders(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	org := tr.create(t, tr.owner, "acme", "convention", "Logging library", "use zap for structured logging")
	proj, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "convention",
		Title: "Payments logging", Content: "payments uses slog instead", Overrides: []uuid.UUID{org.ID}})
	if err != nil {
		t.Fatal(err)
	}
	tr.drain(t)
	p := search.Input{Scope: "acme/platform/payments", Query: "zap structured logging"}

	// A draft (unapproved) link never hides inherited guidance.
	if _, ok := byID(tr.find(t, tr.owner, p), org.ID); !ok {
		t.Fatal("draft override hid the inherited memory")
	}

	approve(t, tr, proj.ID)
	res, _ := tr.search.Search(ctx, tr.owner, p)
	if _, ok := byID(res.Results, org.ID); ok || res.Meta.OverridesInEffect != 1 {
		t.Fatalf("approved override should hide the overridden memory: %+v", res)
	}
	// Effectiveness does not depend on the overriding memory matching the query.
	if _, ok := byID(tr.find(t, tr.owner, search.Input{Scope: p.Scope, Query: "zap", Mode: search.ModeFullText}), org.ID); ok {
		t.Fatal("override ignored when the overriding memory does not match")
	}
	// include_overridden returns it, flagged.
	inc := p
	inc.IncludeOverridden = true
	r, ok := byID(tr.find(t, tr.owner, inc), org.ID)
	if !ok || !slices.Equal(r.OverriddenBy, []uuid.UUID{proj.ID}) {
		t.Fatalf("include_overridden: %+v", r)
	}
	// Ancestors and siblings are unaffected.
	for _, scope := range []string{"acme", "acme/platform", "acme/platform/billing"} {
		if _, ok := byID(tr.find(t, tr.owner, search.Input{Scope: scope, Query: "zap structured logging"}), org.ID); !ok {
			t.Fatalf("override leaked to %s", scope)
		}
	}
	// A new revision of the overriding memory resets approval and disables
	// the override until it is reapproved.
	if _, err := tr.svc.Update(ctx, tr.owner, memory.UpdateInput{ID: proj.ID, ExpectedRevision: 1, Content: strp("payments uses slog")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := byID(tr.find(t, tr.owner, p), org.ID); !ok {
		t.Fatal("override stayed effective after the overriding memory changed")
	}
	// A rejection recorded after the approval also disables it.
	approve(t, tr, proj.ID)
	if _, ok := byID(tr.find(t, tr.owner, p), org.ID); ok {
		t.Fatal("reapproved override not effective")
	}
	cur, err := tr.svc.Get(ctx, tr.owner, proj.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.svc.Reject(ctx, portal(tr.owner), memory.ReviewInput{ID: proj.ID, Revision: cur.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, ok := byID(tr.find(t, tr.owner, p), org.ID); !ok {
		t.Fatal("override effective after a later rejection")
	}
}

func TestConflictCandidates(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	org := tr.create(t, tr.owner, "acme", "convention", "Logging library", "use zap for structured logging in services")
	tr.drain(t)
	// Same kind, near-identical text, descendant scope: a candidate.
	proj := tr.create(t, tr.owner, "acme/platform/payments", "convention", "Logging library", "use slog for structured logging in services")
	// Different kind: not a candidate.
	tr.create(t, tr.owner, "acme/platform/payments", "fact", "Logging library", "use zap for structured logging in services")
	// Sibling scopes never meet in one reader's results: not a candidate.
	billing := tr.create(t, tr.owner, "acme/platform/billing", "convention", "Logging library", "use slog for structured logging in services")
	ix := tr.indexer(t)
	ix.ConflictThreshold = 0.6
	w := ix.Worker(jobs.Config{Name: "conflicts"})
	for {
		if n, err := w.RunOnce(ctx); err != nil || n == 0 {
			break
		}
	}
	rows, err := db.New(tr.db.Admin).ListConflictCandidates(ctx, "nomic-embed-text")
	if err != nil {
		t.Fatal(err)
	}
	pairs := map[[2]uuid.UUID]bool{}
	for _, r := range rows {
		pairs[[2]uuid.UUID{r.MemoryID, r.CandidateMemoryID}] = true
	}
	if !pairs[[2]uuid.UUID{proj.ID, org.ID}] || !pairs[[2]uuid.UUID{billing.ID, org.ID}] || len(pairs) != 2 {
		t.Fatalf("candidates (deeper memory first): %+v", rows)
	}
	// Candidates are suggestions: search results are unchanged.
	if _, ok := byID(tr.find(t, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "zap logging"}), org.ID); !ok {
		t.Fatal("conflict candidate changed search results")
	}
}

func TestHostileQueriesAreLiteral(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Windows paths", `config lives in c:\payments\conf for O'Reilly builds`)
	tr.drain(t)
	for _, q := range []string{`a & b | !c`, `foo:* bar`, `'`, `\\`, `(((`, `<->`, `the and of`, `O'Reilly`, `c:\payments\conf`, `"unterminated`, `-`, `OR`} {
		if _, err := tr.search.Search(ctx, tr.owner, search.Input{Scope: "acme/platform/payments", Query: q}); err != nil {
			t.Errorf("query %q: %v", q, err)
		}
	}
	// Operator characters are ignored rather than interpreted: "!payments"
	// does not exclude the memory, it matches it.
	if got := tr.find(t, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "!payments & !reilly", Mode: search.ModeFullText}); len(got) != 1 || got[0].ID != m.ID {
		t.Fatalf("operators were interpreted: %+v", got)
	}
}

func TestOverridesOnlyFromActiveMemoriesInTheChain(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	org := tr.create(t, tr.owner, "acme", "convention", "Logging library", "use zap for structured logging")
	payments := search.Input{Scope: "acme/platform/payments", Query: "zap structured logging"}
	billing := search.Input{Scope: "acme/platform/billing", Query: "zap structured logging"}
	visible := func(in search.Input) bool {
		t.Helper()
		_, ok := byID(tr.find(t, tr.owner, in), org.ID)
		return ok
	}

	// An approved override owned by a sibling hides nothing for payments readers.
	sib, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/billing", Kind: "convention",
		Title: "Billing logging", Content: "billing uses slog", Overrides: []uuid.UUID{org.ID}})
	if err != nil {
		t.Fatal(err)
	}
	approve(t, tr, sib.ID)
	if !visible(payments) {
		t.Fatal("sibling scope's override hid the memory for payments")
	}
	if visible(billing) {
		t.Fatal("billing's own approved override is not effective for billing")
	}

	// Archiving the overriding memory disables the override.
	cur, _ := tr.svc.Get(ctx, tr.owner, sib.ID, "")
	if _, err := tr.svc.Archive(ctx, tr.owner, memory.ArchiveInput{ID: sib.ID, ExpectedStateVersion: cur.StateVersion}); err != nil {
		t.Fatal(err)
	}
	if !visible(billing) {
		t.Fatal("archived override still hides the memory")
	}

	// Superseding the overriding memory disables it too; the replacement does
	// not inherit the approval.
	pay, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "convention",
		Title: "Payments logging", Content: "payments uses slog", Overrides: []uuid.UUID{org.ID}})
	if err != nil {
		t.Fatal(err)
	}
	approve(t, tr, pay.ID)
	if visible(payments) {
		t.Fatal("approved override not effective")
	}
	if _, _, err := tr.svc.Supersede(ctx, tr.owner, memory.SupersedeInput{ID: pay.ID, ExpectedRevision: 1,
		Replacement: memory.CreateInput{Title: "Payments logging", Content: "payments uses slog v2", Overrides: []uuid.UUID{org.ID}}}); err != nil {
		t.Fatal(err)
	}
	if !visible(payments) {
		t.Fatal("superseded override (or its unapproved replacement) still hides the memory")
	}

	// A memory from another organization cannot be named as an override target.
	other, err := tr.admin.CreateOrganization(ctx, tr.owner.ID, "globex", "Globex")
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.admin.AddMember(ctx, uuid.Nil, "globex", tr.owner.ID, memory.RoleMaintainer, true); err != nil {
		t.Fatal(err)
	}
	var ve *memory.ValidationError
	if _, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: other.Path, Kind: "convention",
		Title: "x", Content: "y", Overrides: []uuid.UUID{org.ID}}); !errors.As(err, &ve) {
		t.Fatalf("cross-organization override: %v", err)
	}
}
