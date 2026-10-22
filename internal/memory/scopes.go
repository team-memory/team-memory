package memory

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// Service is the memory engine. Methods taking a Principal enforce the scope
// access rules in Go and run as that principal, so PostgreSQL row-level
// security enforces them again; such a Service is built on the app pool.
// Methods without a Principal are operator paths used by the CLI; they work
// only on a pool connected as the operator role.
type Service struct {
	pool *pgxpool.Pool
	// Logger receives audit-write failures. Nil discards them.
	Logger *slog.Logger
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func toScope(s db.Scope) Scope {
	return Scope{
		ID: s.ID, OrgID: s.OrgID, ParentID: s.ParentID, Level: s.Level,
		Slug: s.Slug, Name: s.Name, Depth: int(s.Depth), Path: s.Path,
	}
}

// mapDBError converts database constraint failures into domain errors.
func mapDBError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23514": // check_violation, including trigger-raised tree rules
			return &ValidationError{Msg: pgErr.Message}
		case "23505": // unique_violation
			return &ValidationError{Msg: "already exists: " + pgErr.ConstraintName}
		case "23503": // foreign_key_violation
			return &ValidationError{Msg: "referenced record does not exist"}
		case "42501": // insufficient_privilege: an RLS policy or guard trigger refused
			return ErrForbidden
		}
	}
	return err
}

func resolveScope(ctx context.Context, q *db.Queries, ref string) (db.Scope, error) {
	ref = strings.Trim(strings.TrimSpace(ref), "/")
	if ref == "" {
		return db.Scope{}, invalidf("scope is required")
	}
	var (
		s   db.Scope
		err error
	)
	if id, perr := uuid.Parse(ref); perr == nil {
		s, err = q.GetScopeByID(ctx, id)
	} else {
		s, err = q.GetScopeByPath(ctx, ref)
	}
	if err != nil {
		return db.Scope{}, mapDBError(err)
	}
	return s, nil
}

// ResolveScope looks up a scope by id or path without an access check.
// Operator use only.
func (s *Service) ResolveScope(ctx context.Context, ref string) (Scope, error) {
	var out Scope
	err := postgres.WithReadTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, ref)
		out = toScope(sc)
		return err
	})
	return out, err
}

// CreateOrganization creates an organization root scope and its settings.
func (s *Service) CreateOrganization(ctx context.Context, owner uuid.UUID, slug, name string) (Scope, error) {
	if err := validSlug(slug); err != nil {
		return Scope{}, err
	}
	if err := checkText("name", name, 1, 200); err != nil {
		return Scope{}, err
	}
	var out Scope
	err := postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		root, err := tx.Q.CreateRootScope(ctx, db.CreateRootScopeParams{
			ID: uuid.New(), Level: "organization", Slug: slug, Name: name, CreatedBy: &owner,
		})
		if err != nil {
			return mapDBError(err)
		}
		if _, err := tx.Q.InsertOrgSettings(ctx, db.InsertOrgSettingsParams{
			OrgID:            root.ID,
			AllowedLevels:    []string{"organization", "team", "project"},
			MaxDepth:         5,
			OwnerPrincipalID: owner,
		}); err != nil {
			return mapDBError(err)
		}
		out = toScope(root)
		return nil
	})
	return out, err
}

// CreateScope adds a child scope. The database enforces single parentage,
// the maximum depth and the allowed level labels.
func (s *Service) CreateScope(ctx context.Context, actor uuid.UUID, parentRef, level, slug, name string) (Scope, error) {
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
	var out Scope
	err := postgres.WithTx(ctx, s.pool, actor, func(tx postgres.Tx) error {
		parent, err := resolveScope(ctx, tx.Q, parentRef)
		if err != nil {
			return err
		}
		var createdBy *uuid.UUID
		if actor != uuid.Nil {
			createdBy = &actor
		}
		sc, err := tx.Q.CreateChildScope(ctx, db.CreateChildScopeParams{
			Level: level, Slug: slug, Name: name, CreatedBy: createdBy, ParentID: parent.ID,
		})
		if err != nil {
			return mapDBError(err)
		}
		out = toScope(sc)
		return nil
	})
	return out, err
}

