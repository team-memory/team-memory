package memory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// Authorized scope and membership administration. These run as the acting
// principal: Go checks the rule and returns domain errors, and RLS on
// scopes and scope_members enforces it again. Operator equivalents without
// a Principal (CreateScope, AddMember) remain for the CLI.

// Member is one membership row as administrators see it.
type Member struct {
	PrincipalID uuid.UUID  `json:"principal_id"`
	DisplayName string     `json:"display_name"`
	Role        Role       `json:"role"`
	Cascade     bool       `json:"cascade"`
	Source      string     `json:"source"` // manual | idp
	GrantedBy   *uuid.UUID `json:"granted_by,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// requireCascadingMaintainer checks that p holds maintainer through a grant
// covering every descendant of the scope (a cascading grant on it or an
// ancestor). Creating child scopes and granting cascading roles need it, so
// a maintainer of one scope cannot open its subtree to others.
func requireCascadingMaintainer(ctx context.Context, q *db.Queries, p Principal, scopeID uuid.UUID) error {
	a, err := q.AccessOnScope(ctx, db.AccessOnScopeParams{PrincipalID: p.ID, ScopeID: scopeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if a.CascadeRank < 3 {
		return ErrForbidden
	}
	return nil
}

// CreateScopeAs adds a child scope. Requires a cascading maintainer grant
// covering the parent, so the creator can administer the new scope.
func (s *Service) CreateScopeAs(ctx context.Context, p Principal, parentRef, level, slug, name string) (Scope, error) {
	if err := validSlug(slug); err != nil {
		return Scope{}, err
	}
	if err := validLevel(level); err != nil {
		return Scope{}, err
	}
	if name == "" {
		name = slug
	}
	if err := checkText("name", name, 1, 200); err != nil {
		return Scope{}, err
	}
	id := uuid.New()
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		parent, err := resolveScope(ctx, tx.Q, parentRef)
		if err != nil {
			return err
		}
		if err := requireCascadingMaintainer(ctx, tx.Q, p, parent.ID); err != nil {
			return err
		}
		createdBy := p.ID
		if err := tx.Q.InsertChildScope(ctx, db.InsertChildScopeParams{
			ID: id, Level: level, Slug: slug, Name: name, CreatedBy: &createdBy, ParentID: parent.ID,
		}); err != nil {
			return mapDBError(err)
		}
		return audit(ctx, tx, parent.OrgID, p.ID, "scope.create", "scope", id.String(), &id,
			map[string]any{"parent": parent.ID, "level": level, "slug": slug})
	})
	if err != nil {
		s.auditDenial(ctx, p, "scope.create", "scope", parentRef, err)
		return Scope{}, err
	}
	var out Scope
	err = postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		sc, err := tx.Q.GetScopeByID(ctx, id)
		out = toScope(sc)
		return mapDBError(err)
	})
	return out, err
}

// GrantMember grants or changes a role on a scope. Requires maintainer on
// the scope; a cascading grant also needs a cascading maintainer grant
// covering it. The organization owner's root grant changes only through
// the operator CLI.
func (s *Service) GrantMember(ctx context.Context, p Principal, scopeRef string, principal uuid.UUID, role Role, cascade bool) error {
	if role.rank() == 0 {
		return invalidf("invalid role")
	}
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, scopeRef)
		if err != nil {
			return err
		}
		if err := requireRole(ctx, tx.Q, p, sc.ID, RoleMaintainer); err != nil {
			if errors.Is(err, ErrInherited) {
				return ErrForbidden
			}
			return err
		}
		if cascade {
			if err := requireCascadingMaintainer(ctx, tx.Q, p, sc.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Q.GetPrincipal(ctx, principal); err != nil {
			return mapDBError(err)
		}
		grantedBy := p.ID
		if _, err := tx.Q.UpsertScopeMember(ctx, db.UpsertScopeMemberParams{
			PrincipalID: principal, Role: string(role), Cascade: cascade, GrantedBy: &grantedBy, ScopeID: sc.ID,
		}); err != nil {
			return mapDBError(err)
		}
		return audit(ctx, tx, sc.OrgID, p.ID, "scope.member.grant", "scope_member", principal.String(), &sc.ID,
			map[string]any{"role": role, "cascade": cascade})
	})
	if err != nil {
		s.auditDenial(ctx, p, "scope.member.grant", "scope_member", principal.String(), err)
	}
	return err
}

// RevokeMember removes a membership. Requires maintainer on the scope. The
// change is visible to the revoked principal's next transaction: access is
// precomputed in the same transaction and nothing caches it.
func (s *Service) RevokeMember(ctx context.Context, p Principal, scopeRef string, principal uuid.UUID) error {
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, scopeRef)
		if err != nil {
			return err
		}
		if err := requireRole(ctx, tx.Q, p, sc.ID, RoleMaintainer); err != nil {
			if errors.Is(err, ErrInherited) {
				return ErrForbidden
			}
			return err
		}
		n, err := tx.Q.DeleteScopeMember(ctx, db.DeleteScopeMemberParams{ScopeID: sc.ID, PrincipalID: principal})
		if err != nil {
			return mapDBError(err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, sc.OrgID, p.ID, "scope.member.revoke", "scope_member", principal.String(), &sc.ID, nil)
	})
	if err != nil {
		s.auditDenial(ctx, p, "scope.member.revoke", "scope_member", principal.String(), err)
	}
	return err
}

// ListMembers lists a scope's direct memberships. Requires maintainer.
func (s *Service) ListMembers(ctx context.Context, p Principal, scopeRef string) ([]Member, error) {
	var out []Member
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, scopeRef)
		if err != nil {
			return err
		}
		if err := requireRole(ctx, tx.Q, p, sc.ID, RoleMaintainer); err != nil {
			if errors.Is(err, ErrInherited) {
				return ErrForbidden
			}
			return err
		}
		rows, err := tx.Q.ListScopeMembers(ctx, sc.ID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, Member{PrincipalID: r.PrincipalID, DisplayName: r.DisplayName, Role: Role(r.Role),
				Cascade: r.Cascade, Source: r.Source, GrantedBy: r.GrantedBy, UpdatedAt: r.UpdatedAt})
		}
		return nil
	})
	return out, err
}

// AuditEntry is an audit event as maintainers read it.
type AuditEntry struct {
	ID         int64          `json:"id"`
	OccurredAt time.Time      `json:"occurred_at"`
	ActorID    *uuid.UUID     `json:"actor_id,omitempty"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	ScopeID    *uuid.UUID     `json:"scope_id,omitempty"`
	Outcome    string         `json:"outcome"`
	RequestID  string         `json:"request_id,omitempty"`
	Details    map[string]any `json:"details"`
}

