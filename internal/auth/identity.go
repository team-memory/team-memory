package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/ratelimit"
)

var (
	// ErrIdentityLinked means the external identity already belongs to a
	// different principal. Linking never moves an identity.
	ErrIdentityLinked = errors.New("identity is already linked to another principal")
	// ErrUnknownIdentity means no principal has the identity and automatic
	// provisioning is off for its provider.
	ErrUnknownIdentity = errors.New("no principal is linked to this identity")
	// ErrPrincipalDisabled means the principal exists but may not sign in.
	ErrPrincipalDisabled = errors.New("principal is disabled")
)

// ExternalIdentity is a verified upstream identity. Issuer and Subject are
// the only lookup key; Email and Name are display data and never used to
// match an existing principal.
type ExternalIdentity struct {
	Issuer  string
	Subject string
	Email   string
	Name    string
	// Groups is the provider's groups claim, used only for optional group
	// to role mapping. AuthTime is the upstream authentication time.
	Groups   []string
	AuthTime time.Time
}

func (e ExternalIdentity) valid() error {
	if e.Issuer == "" || e.Subject == "" || len(e.Issuer) > 2000 || len(e.Subject) > 255 {
		return errors.New("identity needs an issuer and a subject")
	}
	return nil
}

// Identities maps upstream (issuer, subject) pairs to principals.
type Identities struct{ pool *pgxpool.Pool }

func NewIdentities(pool *pgxpool.Pool) *Identities { return &Identities{pool: pool} }

// Resolve returns the principal linked to id. When none is linked and
// provision is true, it creates a human principal with no scope memberships
// and links id to it. A provisioned principal never becomes an owner: roles
// are granted separately by an administrator.
func (s *Identities) Resolve(ctx context.Context, id ExternalIdentity, provision bool) (memory.Principal, bool, error) {
	if err := id.valid(); err != nil {
		return memory.Principal{}, false, err
	}
	var (
		out     memory.Principal
		created bool
	)
	err := postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		// Serialize concurrent first logins of the same identity.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "identity|"+id.Issuer+"|"+id.Subject); err != nil {
			return err
		}
		p, err := tx.Q.GetPrincipalByIdentity(ctx, db.GetPrincipalByIdentityParams{Issuer: id.Issuer, Subject: id.Subject})
		switch {
		case err == nil:
			if p.DisabledAt != nil {
				return ErrPrincipalDisabled
			}
			out = toPrincipal(p)
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		case !provision:
			return ErrUnknownIdentity
		}
		p, err = tx.Q.CreatePrincipal(ctx, db.CreatePrincipalParams{Kind: "human", DisplayName: displayName(id)})
		if err != nil {
			return err
		}
		if _, err := tx.Q.InsertPrincipalIdentity(ctx, db.InsertPrincipalIdentityParams{PrincipalID: p.ID, Issuer: id.Issuer, Subject: id.Subject}); err != nil {
			return err
		}
		// The audit row's actor is the new principal; RLS accepts an actor
		// only when it is the transaction's principal.
		if err := actAs(ctx, tx, p.ID); err != nil {
			return err
		}
		if err := auditIdentity(ctx, tx.Q, p.ID, "auth.principal_provisioned", id); err != nil {
			return err
		}
		out, created = toPrincipal(p), true
		return nil
	})
	return out, created, err
}

// Link attaches id to an existing principal. Callers must already have
// proven control of both the principal (an authenticated session) and the
// identity (a fresh upstream login); this function only enforces the data
// rules: an identity belongs to at most one principal, and linking the same
// pair twice is a no-op. Email equality is never consulted.
func (s *Identities) Link(ctx context.Context, principal uuid.UUID, id ExternalIdentity) error {
	if err := id.valid(); err != nil {
		return err
	}
	return postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "identity|"+id.Issuer+"|"+id.Subject); err != nil {
			return err
		}
		p, err := tx.Q.GetPrincipal(ctx, principal)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("principal %s not found", principal)
		}
		if err != nil {
			return err
		}
		if p.DisabledAt != nil {
			return ErrPrincipalDisabled
		}
		existing, err := tx.Q.GetPrincipalByIdentity(ctx, db.GetPrincipalByIdentityParams{Issuer: id.Issuer, Subject: id.Subject})
		switch {
		case err == nil && existing.ID == principal:
			return nil
		case err == nil:
			return ErrIdentityLinked
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if _, err := tx.Q.InsertPrincipalIdentity(ctx, db.InsertPrincipalIdentityParams{PrincipalID: principal, Issuer: id.Issuer, Subject: id.Subject}); err != nil {
			return err
		}
		if err := actAs(ctx, tx, principal); err != nil {
			return err
		}
		return auditIdentity(ctx, tx.Q, principal, "auth.identity_linked", id)
	})
}

