// Package integration tests the service against a real PostgreSQL with
// pgvector. Set TEST_DATABASE_URL to a role that may create databases; each
// test gets a fresh, migrated database that is dropped afterwards.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI")
		}
		t.Skip("TEST_DATABASE_URL not set; run `make test` to start PostgreSQL")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "tm_test_" + hex.EncodeToString(b)

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	_ = conn.Close(ctx)

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// tree is the standard fixture: acme (org) -> platform (team) -> payments,
// with a sibling project billing under the same team.
type tree struct {
	pool     *pgxpool.Pool
	svc      *memory.Service
	owner    memory.Principal
	org      memory.Scope
	team     memory.Scope
	payments memory.Scope
	billing  memory.Scope
}

func newTree(t *testing.T) *tree {
	t.Helper()
	ctx := context.Background()
	pool := newDB(t)
	svc := memory.NewService(pool)
	owner := newPrincipal(t, pool, "Owner")
	org, err := svc.CreateOrganization(ctx, owner.ID, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	team, err := svc.CreateScope(ctx, uuid.Nil, "acme", "team", "platform", "Platform")
	if err != nil {
		t.Fatal(err)
	}
	payments, err := svc.CreateScope(ctx, uuid.Nil, "acme/platform", "project", "payments", "")
	if err != nil {
		t.Fatal(err)
	}
	billing, err := svc.CreateScope(ctx, uuid.Nil, "acme/platform", "project", "billing", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(ctx, uuid.Nil, "acme", owner.ID, memory.RoleMaintainer, true); err != nil {
		t.Fatal(err)
	}
	return &tree{pool: pool, svc: svc, owner: owner, org: org, team: team, payments: payments, billing: billing}
}

func newPrincipal(t *testing.T, pool *pgxpool.Pool, name string) memory.Principal {
	t.Helper()
	p, err := db.New(pool).CreatePrincipal(context.Background(), db.CreatePrincipalParams{Kind: "human", DisplayName: name})
	if err != nil {
		t.Fatal(err)
	}
	return memory.Principal{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName, AuthMethod: "test"}
}

// member creates a principal with one grant.
func (tr *tree) member(t *testing.T, name, scope string, role memory.Role, cascade bool) memory.Principal {
	t.Helper()
	p := newPrincipal(t, tr.pool, name)
	if err := tr.svc.AddMember(context.Background(), uuid.Nil, scope, p.ID, role, cascade); err != nil {
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
