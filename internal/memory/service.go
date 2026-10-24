package memory

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/secrets"
)

// CreateInput describes a new memory.
type CreateInput struct {
	Scope      string
	Kind       string
	Title      string
	Content    string
	Rationale  string
	SourceURI  string
	ChangeNote string
	// Overrides lists inherited memories this one proposes to override. Links
	// are part of the revision; they take effect only when a maintainer
	// approves that exact revision, and never hide inherited guidance before.
	Overrides []uuid.UUID
}

// UpdateInput creates a new revision. Nil fields keep the current value.
type UpdateInput struct {
	ID               uuid.UUID
	ExpectedRevision int
	// Scope, when set, is the scope the caller is working in. Changing a
	// memory owned by an ancestor from a descendant scope is rejected.
	Scope      string
	Kind       *string
	Title      *string
	Content    *string
	Rationale  *string
	SourceURI  *string
	Overrides  *[]uuid.UUID
	ChangeNote string
}

// SupersedeInput replaces a memory with a new one in the same scope.
type SupersedeInput struct {
	ID               uuid.UUID
	ExpectedRevision int
	Scope            string
	// Reason is recorded on the original memory's superseded event.
	Reason      string
	Replacement CreateInput
}

// ArchiveInput excludes a memory from default retrieval without deleting history.
type ArchiveInput struct {
	ID                   uuid.UUID
	ExpectedStateVersion int64
	Reason               string
}