// Unlink removes an identity from a principal (operator recovery). It
// never moves the identity elsewhere; the next sign-in with it provisions a
// new principal or is refused, per the provider's auto_provision setting.
func (s *Identities) Unlink(ctx context.Context, principal uuid.UUID, id ExternalIdentity) error {
	if err := id.valid(); err != nil {
		return err
	}
	return postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		n, err := tx.Q.DeletePrincipalIdentity(ctx, db.DeletePrincipalIdentityParams{PrincipalID: principal, Issuer: id.Issuer, Subject: id.Subject})
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("identity is not linked to principal %s", principal)
		}
		return memory.InsertAudit(ctx, tx.Q, memory.AuditEvent{Action: "auth.identity_unlinked", TargetType: "principal",
			TargetID: principal.String(), Details: map[string]any{"issuer": id.Issuer, "subject": id.Subject, "via": "cli"}})
	})
}

// SyncGroupRoles applies the operator-managed IdP group mappings for the
// identity's issuer to the principal: grants marked source=idp are added,
// changed or removed to match the groups in this sign-in. Manual grants are
// never touched. Runs as the principal through a definer function that
// reads only operator-managed mappings.
func (s *Identities) SyncGroupRoles(ctx context.Context, principal uuid.UUID, id ExternalIdentity) error {
	return postgres.WithTx(ctx, s.pool, principal, func(tx postgres.Tx) error {
		groups := id.Groups
		if groups == nil {
			groups = []string{}
		}
		if err := tx.Q.SyncIdPGrants(ctx, db.SyncIdPGrantsParams{PrincipalID: principal, Issuer: id.Issuer, Groups: groups}); err != nil {
			return err
		}
		return memory.InsertAudit(ctx, tx.Q, memory.AuditEvent{ActorID: &principal, Action: "auth.group_roles_synced",
			TargetType: "principal", TargetID: principal.String(), Details: map[string]any{"issuer": id.Issuer, "groups": len(groups)}})
	})
}

// ErrStaleLogin means a linking login is older than LinkMaxAge.
var ErrStaleLogin = errors.New("the identity provider login is not recent; sign in again to link")

// LinkMaxAge bounds how old the upstream authentication may be when an
// identity is linked from the portal.
const LinkMaxAge = 5 * time.Minute

// LinkFromPortal is the authorization for "Connect SSO" (stage 05 supplies
// the browser flow): the actor must be in an authenticated portal session,
// and the identity must come from a fresh upstream login started for
// linking (prompt=login), so both identities are proven now. Ownership is
// never changed; Link enforces the data rules.
func (s *Identities) LinkFromPortal(ctx context.Context, actor memory.Principal, id ExternalIdentity, now time.Time) error {
	if actor.AuthMethod != memory.AuthPortal {
		return memory.ErrBrowserReviewRequired
	}
	if id.AuthTime.IsZero() || now.Sub(id.AuthTime) > LinkMaxAge || id.AuthTime.After(now.Add(time.Minute)) {
		return ErrStaleLogin
	}
	return s.Link(ctx, actor.ID, id)
}

// Principal loads an enabled principal by id, for bearer tokens that carry
// the principal id as their subject.
func (s *Identities) Principal(ctx context.Context, id uuid.UUID) (memory.Principal, error) {
	var out memory.Principal
	err := postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		p, err := tx.Q.GetPrincipal(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCredential
		}
		if err != nil {
			return err
		}
		if p.DisabledAt != nil {
			return ErrInvalidCredential
		}
		out = toPrincipal(p)
		return nil
	})
	return out, err
}

