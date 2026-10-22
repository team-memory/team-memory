// Package integration tests the service against a real PostgreSQL with
// pgvector. Set TEST_DATABASE_URL to a role that may create databases; each
// test gets a fresh, migrated database that is dropped afterwards.
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/testdb"
)

func newDB(t *testing.T) *pgxpool.Pool { return testdb.New(t) }

// tree is the standard fixture: acme (org) -> platform (team) -> payments,
// with a sibling project billing under the same team.
//
// Code under test runs as the database roles production uses: svc and
// search on the app role (RLS applies), the indexer on the worker role.
// admin runs operator setup on the superuser pool.
type tree struct {
	db       *testdb.DB
	pool     *pgxpool.Pool // app role
	svc      *memory.Service
	admin    *memory.Service
	search   *search.Service
	embedder *embeddings.Fake
	owner    memory.Principal
	org      memory.Scope
	team     memory.Scope
	payments memory.Scope
	billing  memory.Scope
}

func newTree(t *testing.T) *tree {
	t.Helper()
	ctx := context.Background()
	d := testdb.NewDB(t)
	admin := memory.NewService(d.Admin)
	owner := newPrincipal(t, d.Admin, "Owner")
	org, err := admin.CreateOrganization(ctx, owner.ID, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	team, err := admin.CreateScope(ctx, uuid.Nil, "acme", "team", "platform", "Platform")
	if err != nil {
		t.Fatal(err)
	}
	payments, err := admin.CreateScope(ctx, uuid.Nil, "acme/platform", "project", "payments", "")
	if err != nil {
		t.Fatal(err)
	}
	billing, err := admin.CreateScope(ctx, uuid.Nil, "acme/platform", "project", "billing", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.AddMember(ctx, uuid.Nil, "acme", owner.ID, memory.RoleMaintainer, true); err != nil {
		t.Fatal(err)
	}
	// The fake embedder stands in for the default model so the migration's
	// registry row and HNSW index are exercised as in production.
	fake := embeddings.NewFake("nomic-embed-text", 768)
	srch := search.New(d.App, search.Config{Provider: fake, MinVectorSimilarity: fakeMinSimilarity})
	return &tree{db: d, pool: d.App, svc: memory.NewService(d.App), admin: admin, search: srch, embedder: fake, owner: owner,
		org: org, team: team, payments: payments, billing: billing}
}

// portal returns p as it would act in a browser review session.
func portal(p memory.Principal) memory.Principal {
	p.AuthMethod = memory.AuthPortal
	return p
}

// fakeMinSimilarity separates texts sharing words from unrelated texts for
// the bag-of-words fake (unrelated texts score near 0 +- 0.04 at 768 dims).
const fakeMinSimilarity = 0.15

// indexer returns an embedding job handler for the tree's fake model.
func (tr *tree) indexer(t *testing.T) *embeddings.Indexer {
	t.Helper()
	ix, err := embeddings.NewIndexer(context.Background(), tr.db.Worker, tr.embedder)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// drain runs embedding jobs until none is ready.
func (tr *tree) drain(t *testing.T) {
	t.Helper()
	w := tr.indexer(t).Worker(jobs.Config{Name: "test"})
	for {
		n, err := w.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// find runs a hybrid search and fails the test on error.
func (tr *tree) find(t *testing.T, p memory.Principal, in search.Input) []search.Result {
	t.Helper()
	res, err := tr.search.Search(context.Background(), p, in)
	if err != nil {
		t.Fatalf("search %q in %s: %v", in.Query, in.Scope, err)
	}
	return res.Results
}

func newPrincipal(t *testing.T, pool *pgxpool.Pool, name string) memory.Principal {
	t.Helper()
	p, err := db.New(pool).CreatePrincipal(context.Background(), db.CreatePrincipalParams{Kind: "human", DisplayName: name})
	if err != nil {
		t.Fatal(err)
	}
	return memory.Principal{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName, AuthMethod: memory.AuthPAT}
}

// member creates a principal with one grant.
func (tr *tree) member(t *testing.T, name, scope string, role memory.Role, cascade bool) memory.Principal {
	t.Helper()
	p := newPrincipal(t, tr.db.Admin, name)
	if err := tr.admin.AddMember(context.Background(), uuid.Nil, scope, p.ID, role, cascade); err != nil {
		t.Fatal(err)
	}
	return p
}

func (tr *tree) create(t *testing.T, p memory.Principal, scope, kind, title, content string) memory.Memory {
	t.Helper()
	m, err := tr.svc.Create(context.Background(), p, memory.CreateInput{Scope: scope, Kind: kind, Title: title, Content: content})
	if err != nil {
		t.Fatalf("create %q in %s: %v", title, scope, err)
	}
	return m
}