func jsonBytes(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func trustFor(sourceURI string) Trust {
	if sourceURI != "" {
		return TrustCited
	}
	return TrustUnverified
}

type content struct {
	kind       Kind
	title      string
	content    string
	rationale  string
	sourceURI  string
	changeNote string
	overrides  []uuid.UUID
}

func (c *content) validate() error {
	c.title = strings.TrimSpace(c.title)
	c.sourceURI = strings.TrimSpace(c.sourceURI)
	if err := checkText("title", c.title, 1, MaxTitleLen); err != nil {
		return err
	}
	if err := checkText("content", c.content, 1, MaxContentLen); err != nil {
		return err
	}
	if strings.TrimSpace(c.content) == "" {
		return invalidf("content is required")
	}
	if err := checkText("rationale", c.rationale, 0, MaxRationaleLen); err != nil {
		return err
	}
	if err := checkText("change_note", c.changeNote, 0, MaxChangeNoteLen); err != nil {
		return err
	}
	if err := ValidateSourceURI(c.sourceURI); err != nil {
		return err
	}
	if len(c.overrides) > MaxOverrides {
		return invalidf("at most %d overrides", MaxOverrides)
	}
	slices.SortFunc(c.overrides, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	c.overrides = slices.Compact(c.overrides)
	return c.scan()
}

// scan rejects content that looks like it contains a credential. Shared
// memory reaches every reader of the scope and its descendants, and later
// the git mirror, so a leaked secret spreads; rejecting at write time is
// the cheapest place to stop it.
func (c *content) scan() error {
	if f := secrets.Scan(map[string]string{
		"title": c.title, "content": c.content, "rationale": c.rationale,
		"source_uri": c.sourceURI, "change_note": c.changeNote,
	}); len(f) > 0 {
		return &SecretError{Findings: f}
	}
	return nil
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// requireRole loads the effective role on a scope; it hides scopes the
// principal cannot read behind ErrNotFound.
func requireRole(ctx context.Context, q *db.Queries, p Principal, scopeID uuid.UUID, min Role) error {
	role, err := effectiveRole(ctx, q, p, scopeID)
	if err != nil {
		return err
	}
	if !role.AtLeast(min) {
		if role.AtLeast(RoleReader) {
			return ErrForbidden
		}
		readable, err := q.CanReadMemoriesOf(ctx, db.CanReadMemoriesOfParams{PrincipalID: p.ID, TargetScopeID: scopeID})
		if err != nil {
			return err
		}
		if readable {
			// Visible through inheritance, but owned by an ancestor scope.
			return ErrInherited
		}
		return ErrNotFound
	}
	return nil
}

// checkOverrides requires each overridden memory to be owned by a strict
// ancestor of the overriding memory's scope. Inherited memories visible from
// the scope are exactly those owned by ancestors.
func checkOverrides(ctx context.Context, q *db.Queries, scopeID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	ancestors, err := q.ListAncestors(ctx, scopeID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		target, err := q.GetMemoryView(ctx, id)
		if err != nil {
			return invalidf("override target %s is not an inherited memory of this scope", id)
		}
		ok := false
		for _, a := range ancestors {
			if a.Distance > 0 && a.ID == target.ScopeID {
				ok = true
			}
		}
		if !ok {
			return invalidf("override target %s is not an inherited memory of this scope", id)
		}
	}
	return nil
}

func eventSnapshot(m db.Memory, extra map[string]any) []byte {
	snap := map[string]any{
		"status":        m.Status,
		"trust":         m.Trust,
		"kind":          m.Kind,
		"revision":      m.CurrentRevision,
		"state_version": m.StateVersion,
	}
	if m.SupersededBy != nil {
		snap["superseded_by"] = m.SupersededBy.String()
	}
	for k, v := range extra {
		snap[k] = v
	}
	b, _ := json.Marshal(snap)
	return b
}

func recordEvent(ctx context.Context, tx postgres.Tx, m db.Memory, typ string, actor uuid.UUID, extra map[string]any) error {
	_, err := tx.Q.InsertMemoryEvent(ctx, db.InsertMemoryEventParams{
		OrgID: m.OrgID, ScopeID: m.ScopeID, MemoryID: m.ID, Revision: m.CurrentRevision,
		EventType: typ, ActorID: actor, Snapshot: eventSnapshot(m, withPublicAttribution(ctx, extra)),
	})
	return err
}

func insertRevision(ctx context.Context, tx postgres.Tx, m db.Memory, rev int32, c content, actor uuid.UUID) error {
	if _, err := tx.Q.InsertRevision(ctx, db.InsertRevisionParams{
		MemoryID: m.ID, Revision: rev, OrgID: m.OrgID, ScopeID: m.ScopeID, Kind: string(c.kind),
		Title: c.title, Content: c.content, Rationale: c.rationale, SourceUri: stringPtr(c.sourceURI),
		ChangedBy: actor, ChangeNote: c.changeNote, Attribution: attributionJSON(ctx),
	}); err != nil {
		return mapDBError(err)
	}
	for _, o := range c.overrides {
		if err := tx.Q.InsertRevisionOverride(ctx, db.InsertRevisionOverrideParams{
			MemoryID: m.ID, Revision: rev, OrgID: m.OrgID, ScopeID: m.ScopeID, OverriddenMemoryID: o,
		}); err != nil {
			return mapDBError(err)
		}
	}
	// Transactional outbox: the embedding job commits or rolls back with the
	// revision, so no revision is ever left without a job.
	return tx.Q.EnqueueEmbedJobs(ctx, db.EnqueueEmbedJobsParams{OrgID: m.OrgID, MemoryID: m.ID, Revision: rev})
}

func createInTx(ctx context.Context, tx postgres.Tx, p Principal, sc db.Scope, c content) (db.Memory, error) {
	if err := checkOverrides(ctx, tx.Q, sc.ID, c.overrides); err != nil {
		return db.Memory{}, err
	}
	m, err := tx.Q.InsertMemory(ctx, db.InsertMemoryParams{
		OrgID: sc.OrgID, ScopeID: sc.ID, Kind: string(c.kind), Trust: string(trustFor(c.sourceURI)), CreatedBy: p.ID,
	})
	if err != nil {
		return db.Memory{}, mapDBError(err)
	}
	if err := insertRevision(ctx, tx, m, 1, c, p.ID); err != nil {
		return db.Memory{}, err
	}
	if err := recordEvent(ctx, tx, m, "created", p.ID, nil); err != nil {
		return db.Memory{}, err
	}
	return m, audit(ctx, tx, m.OrgID, p.ID, "memory.create", "memory", m.ID.String(), &m.ScopeID, map[string]any{"revision": m.CurrentRevision})
}

// Create records a new memory in a scope. Requires writer on the scope.
func (s *Service) Create(ctx context.Context, p Principal, in CreateInput) (Memory, error) {
	kind, err := ParseKind(in.Kind)
	if err != nil {
		return Memory{}, err
	}
	c := content{kind: kind, title: in.Title, content: in.Content, rationale: in.Rationale,
		sourceURI: in.SourceURI, changeNote: in.ChangeNote, overrides: slices.Clone(in.Overrides)}
	if err := c.validate(); err != nil {
		return Memory{}, err
	}
	var id uuid.UUID
	var scopeID uuid.UUID
	err = postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		sc, err := resolveScope(ctx, tx.Q, in.Scope)
		if err != nil {
			return err
		}
		if err := requireRole(ctx, tx.Q, p, sc.ID, RoleWriter); err != nil {
			if err == ErrInherited {
				return ErrForbidden
			}
			return err
		}
		m, err := createInTx(ctx, tx, p, sc, c)
		id, scopeID = m.ID, sc.ID
		return err
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.create", "scope", in.Scope, err)
		return Memory{}, err
	}
	return s.getAs(ctx, p, id, scopeID)
}

// lockForChange checks that the principal may change a memory from the
// requested scope with the given role, then locks it. Access is checked
// before locking so callers cannot hold locks on memories they cannot see.
// A memory's owning scope never changes, so checking before the lock is safe.
func lockForChange(ctx context.Context, tx postgres.Tx, p Principal, id uuid.UUID, scopeRef string, min Role) (db.Memory, error) {
	owner, err := tx.Q.GetMemoryScope(ctx, id)
	if err != nil {
		return db.Memory{}, mapDBError(err)
	}
	if err := requireRole(ctx, tx.Q, p, owner, min); err != nil {
		return db.Memory{}, err
	}
	if scopeRef != "" {
		if err := checkWorkingScope(ctx, tx.Q, p, owner, scopeRef); err != nil {
			return db.Memory{}, err
		}
	}
	m, err := tx.Q.LockMemory(ctx, id)
	if err != nil {
		return db.Memory{}, mapDBError(err)
	}
	return m, nil
}

// checkWorkingScope rejects changes made from a scope other than the owning
// scope. It reports ErrInherited only when the memory is genuinely inherited
// into a scope the caller can read; any other scope reference, existing or
// not, is ErrNotFound so the response never reveals whether it exists.
func checkWorkingScope(ctx context.Context, q *db.Queries, p Principal, owner uuid.UUID, scopeRef string) error {
	sc, err := resolveScope(ctx, q, scopeRef)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			return err
		}
		return ErrNotFound
	}
	if sc.ID == owner {
		return nil
	}
	role, err := effectiveRole(ctx, q, p, sc.ID)
	if err != nil {
		return err
	}
	if !role.AtLeast(RoleReader) {
		return ErrNotFound
	}
	ancestors, err := q.ListAncestors(ctx, sc.ID)
	if err != nil {
		return err
	}
	for _, a := range ancestors {
		if a.ID == owner {
			return ErrInherited
		}
	}
	return ErrNotFound
}