// ListScopes lists every scope in an organization. Operator use only.
func (s *Service) ListScopes(ctx context.Context, orgRef string) ([]Scope, error) {
	var out []Scope
	err := postgres.WithReadTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		var rows []db.Scope
		if orgRef == "" {
			roots, err := tx.Q.ListOrgRoots(ctx)
			if err != nil {
				return err
			}
			for _, r := range roots {
				more, err := tx.Q.ListScopesInOrg(ctx, r.ID)
				if err != nil {
					return err
				}
				rows = append(rows, more...)
			}
		} else {
			org, err := resolveScope(ctx, tx.Q, orgRef)
			if err != nil {
				return err
			}
			rows, err = tx.Q.ListScopesInOrg(ctx, org.OrgID)
			if err != nil {
				return err
			}
		}
		for _, r := range rows {
			out = append(out, toScope(r))
		}
		return nil
	})
	return out, err
}

// GetOrgSettings returns the organization's tree settings.
func (s *Service) GetOrgSettings(ctx context.Context, orgRef string) (OrgSettings, error) {
	var out OrgSettings
	err := postgres.WithReadTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, orgRef)
		if err != nil {
			return err
		}
		st, err := tx.Q.GetOrgSettings(ctx, sc.OrgID)
		if err != nil {
			return mapDBError(err)
		}
		out = OrgSettings{OrgID: st.OrgID, AllowedLevels: st.AllowedLevels, MaxDepth: int(st.MaxDepth), OwnerPrincipal: st.OwnerPrincipalID}
		return nil
	})
	return out, err
}

// ConfigureOrg changes the allowed level labels and maximum depth. It refuses
// changes that would invalidate existing scopes.
func (s *Service) ConfigureOrg(ctx context.Context, orgRef string, levels []string, maxDepth int) (OrgSettings, error) {
	if maxDepth < 1 || maxDepth > 16 {
		return OrgSettings{}, invalidf("max depth must be between 1 and 16")
	}
	if len(levels) == 0 {
		return OrgSettings{}, invalidf("at least one level is required")
	}
	for _, l := range levels {
		if err := validLevel(l); err != nil {
			return OrgSettings{}, err
		}
	}
	var out OrgSettings
	err := postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, orgRef)
		if err != nil {
			return err
		}
		// Lock settings first: the scope trigger reads them FOR SHARE, so no
		// scope can be created under the old settings while these are checked.
		if _, err := tx.Q.LockOrgSettings(ctx, sc.OrgID); err != nil {
			return mapDBError(err)
		}
		deepest, err := tx.Q.MaxScopeDepthInOrg(ctx, sc.OrgID)
		if err != nil {
			return err
		}
		if int(deepest) >= maxDepth {
			return invalidf("existing scopes use %d levels; max depth cannot be lower", deepest+1)
		}
		existing, err := tx.Q.ListScopesInOrg(ctx, sc.OrgID)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if e.ParentID != nil && !slices.Contains(levels, e.Level) {
				return invalidf("level %q is used by scope %s", e.Level, e.Path)
			}
		}
		st, err := tx.Q.UpdateOrgSettings(ctx, db.UpdateOrgSettingsParams{
			OrgID: sc.OrgID, AllowedLevels: levels, MaxDepth: int32(maxDepth),
		})
		if err != nil {
			return mapDBError(err)
		}
		out = OrgSettings{OrgID: st.OrgID, AllowedLevels: st.AllowedLevels, MaxDepth: int(st.MaxDepth), OwnerPrincipal: st.OwnerPrincipalID}
		return nil
	})
	return out, err
}

// AddMember grants a role on a scope (initial membership bootstrap). Authorized
// membership administration is a stage 04 feature; this is an operator path.
func (s *Service) AddMember(ctx context.Context, actor uuid.UUID, scopeRef string, principal uuid.UUID, role Role, cascade bool) error {
	if role.rank() == 0 {
		return invalidf("invalid role")
	}
	return postgres.WithTx(ctx, s.pool, actor, func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, scopeRef)
		if err != nil {
			return err
		}
		var grantedBy *uuid.UUID
		if actor != uuid.Nil {
			grantedBy = &actor
		}
		if _, err := tx.Q.UpsertScopeMember(ctx, db.UpsertScopeMemberParams{
			PrincipalID: principal, Role: string(role), Cascade: cascade, GrantedBy: grantedBy, ScopeID: sc.ID,
		}); err != nil {
			return mapDBError(err)
		}
		return audit(ctx, tx, sc.OrgID, actor, "scope.member.grant", "scope_member", principal.String(), &sc.ID,
			map[string]any{"role": role, "cascade": cascade})
	})
}