// AuditLog returns recent audit events of a scope. Requires maintainer;
// RLS limits rows to scopes the reader maintains.
func (s *Service) AuditLog(ctx context.Context, p Principal, scopeRef string, limit int) ([]AuditEntry, error) {
	var out []AuditEntry
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, scopeRef)
		if err != nil {
			return err
		}
		if err := requireRole(ctx, tx.Q, p, sc.ID, RoleMaintainer); err != nil {
			if errors.Is(err, ErrInherited) {
				return ErrForbidden
			}
			return err
		}
		rows, err := tx.Q.ListAuditEvents(ctx, db.ListAuditEventsParams{ScopeIds: []uuid.UUID{sc.ID}, MaxResults: int32(ClampLimit(limit))})
		if err != nil {
			return err
		}
		for _, r := range rows {
			e := AuditEntry{ID: r.ID, OccurredAt: r.OccurredAt, ActorID: r.ActorID, Action: r.Action, TargetType: r.TargetType,
				TargetID: r.TargetID, ScopeID: r.ScopeID, Outcome: r.Outcome}
			if r.RequestID != nil {
				e.RequestID = *r.RequestID
			}
			_ = jsonUnmarshal(r.Details, &e.Details)
			out = append(out, e)
		}
		return nil
	})
	return out, err
}