// Update appends a new immutable revision. ExpectedRevision must match the
// current revision; otherwise a *ConflictError is returned. A new revision
// resets trust, because approval is bound to an exact revision: an approved
// memory becomes cited or unverified until its new revision is approved,
// and any override it carries stops taking effect until then.
func (s *Service) Update(ctx context.Context, p Principal, in UpdateInput) (Memory, error) {
	if in.ExpectedRevision < 1 {
		return Memory{}, invalidf("expected_revision is required")
	}
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		m, err := lockForChange(ctx, tx, p, in.ID, in.Scope, RoleWriter)
		if err != nil {
			return err
		}
		if Status(m.Status) != StatusActive {
			return ErrNotActive
		}
		if int(m.CurrentRevision) != in.ExpectedRevision {
			return &ConflictError{MemoryID: m.ID, ExpectedRevision: in.ExpectedRevision, CurrentRevision: int(m.CurrentRevision)}
		}
		cur, err := tx.Q.GetMemoryView(ctx, m.ID)
		if err != nil {
			return err
		}
		curOverrides, err := overridesAt(ctx, tx.Q, m.ID, m.CurrentRevision)
		if err != nil {
			return err
		}
		c := content{kind: Kind(cur.Kind), title: cur.Title, content: cur.Content, rationale: cur.Rationale,
			sourceURI: deref(cur.SourceUri), changeNote: in.ChangeNote, overrides: curOverrides}
		if in.Kind != nil {
			if c.kind, err = ParseKind(*in.Kind); err != nil {
				return err
			}
		}
		if in.Title != nil {
			c.title = *in.Title
		}
		if in.Content != nil {
			c.content = *in.Content
		}
		if in.Rationale != nil {
			c.rationale = *in.Rationale
		}
		if in.SourceURI != nil {
			c.sourceURI = *in.SourceURI
		}
		if in.Overrides != nil {
			c.overrides = slices.Clone(*in.Overrides)
		}
		if err := c.validate(); err != nil {
			return err
		}
		if err := checkOverrides(ctx, tx.Q, m.ScopeID, c.overrides); err != nil {
			return err
		}
		next := m.CurrentRevision + 1
		if err := insertRevision(ctx, tx, m, next, c, p.ID); err != nil {
			return err
		}
		m, err = tx.Q.SetMemoryRevision(ctx, db.SetMemoryRevisionParams{
			ID: m.ID, Revision: next, Kind: string(c.kind), Trust: string(trustFor(c.sourceURI)),
		})
		if err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, m, "revised", p.ID, nil); err != nil {
			return err
		}
		return audit(ctx, tx, m.OrgID, p.ID, "memory.update", "memory", m.ID.String(), &m.ScopeID,
			map[string]any{"revision": next})
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.update", "memory", in.ID.String(), err)
		return Memory{}, err
	}
	return s.Get(ctx, p, in.ID, in.Scope)
}

