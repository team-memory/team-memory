package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
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
		return auditIdentity(ctx, tx.Q, principal, "auth.identity_linked", id)
	})
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

func auditIdentity(ctx context.Context, q *db.Queries, principal uuid.UUID, action string, id ExternalIdentity) error {
	// Issuer and subject identify the identity; email and name stay out of
	// the audit log.
	details, err := json.Marshal(map[string]string{"issuer": id.Issuer, "subject": id.Subject})
	if err != nil {
		return err
	}
	return q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorID: &principal, Action: action, TargetType: "principal",
		TargetID: principal.String(), Outcome: "success", Details: details,
	})
}
