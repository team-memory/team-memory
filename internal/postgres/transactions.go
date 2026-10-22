package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/postgres/db"
)

// Tx is a transaction bound to an acting principal.
type Tx struct {
	pgx.Tx
	Q *db.Queries
}

// Actor is the request identity a transaction runs as. RLS policies read
// both settings (tm_principal() and tm_auth_method()). AuthMethod is "pat",
// "oauth" or "portal"; review completion requires "portal".
type Actor struct {
	ID         uuid.UUID
	AuthMethod string
}

// WithTx runs fn in a transaction as the given principal with no auth
// method. A nil principal (uuid.Nil) is used by operator CLI paths, which
// connect as the operator role, and by authentication itself, which reads
// only identity tables.
func WithTx(ctx context.Context, pool *pgxpool.Pool, principal uuid.UUID, fn func(tx Tx) error) error {
	return WithActorTx(ctx, pool, Actor{ID: principal}, fn)
}

// WithReadTx is WithTx with a read-only REPEATABLE READ transaction: every
// statement sees one snapshot, so an access check and the queries that depend
// on it (search candidates, then their rows) cannot observe different states.
// Read-only transactions at this level never fail with serialization errors.
func WithReadTx(ctx context.Context, pool *pgxpool.Pool, principal uuid.UUID, fn func(tx Tx) error) error {
	return WithActorReadTx(ctx, pool, Actor{ID: principal}, fn)
}

// WithActorTx runs fn in a read-write transaction whose app.principal_id and
// app.auth_method settings identify the actor. Both are transaction-local
// (set_config(..., true), equivalent to SET LOCAL but with bind parameters),
// so they end with the transaction and a pooled connection never carries
// one request's identity into the next.
func WithActorTx(ctx context.Context, pool *pgxpool.Pool, a Actor, fn func(tx Tx) error) error {
	return withTx(ctx, pool, pgx.TxOptions{}, a, fn)
}

// WithActorReadTx is WithActorTx with a read-only REPEATABLE READ
// transaction (see WithReadTx).
func WithActorReadTx(ctx context.Context, pool *pgxpool.Pool, a Actor, fn func(tx Tx) error) error {
	return withTx(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, a, fn)
}

func withTx(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, a Actor, fn func(tx Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	principal := ""
	if a.ID != uuid.Nil {
		principal = a.ID.String()
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('app.principal_id', $1, true), set_config('app.auth_method', $2, true)",
		principal, a.AuthMethod); err != nil {
		return fmt.Errorf("set principal: %w", err)
	}
	if err = fn(Tx{Tx: tx, Q: db.New(tx)}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