// Supersede creates a replacement memory in the same scope and marks the
// original superseded, linking the two. Requires maintainer on the owning
// scope: superseding retires guidance other people rely on.
func (s *Service) Supersede(ctx context.Context, p Principal, in SupersedeInput) (old, replacement Memory, err error) {
	if in.ExpectedRevision < 1 {
		return Memory{}, Memory{}, invalidf("expected_revision is required")
	}
	if err := checkText("reason", in.Reason, 0, MaxChangeNoteLen); err != nil {
		return Memory{}, Memory{}, err
	}
	reason := in.Reason
	if reason == "" {
		reason = "replaced"
	}
	var newID uuid.UUID
	err = postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		m, err := lockForChange(ctx, tx, p, in.ID, in.Scope, RoleMaintainer)
		if err != nil {
			return err
		}
		if Status(m.Status) != StatusActive {
			return ErrNotActive
		}
		if int(m.CurrentRevision) != in.ExpectedRevision {
			return &ConflictError{MemoryID: m.ID, ExpectedRevision: in.ExpectedRevision, CurrentRevision: int(m.CurrentRevision)}
		}
		r := in.Replacement
		if r.Kind == "" {
			r.Kind = m.Kind
		}
		kind, err := ParseKind(r.Kind)
		if err != nil {
			return err
		}
		c := content{kind: kind, title: r.Title, content: r.Content, rationale: r.Rationale,
			sourceURI: r.SourceURI, changeNote: r.ChangeNote, overrides: slices.Clone(r.Overrides)}
		if err := c.validate(); err != nil {
			return err
		}
		sc, err := tx.Q.GetScopeByID(ctx, m.ScopeID)
		if err != nil {
			return err
		}
		repl, err := createInTx(ctx, tx, p, sc, c)
		if err != nil {
			return err
		}
		newID = repl.ID
		m, err = tx.Q.SetMemorySuperseded(ctx, db.SetMemorySupersededParams{ID: m.ID, SupersededBy: &repl.ID})
		if err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, m, "superseded", p.ID, map[string]any{"reason": reason, "replacement": repl.ID.String()}); err != nil {
			return err
		}
		return audit(ctx, tx, m.OrgID, p.ID, "memory.supersede", "memory", m.ID.String(), &m.ScopeID,
			map[string]any{"replacement": repl.ID})
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.supersede", "memory", in.ID.String(), err)
		return Memory{}, Memory{}, err
	}
	if old, err = s.Get(ctx, p, in.ID, ""); err != nil {
		return Memory{}, Memory{}, err
	}
	replacement, err = s.Get(ctx, p, newID, "")
	return old, replacement, err
}

// Archive excludes a memory from default retrieval. History is kept.
// Requires maintainer on the owning scope. Not exposed through MCP in v1.
func (s *Service) Archive(ctx context.Context, p Principal, in ArchiveInput) (Memory, error) {
	if in.ExpectedStateVersion < 1 {
		return Memory{}, invalidf("expected_state_version is required")
	}
	if err := checkText("reason", in.Reason, 0, MaxChangeNoteLen); err != nil {
		return Memory{}, err
	}
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		m, err := lockForChange(ctx, tx, p, in.ID, "", RoleMaintainer)
		if err != nil {
			return err
		}
		if m.StateVersion != in.ExpectedStateVersion {
			return &ConflictError{MemoryID: m.ID, ExpectedStateVersion: in.ExpectedStateVersion, CurrentStateVersion: m.StateVersion}
		}
		if Status(m.Status) != StatusActive {
			return ErrNotActive
		}
		m, err = tx.Q.SetMemoryArchived(ctx, m.ID)
		if err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, m, "archived", p.ID, map[string]any{"reason": in.Reason}); err != nil {
			return err
		}
		return audit(ctx, tx, m.OrgID, p.ID, "memory.archive", "memory", m.ID.String(), &m.ScopeID, nil)
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.archive", "memory", in.ID.String(), err)
		return Memory{}, err
	}
	return s.Get(ctx, p, in.ID, "")
}