func toPrincipal(p db.Principal) memory.Principal {
	return memory.Principal{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName}
}

func displayName(id ExternalIdentity) string {
	name := strings.TrimSpace(id.Name)
	if name == "" {
		name = strings.TrimSpace(id.Email)
	}
	if name == "" {
		name = "user " + id.Subject
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name
}

// actAs switches the transaction's request identity to principal, for
// writes attributed to a principal that was just created or verified.
func actAs(ctx context.Context, tx postgres.Tx, principal uuid.UUID) error {
	_, err := tx.Exec(ctx, "SELECT set_config('app.principal_id', $1, true)", principal.String())
	return err
}

func auditIdentity(ctx context.Context, q *db.Queries, principal uuid.UUID, action string, id ExternalIdentity) error {
	// Issuer and subject identify the identity; email and name stay out of
	// the audit log.
	return memory.InsertAudit(ctx, q, memory.AuditEvent{ActorID: &principal, Action: action, TargetType: "principal",
		TargetID: principal.String(), Details: map[string]any{"issuer": id.Issuer, "subject": id.Subject}})
}

// FailureAuditor records rejected credentials in the audit log without the
// credential itself. Writes are rate-limited per client address, so a flood
// of bad tokens cannot turn into an unbounded stream of database writes;
// suppressed failures are only counted in the log.
type FailureAuditor struct {
	pool    *pgxpool.Pool
	limiter *ratelimit.Limiter
	logger  *slog.Logger
}

// NewFailureAuditor audits at most one failure per second per client
// address, with a burst of 20.
func NewFailureAuditor(pool *pgxpool.Pool, logger *slog.Logger) *FailureAuditor {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &FailureAuditor{pool: pool, limiter: ratelimit.New(1, 20), logger: logger}
}

type clientAddrKey struct{}

// WithClientAddr attaches the client address used to rate-limit audits.
func WithClientAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, clientAddrKey{}, addr)
}

// ClientAddr returns the address attached by WithClientAddr.
func ClientAddr(ctx context.Context) string {
	a, _ := ctx.Value(clientAddrKey{}).(string)
	return a
}

// Record writes an auth.failure audit event. kind is the credential type
// ("pat", "jwt", "upstream"); reason is a fixed code.
func (a *FailureAuditor) Record(ctx context.Context, kind, reason string) {
	addr := ClientAddr(ctx)
	if ok, _ := a.limiter.Allow(addr); !ok {
		a.logger.Warn("authentication failure (audit suppressed by rate limit)", "kind", kind, "reason", reason)
		return
	}
	err := postgres.WithTx(context.WithoutCancel(ctx), a.pool, uuid.Nil, func(tx postgres.Tx) error {
		return memory.InsertAudit(ctx, tx.Q, memory.AuditEvent{Action: "auth.failure", TargetType: "credential",
			TargetID: kind, Outcome: "denied", Details: map[string]any{"reason": reason, "client": addr}})
	})
	if err != nil {
		a.logger.Error("audit authentication failure", "err", err)
	}
}

// RecordSignIn writes an auth.sign_in audit event for a completed upstream
// login (outcome success) or a refused one (denied, principal unknown).
func (a *FailureAuditor) RecordSignIn(ctx context.Context, principal uuid.UUID, issuer, reason string) {
	outcome, actor := "success", &principal
	if principal == uuid.Nil {
		outcome, actor = "denied", nil
	}
	err := postgres.WithTx(context.WithoutCancel(ctx), a.pool, principal, func(tx postgres.Tx) error {
		return memory.InsertAudit(ctx, tx.Q, memory.AuditEvent{ActorID: actor, Action: "auth.sign_in", TargetType: "identity_provider",
			TargetID: issuer, Outcome: outcome, Details: map[string]any{"reason": reason, "client": ClientAddr(ctx)}})
	})
	if err != nil {
		a.logger.Error("audit sign-in", "err", err)
	}
}
