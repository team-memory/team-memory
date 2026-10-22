// Package testdb gives tests a fresh, migrated PostgreSQL database. Set
// TEST_DATABASE_URL to a role that may create databases and roles; each call
// creates a database that is dropped when the test ends. Used only from tests.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/postgres"
)

// Login roles the tests connect as. They are members of the group roles that
// migration 00003 creates, exactly as a deployment's login roles would be.
// The password is a throwaway for the local test cluster.
const (
	AppUser      = "tm_test_app"
	WorkerUser   = "tm_test_worker"
	OperatorUser = "tm_test_operator"
	testPassword = "tm-test-only"
)

// New returns a pool on a new migrated database, or skips the test when
// TEST_DATABASE_URL is unset (it fails instead when CI is set). The pool
// connects as TEST_DATABASE_URL's role (a superuser locally), so it bypasses
// RLS: use it for setup, and DB.App for code under test.
func New(t testing.TB) *pgxpool.Pool {
	return NewDB(t).Admin
}

// DB holds pools on one test database, one per database role.
type DB struct {
	Name string
	// Admin is TEST_DATABASE_URL's role: fixture setup and assertions.
	Admin *pgxpool.Pool
	// App connects as a member of team_memory_app, like serve. Guard counts
	// statements it runs outside a transaction.
	App   *pgxpool.Pool
	Guard *postgres.TxGuard
	// Worker connects as a member of team_memory_worker.
	Worker *pgxpool.Pool
	// Operator connects as a non-superuser member of team_memory_operator.
	Operator *pgxpool.Pool
}

// NewDB creates and migrates a database and connects one pool per role.
func NewDB(t testing.TB) *DB {
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
	d := &DB{Name: name, Admin: pool}
	t.Cleanup(func() {
		for _, p := range []*pgxpool.Pool{d.App, d.Worker, d.Operator, d.Admin} {
			if p != nil {
				p.Close()
			}
		}
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ensureLoginRoles(t, pool)

	connect := func(user string, tracer pgx.QueryTracer) *pgxpool.Pool {
		ru := *u
		ru.User = url.UserPassword(user, testPassword)
		p, err := postgres.OpenWithTracer(ctx, ru.String(), tracer)
		if err != nil {
			t.Fatalf("connect as %s: %v", user, err)
		}
		return p
	}
	d.Guard = &postgres.TxGuard{}
	d.App = connect(AppUser, d.Guard)
	d.Worker = connect(WorkerUser, nil)
	d.Operator = connect(OperatorUser, nil)
	t.Cleanup(func() {
		if n := d.Guard.Violations(); n > 0 {
			t.Errorf("%d statements ran outside a transaction on the app pool", n)
		}
	})
	return d
}

// ensureLoginRoles creates the cluster-wide test login roles once. An
// advisory lock serializes parallel test packages.
func ensureLoginRoles(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext('tm_test_roles'))"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext('tm_test_roles'))") }()
	for user, group := range map[string]string{
		AppUser: postgres.RoleApp, WorkerUser: postgres.RoleWorker, OperatorUser: postgres.RoleOperator,
	} {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", user).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			if _, err := conn.Exec(ctx, "CREATE ROLE "+user+" LOGIN PASSWORD '"+testPassword+"'"); err != nil {
				t.Fatalf("create role %s: %v", user, err)
			}
		}
		if _, err := conn.Exec(ctx, "GRANT "+group+" TO "+user); err != nil {
			t.Fatalf("grant %s to %s: %v", group, user, err)
		}
	}
}