func overridesAt(ctx context.Context, q *db.Queries, id uuid.UUID, rev int32) ([]uuid.UUID, error) {
	rows, err := q.ListRevisionOverrides(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, r := range rows {
		if r.Revision == rev {
			out = append(out, r.OverriddenMemoryID)
		}
	}
	return out, nil
}

// Get returns a memory if the principal can read it. scopeRef, when set, is
// the scope the caller is viewing from: the memory must be owned by that
// scope or one of its ancestors, and Inherited reports which.
func (s *Service) Get(ctx context.Context, p Principal, id uuid.UUID, scopeRef string) (Memory, error) {
	var out Memory
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		var viewFrom uuid.UUID
		if scopeRef != "" {
			// Resolved as the principal: an unreadable scope is not found.
			sc, err := resolveScope(ctx, tx.Q, scopeRef)
			if err != nil {
				return err
			}
			viewFrom = sc.ID
		}
		var err error
		out, err = getInTx(ctx, tx.Q, p, id, viewFrom)
		return err
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.get", "memory", id.String(), err)
	}
	return out, err
}

func (s *Service) getAs(ctx context.Context, p Principal, id, viewFrom uuid.UUID) (Memory, error) {
	var out Memory
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		var err error
		out, err = getInTx(ctx, tx.Q, p, id, viewFrom)
		return err
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.get", "memory", id.String(), err)
	}
	return out, err
}

