package memory

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/secrets"
)

// Read models for the portal. Everything runs in one read-only transaction
// as the acting principal, so RLS decides what exists: counts and listings
// can never include a scope or memory the principal cannot read, and nothing
// here aggregates descendants' memories.

// BrowseFilter narrows a scope listing. Empty fields match everything,
// except Statuses, which defaults to active.
type BrowseFilter struct {
	Kinds    []string
	Trusts   []string
	Statuses []string
	Since    time.Time // updated on or after
	Until    time.Time // updated before
	Limit    int
}

// InheritedGroup is one ancestor's memories, as seen read-only from a
// descendant.
type InheritedGroup struct {
	Scope    Scope
	Memories []SearchResult
}

// ScopePage is everything the scope page shows.
type ScopePage struct {
	Scope Scope
	// Role is the viewer's effective role on this scope.
	Role      Role
	Ancestors []Scope // root first, excluding Scope
	Own       []SearchResult
	Inherited []InheritedGroup
	// Overridden maps an inherited memory to the memories that effectively
	// override it here (approved, active). Used to collapse them.
	Overridden map[uuid.UUID][]uuid.UUID
	// Referenced describes the memories named by Overridden (both sides),
	// whether or not they match the filters. All are readable here: they
	// are owned by scopes in the viewer's chain.
	Referenced map[uuid.UUID]SearchResult
	// OwnTruncated and InheritedTruncated report that a listing hit its
	// limit. The two lists are limited separately, so a busy ancestor
	// cannot push the scope's own memories off the page.
	OwnTruncated, InheritedTruncated bool
}