// RemoveMember revokes a membership (operator path, audited). Revoking the
// organization owner's root grant is allowed here and only here.
func (s *Service) RemoveMember(ctx context.Context, scopeRef string, principal uuid.UUID) error {
	return postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, scopeRef)
		if err != nil {
			return err
		}
		n, err := tx.Q.DeleteScopeMember(ctx, db.DeleteScopeMemberParams{ScopeID: sc.ID, PrincipalID: principal})
		if err != nil {
			return mapDBError(err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, sc.OrgID, uuid.Nil, "scope.member.revoke", "scope_member", principal.String(), &sc.ID,
			map[string]any{"via": "cli"})
	})
}

// effectiveRole is the strongest role that applies to a scope for the
// principal: a direct membership, or a cascading grant on an ancestor. It is
// empty for scopes readable only as inherited, and for invisible scopes.
func effectiveRole(ctx context.Context, q *db.Queries, p Principal, scopeID uuid.UUID) (Role, error) {
	a, err := q.AccessOnScope(ctx, db.AccessOnScopeParams{PrincipalID: p.ID, ScopeID: scopeID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return roleForRank(a.Rank), nil
}

func roleForRank(rank int16) Role {
	switch rank {
	case 1:
		return RoleReader
	case 2:
		return RoleWriter
	case 3:
		return RoleMaintainer
	}
	return ""
}

// visibleScopes returns the scope itself and its ancestors if the principal
// can read the scope, nearest first. Descendants are never included.
func visibleScopes(ctx context.Context, q *db.Queries, p Principal, scopeRef string) ([]VisibleScope, error) {
	sc, err := resolveScope(ctx, q, scopeRef)
	if err != nil {
		return nil, err
	}
	role, err := effectiveRole(ctx, q, p, sc.ID)
	if err != nil {
		return nil, err
	}
	if !role.AtLeast(RoleReader) {
		return nil, ErrNotFound
	}
	rows, err := q.ListAncestors(ctx, sc.ID)
	if err != nil {
		return nil, err
	}
	out := make([]VisibleScope, 0, len(rows))
	for _, r := range rows {
		out = append(out, VisibleScope{
			Scope: Scope{ID: r.ID, OrgID: r.OrgID, ParentID: r.ParentID, Level: r.Level,
				Slug: r.Slug, Name: r.Name, Depth: int(r.Depth), Path: r.Path},
			Distance:  int(r.Distance),
			Inherited: r.Distance > 0,
		})
	}
	return out, nil
}

// VisibleScopesTx is visibleScopes for callers that run their own
// transaction (internal/search). Inside postgres.WithReadTx (repeatable read)
// the access check and the queries that depend on it share one snapshot.
func VisibleScopesTx(ctx context.Context, q *db.Queries, p Principal, scopeRef string) ([]VisibleScope, error) {
	return visibleScopes(ctx, q, p, scopeRef)
}

// VisibleScopes returns the scopes whose memories are visible from scopeRef:
// the scope itself plus its ancestors (inherited, read-only). It returns
// ErrNotFound if the principal cannot read the scope.
func (s *Service) VisibleScopes(ctx context.Context, p Principal, scopeRef string) ([]VisibleScope, error) {
	var out []VisibleScope
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		var err error
		out, err = visibleScopes(ctx, tx.Q, p, scopeRef)
		return err
	})
	return out, err
}

// BrowsableScopes returns the scopes the principal can open directly: those
// with a membership and the descendants of cascading grants.
func (s *Service) BrowsableScopes(ctx context.Context, p Principal) ([]Scope, error) {
	var out []Scope
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		rows, err := tx.Q.ReadableScopes(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, Scope{ID: r.ID, OrgID: r.OrgID, ParentID: r.ParentID, Level: r.Level,
				Slug: r.Slug, Name: r.Name, Depth: int(r.Depth), Path: r.Path})
		}
		return nil
	})
	return out, err
}
