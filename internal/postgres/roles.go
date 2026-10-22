package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Database roles created by migration 00003. Deployments create login roles
// that are members of these (see docs/decisions/rls.md).
const (
	RoleApp      = "team_memory_app"
	RoleWorker   = "team_memory_worker"
	RoleOperator = "team_memory_operator"
	// RoleMirror (migration 00007) is the git mirror export worker.
	RoleMirror = "team_memory_mirror"
)

// RoleInfo describes the role a pool connects as.
type RoleInfo struct {
	User       string
	Superuser  bool
	BypassRLS  bool
	Operator   bool
	OwnsTables bool
	App        bool
	Worker     bool
	Mirror     bool
}

// InspectRole reports the privileges of the pool's connection role. Before
// the first migration the group roles may not exist; ok is false then.
func InspectRole(ctx context.Context, pool *pgxpool.Pool) (ri RoleInfo, ok bool, err error) {
	// A read-only transaction, so the check itself passes the TxGuard.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return ri, false, fmt.Errorf("inspect database role: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var n int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname IN ($1, $2, $3)",
		RoleOperator, RoleApp, RoleWorker).Scan(&n); err != nil {
		return ri, false, fmt.Errorf("inspect database role: %w", err)
	}
	if n < 3 {
		return ri, false, nil
	}
	err = tx.QueryRow(ctx, `
SELECT current_user, r.rolsuper, r.rolbypassrls,
       pg_has_role(current_user, $1, 'USAGE'),
       EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
               WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
                 AND pg_has_role(current_user, c.relowner, 'USAGE')),
       pg_has_role(current_user, $2, 'USAGE'),
       pg_has_role(current_user, $3, 'USAGE'),
       CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $4)
            THEN pg_has_role(current_user, $4, 'USAGE') ELSE false END
FROM pg_roles r WHERE r.rolname = current_user`, RoleOperator, RoleApp, RoleWorker, RoleMirror).Scan(
		&ri.User, &ri.Superuser, &ri.BypassRLS, &ri.Operator, &ri.OwnsTables, &ri.App, &ri.Worker, &ri.Mirror)
	if err != nil {
		return ri, false, fmt.Errorf("inspect database role: %w", err)
	}
	return ri, true, nil
}

// ErrUnsafeRole means a runtime process would connect with privileges that
// bypass row-level security or the append-only guards.
var ErrUnsafeRole = errors.New("unsafe database role")

// CheckRuntimeRole fails closed unless the pool's role is a plain member of
// want (RoleApp, RoleWorker or RoleMirror): not a superuser, without BYPASSRLS, not an
// operator, and owning no tables. serve and worker refuse to start
// otherwise; there is no override.
func CheckRuntimeRole(ctx context.Context, pool *pgxpool.Pool, want string) error {
	ri, ok, err := InspectRole(ctx, pool)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: database roles are missing; run `team-memory migrate` first", ErrUnsafeRole)
	}
	var problems []string
	if ri.Superuser {
		problems = append(problems, "is a superuser")
	}
	if ri.BypassRLS {
		problems = append(problems, "has BYPASSRLS")
	}
	if ri.Operator {
		problems = append(problems, "is a member of "+RoleOperator)
	}
	if ri.OwnsTables {
		problems = append(problems, "owns Team Memory tables")
	}
	members := map[string]bool{RoleApp: ri.App, RoleWorker: ri.Worker, RoleMirror: ri.Mirror}
	if _, known := members[want]; !known {
		return fmt.Errorf("unknown runtime role %q", want)
	}
	if !members[want] {
		problems = append(problems, "is not a member of "+want)
	}
	for _, other := range []string{RoleApp, RoleWorker, RoleMirror} {
		if other != want && members[other] {
			problems = append(problems, "is also a member of "+other)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: database user %q %s; connect as a login role that is a member of %s only (see docs/decisions/rls.md)",
			ErrUnsafeRole, ri.User, strings.Join(problems, ", "), want)
	}
	return nil
}

// CheckOperatorRole verifies the pool can act as operator (migrations and
// the operator CLI): a superuser or a member of RoleOperator. Before the
// first migration there is nothing to check; migrate creates the roles.
func CheckOperatorRole(ctx context.Context, pool *pgxpool.Pool) error {
	ri, ok, err := InspectRole(ctx, pool)
	if err != nil || !ok {
		return err
	}
	if !ri.Superuser && !ri.Operator {
		return fmt.Errorf("database user %q is not a member of %s; operator commands need the operator connection (TEAM_MEMORY_OPERATOR_DATABASE_URL or DATABASE_URL)", ri.User, RoleOperator)
	}
	return nil
}

// TxGuard is a pgx query tracer that counts statements executed outside a
// transaction. On the app pool every statement must run inside WithActorTx,
// where the request identity is set; RLS makes a stray statement see
// nothing, and the guard makes it visible. Tests assert Violations() == 0.
type TxGuard struct {
	Logger     *slog.Logger
	violations atomic.Int64
}

// Violations returns how many statements ran outside a transaction.
func (g *TxGuard) Violations() int64 { return g.violations.Load() }

// Reset clears the count (tests that run stray statements on purpose).
func (g *TxGuard) Reset() { g.violations.Store(0) }

// TraceQueryStart implements pgx.QueryTracer.
func (g *TxGuard) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if conn.PgConn().TxStatus() == 'I' && !isTxControl(data.SQL) {
		g.violations.Add(1)
		if g.Logger != nil {
			g.Logger.Error("statement outside a transaction on the app pool", "sql", firstLine(data.SQL))
		}
	}
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (g *TxGuard) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func isTxControl(sql string) bool {
	s := strings.ToLower(strings.TrimSpace(sql))
	return strings.HasPrefix(s, "begin") || s == "-- ping" || s == ";"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