const browseSQL = `
SELECT m.id, m.scope_id, s.path, m.kind, m.status, m.trust, m.current_revision, m.created_by, m.updated_at,
       r.title, r.content, r.rationale, r.source_uri, ap.reviewer_id, ap.decided_at
FROM memories m
JOIN memory_revisions r ON r.memory_id = m.id AND r.revision = m.current_revision
JOIN scopes s ON s.id = m.scope_id
LEFT JOIN approvals ap ON m.trust = 'approved' AND ap.id = (
    SELECT a.id FROM approvals a WHERE a.memory_id = m.id AND a.revision = m.current_revision
    ORDER BY a.decided_at DESC, a.id DESC LIMIT 1)
WHERE m.scope_id = ANY($1::uuid[])
  AND m.status = ANY($2::text[])
  AND (cardinality($3::text[]) = 0 OR m.kind = ANY($3::text[]))
  AND (cardinality($4::text[]) = 0 OR m.trust = ANY($4::text[]))
  AND ($5::timestamptz IS NULL OR m.updated_at >= $5)
  AND ($6::timestamptz IS NULL OR m.updated_at < $6)
ORDER BY m.updated_at DESC, m.id
LIMIT $7`

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ScopePage lists the memories a scope owns and, separately, those it
// inherits from its ancestors. ErrNotFound if the viewer cannot read the
// scope.
func (s *Service) ScopePage(ctx context.Context, p Principal, scopeRef string, f BrowseFilter) (ScopePage, error) {
	kinds, err := ParseKinds(f.Kinds)
	if err != nil {
		return ScopePage{}, err
	}
	statuses := f.Statuses
	if len(statuses) == 0 {
		statuses = []string{string(StatusActive)}
	}
	for _, st := range statuses {
		if st != string(StatusActive) && st != string(StatusSuperseded) && st != string(StatusArchived) {
			return ScopePage{}, invalidf("unknown status %q", st)
		}
	}
	for _, tr := range f.Trusts {
		if tr != string(TrustUnverified) && tr != string(TrustCited) && tr != string(TrustApproved) {
			return ScopePage{}, invalidf("unknown trust level %q", tr)
		}
	}
	trusts := append([]string{}, f.Trusts...) // never nil: a NULL array would match nothing
	limit := ClampLimit(f.Limit)
	var out ScopePage
	err = postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		vs, err := visibleScopes(ctx, tx.Q, p, scopeRef)
		if err != nil {
			return err
		}
		out.Scope = vs[0].Scope
		if out.Role, err = effectiveRole(ctx, tx.Q, p, out.Scope.ID); err != nil {
			return err
		}
		byID := map[uuid.UUID]Scope{}
		for _, v := range vs {
			byID[v.ID] = v.Scope
		}
		for i := len(vs) - 1; i >= 1; i-- {
			out.Ancestors = append(out.Ancestors, vs[i].Scope)
		}
		ids, inherited := scopeIDs(vs)
		var ownIDs, inhIDs []uuid.UUID
		for _, id := range ids {
			if inherited[id] {
				inhIDs = append(inhIDs, id)
			} else {
				ownIDs = append(ownIDs, id)
			}
		}
		list := func(scopeIDs []uuid.UUID) ([]SearchResult, bool, error) {
			if len(scopeIDs) == 0 {
				return nil, false, nil
			}
			rows, err := tx.Query(ctx, browseSQL, scopeIDs, statuses, kinds, trusts, nullTime(f.Since), nullTime(f.Until), limit+1)
			if err != nil {
				return nil, false, err
			}
			defer rows.Close()
			var res []SearchResult
			for rows.Next() {
				var (
					r                   SearchResult
					scope               uuid.UUID
					kind, status, trust string
					src                 *string
				)
				if err := rows.Scan(&r.ID, &scope, &r.OriginScope, &kind, &status, &trust, &r.Revision, &r.CreatedBy, &r.UpdatedAt,
					&r.Title, &r.Content, &r.Rationale, &src, &r.ApprovedBy, &r.ApprovedAt); err != nil {
					return nil, false, err
				}
				r.Kind, r.Status, r.Trust, r.SourceURI = Kind(kind), Status(status), Trust(trust), deref(src)
				r.Inherited = inherited[scope]
				r.scope = scope
				res = append(res, r)
			}
			if err := rows.Err(); err != nil {
				return nil, false, err
			}
			if len(res) > limit {
				return res[:limit], true, nil
			}
			return res, false, nil
		}
		var inh []SearchResult
		if out.Own, out.OwnTruncated, err = list(ownIDs); err != nil {
			return err
		}
		if inh, out.InheritedTruncated, err = list(inhIDs); err != nil {
			return err
		}
		for _, r := range inh {
			found := false
			for i := range out.Inherited {
				if out.Inherited[i].Scope.ID == r.scope {
					out.Inherited[i].Memories = append(out.Inherited[i].Memories, r)
					found = true
				}
			}
			if !found {
				out.Inherited = append(out.Inherited, InheritedGroup{Scope: byID[r.scope], Memories: []SearchResult{r}})
			}
		}
		// Root first.
		slices.SortFunc(out.Inherited, func(a, b InheritedGroup) int { return a.Scope.Depth - b.Scope.Depth })
		ov, err := tx.Q.EffectiveOverrides(ctx, ids)
		if err != nil {
			return err
		}
		out.Overridden = map[uuid.UUID][]uuid.UUID{}
		out.Referenced = map[uuid.UUID]SearchResult{}
		for _, o := range ov {
			out.Overridden[o.OverriddenMemoryID] = append(out.Overridden[o.OverriddenMemoryID], o.OverridingMemoryID)
			for _, id := range []uuid.UUID{o.OverriddenMemoryID, o.OverridingMemoryID} {
				if _, ok := out.Referenced[id]; ok {
					continue
				}
				if r, err := listing(ctx, tx.Q, p, id); err == nil {
					out.Referenced[id] = r
				}
			}
		}
		return nil
	})
	return out, err
}

// ConflictCandidate pairs a memory with a similar memory in another scope.
type ConflictCandidate struct {
	Memory     SearchResult // the memory in the deeper scope
	Other      SearchResult // the similar memory in an ancestor scope
	Similarity float64
}

const conflictSQL = `
SELECT c.similarity, c.memory_id, c.candidate_memory_id
FROM conflict_candidates c
JOIN memories a ON a.id = c.memory_id AND a.current_revision = c.revision AND a.status = 'active'
JOIN memories b ON b.id = c.candidate_memory_id AND b.current_revision = c.candidate_revision AND b.status = 'active'
WHERE c.status = 'open' AND (c.memory_id = $1 OR c.candidate_memory_id = $1)
ORDER BY c.similarity DESC
LIMIT 10`