func getInTx(ctx context.Context, q *db.Queries, p Principal, id, viewFrom uuid.UUID) (Memory, error) {
	v, err := q.GetMemoryView(ctx, id)
	if err != nil {
		return Memory{}, mapDBError(err)
	}
	inherited := false
	if viewFrom != uuid.Nil {
		role, err := effectiveRole(ctx, q, p, viewFrom)
		if err != nil {
			return Memory{}, err
		}
		if !role.AtLeast(RoleReader) {
			return Memory{}, ErrNotFound
		}
		ancestors, err := q.ListAncestors(ctx, viewFrom)
		if err != nil {
			return Memory{}, err
		}
		found := false
		for _, a := range ancestors {
			if a.ID == v.ScopeID {
				found, inherited = true, a.Distance > 0
			}
		}
		if !found {
			return Memory{}, ErrNotFound
		}
	} else {
		ok, err := q.CanReadMemoriesOf(ctx, db.CanReadMemoriesOfParams{PrincipalID: p.ID, TargetScopeID: v.ScopeID})
		if err != nil {
			return Memory{}, err
		}
		if !ok {
			return Memory{}, ErrNotFound
		}
		role, err := effectiveRole(ctx, q, p, v.ScopeID)
		if err != nil {
			return Memory{}, err
		}
		inherited = !role.AtLeast(RoleReader)
	}
	overrides, err := overridesAt(ctx, q, v.ID, v.CurrentRevision)
	if err != nil {
		return Memory{}, err
	}
	m := Memory{
		CreatedVia: decodeAttribution(v.CreatedAttribution), RevisedVia: decodeAttribution(v.Attribution),
		ID: v.ID, ScopeID: v.ScopeID, OriginScope: v.ScopePath, Inherited: inherited,
		Kind: Kind(v.Kind), Status: Status(v.Status), Trust: Trust(v.Trust),
		Revision: int(v.CurrentRevision), StateVersion: v.StateVersion,
		Title: v.Title, Content: v.Content, Rationale: v.Rationale, SourceURI: deref(v.SourceUri),
		Overrides: overrides, SupersededBy: v.SupersededBy, PromotedFrom: v.PromotedFromMemoryID,
		CreatedBy: v.CreatedBy, RevisedBy: v.ChangedBy, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
	if v.ReviewDecision != nil && v.ReviewerID != nil && v.ReviewedAt != nil {
		m.Review = &ReviewInfo{Decision: *v.ReviewDecision, Revision: m.Revision, Reviewer: *v.ReviewerID, ReviewedAt: *v.ReviewedAt}
	}
	if len(overrides) > 0 {
		// Links take effect only with an approval of this exact revision.
		m.OverrideStatus = "proposed"
		if m.Trust == TrustApproved && m.Review != nil && m.Review.Decision == "approved" {
			m.OverrideStatus = "approved"
		}
	}
	// Provenance links to memories the reader cannot see are redacted, so a
	// promotion never reveals the source scope's memory to its new audience.
	for _, link := range []**uuid.UUID{&m.PromotedFrom, &m.SupersededBy} {
		if *link == nil {
			continue
		}
		target, err := q.GetMemoryScope(ctx, **link)
		if err != nil {
			*link = nil
			continue
		}
		ok, err := q.CanReadMemoriesOf(ctx, db.CanReadMemoriesOfParams{PrincipalID: p.ID, TargetScopeID: target})
		if err != nil {
			return Memory{}, err
		}
		if !ok {
			*link = nil
		}
	}
	if pr, err := q.GetPrincipal(ctx, v.CreatedBy); err == nil {
		m.CreatedByName = pr.DisplayName
	}
	if pr, err := q.GetPrincipal(ctx, v.ChangedBy); err == nil {
		m.RevisedByName = pr.DisplayName
	}
	return m, nil
}

// History returns every content revision and lifecycle event of a memory.
func (s *Service) History(ctx context.Context, p Principal, id uuid.UUID) (History, error) {
	var out History
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		m, err := getInTx(ctx, tx.Q, p, id, uuid.Nil)
		if err != nil {
			return err
		}
		out.Memory = m
		revs, err := tx.Q.ListRevisions(ctx, id)
		if err != nil {
			return err
		}
		ovr, err := tx.Q.ListRevisionOverrides(ctx, id)
		if err != nil {
			return err
		}
		for _, r := range revs {
			rev := Revision{
				Attribution: decodeAttribution(r.Attribution),
				Revision:    int(r.Revision), Kind: Kind(r.Kind), Title: r.Title, Content: r.Content,
				Rationale: r.Rationale, SourceURI: deref(r.SourceUri), ChangedBy: r.ChangedBy,
				ChangeNote: r.ChangeNote, CreatedAt: r.CreatedAt,
			}
			for _, o := range ovr {
				if o.Revision == r.Revision {
					rev.Overrides = append(rev.Overrides, o.OverriddenMemoryID)
				}
			}
			out.Revisions = append(out.Revisions, rev)
		}
		evs, err := tx.Q.ListMemoryEvents(ctx, id)
		if err != nil {
			return err
		}
		for _, e := range evs {
			ev := Event{ID: e.ID, Seq: e.Seq, Revision: int(e.Revision), Type: e.EventType, ActorID: e.ActorID, CreatedAt: e.CreatedAt}
			_ = json.Unmarshal(e.Snapshot, &ev.Snapshot)
			out.Events = append(out.Events, ev)
		}
		return nil
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.history", "memory", id.String(), err)
	}
	return out, err
}

func scopeIDs(vs []VisibleScope) ([]uuid.UUID, map[uuid.UUID]bool) {
	ids := make([]uuid.UUID, 0, len(vs))
	inherited := make(map[uuid.UUID]bool, len(vs))
	for _, v := range vs {
		ids = append(ids, v.ID)
		inherited[v.ID] = v.Inherited
	}
	return ids, inherited
}

// Recent lists recently changed active memories in the scope and its ancestors.
func (s *Service) Recent(ctx context.Context, p Principal, scopeRef string, kinds []string, limit int) ([]SearchResult, error) {
	ks, err := ParseKinds(kinds)
	if err != nil {
		return nil, err
	}
	out := []SearchResult{}
	err = postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		vs, err := visibleScopes(ctx, tx.Q, p, scopeRef)
		if err != nil {
			return err
		}
		ids, inherited := scopeIDs(vs)
		rows, err := tx.Q.RecentMemories(ctx, db.RecentMemoriesParams{
			ScopeIds: ids, Kinds: ks, MaxResults: int32(ClampLimit(limit)),
		})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, SearchResult{
				ID: r.ID, OriginScope: r.ScopePath, Inherited: inherited[r.ScopeID], Kind: Kind(r.Kind),
				Status: Status(r.Status), Trust: Trust(r.Trust), Revision: int(r.CurrentRevision),
				Title: r.Title, Content: r.Content, Rationale: r.Rationale, SourceURI: deref(r.SourceUri),
				CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt, ApprovedBy: r.ReviewerID, ApprovedAt: r.ReviewedAt,
			})
		}
		return nil
	})
	return out, err
}
