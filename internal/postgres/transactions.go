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

// WithTx runs fn in a transaction whose app.principal_id setting is the
// acting principal. RLS policies (stage 04) read this setting. The setting is
// transaction-local, equivalent to SET LOCAL, but passed as a bind parameter.
// A nil principal (uuid.Nil) is used only by operator CLI paths.
func WithTx(ctx context.Context, pool *pgxpool.Pool, principal uuid.UUID, fn func(tx Tx) error) error {
	return withTx(ctx, pool, pgx.TxOptions{}, principal, fn)
}

// WithReadTx is WithTx with a read-only transaction.
func WithReadTx(ctx context.Context, pool *pgxpool.Pool, principal uuid.UUID, fn func(tx Tx) error) error {
	return withTx(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, principal, fn)
}

func withTx(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, principal uuid.UUID, fn func(tx Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.principal_id', $1, true)", principal.String()); err != nil {
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