// ConflictsFor lists open conflict candidates that involve a memory, limited
// to counterparts the viewer can read. The counterpart's scope name appears
// only when readable, so a candidate never reveals a hidden scope.
func (s *Service) ConflictsFor(ctx context.Context, p Principal, id uuid.UUID) ([]ConflictCandidate, error) {
	var out []ConflictCandidate
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		if _, err := getInTx(ctx, tx.Q, p, id, uuid.Nil); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, conflictSQL, id)
		if err != nil {
			return err
		}
		type pair struct {
			sim        float32
			deep, high uuid.UUID
		}
		var pairs []pair
		for rows.Next() {
			var pr pair
			if err := rows.Scan(&pr.sim, &pr.deep, &pr.high); err != nil {
				rows.Close()
				return err
			}
			pairs = append(pairs, pr)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, pr := range pairs {
			a, errA := listing(ctx, tx.Q, p, pr.deep)
			b, errB := listing(ctx, tx.Q, p, pr.high)
			if errA != nil || errB != nil {
				continue // the counterpart is not readable: not revealed
			}
			out = append(out, ConflictCandidate{Memory: a, Other: b, Similarity: float64(pr.sim)})
		}
		return nil
	})
	return out, err
}

func listing(ctx context.Context, q *db.Queries, p Principal, id uuid.UUID) (SearchResult, error) {
	m, err := getInTx(ctx, q, p, id, uuid.Nil)
	if err != nil {
		return SearchResult{}, err
	}
	return SearchResult{ID: m.ID, OriginScope: m.OriginScope, Inherited: m.Inherited, Kind: m.Kind, Status: m.Status,
		Trust: m.Trust, Revision: m.Revision, Title: m.Title, Content: m.Content, Rationale: m.Rationale,
		SourceURI: m.SourceURI, CreatedBy: m.CreatedBy, UpdatedAt: m.UpdatedAt}, nil
}

// SecretScan reports credential-shaped findings in a memory's current
// content, by field and rule only (never the matched text). The viewer must
// be able to read the memory.
func (s *Service) SecretScan(ctx context.Context, p Principal, id uuid.UUID) ([]secrets.Finding, error) {
	m, err := s.Get(ctx, p, id, "")
	if err != nil {
		return nil, err
	}
	return secrets.Scan(map[string]string{"title": m.Title, "content": m.Content, "rationale": m.Rationale, "source_uri": m.SourceURI}), nil
}

// EffectiveRoleOn reports the principal's role on a scope, or "" if none.
func (s *Service) EffectiveRoleOn(ctx context.Context, p Principal, scopeRef string) (Role, Scope, error) {
	var role Role
	var sc Scope
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		d, err := resolveScope(ctx, tx.Q, scopeRef)
		if err != nil {
			return err
		}
		role, err = effectiveRole(ctx, tx.Q, p, d.ID)
		if err != nil {
			return err
		}
		if !role.AtLeast(RoleReader) {
			return ErrNotFound
		}
		sc = toScope(d)
		return nil
	})
	return role, sc, err
}

// DisplayNames resolves principal ids the viewer has already been shown
// (authors, reviewers) to display names. Unknown ids are omitted.
func (s *Service) DisplayNames(ctx context.Context, p Principal, ids []uuid.UUID) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	_ = postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		for _, id := range ids {
			if _, ok := out[id]; ok || id == uuid.Nil {
				continue
			}
			if pr, err := tx.Q.GetPrincipal(ctx, id); err == nil {
				out[id] = pr.DisplayName
			}
		}
		return nil
	})
	return out
}

// IsOrgOwner reports whether the principal is the owner recorded in the
// settings of an organization they can read.
func (s *Service) IsOrgOwner(ctx context.Context, p Principal) bool {
	var ok bool
	_ = postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_settings WHERE owner_principal_id = $1)`, p.ID).Scan(&ok)
	})
	return ok
}
