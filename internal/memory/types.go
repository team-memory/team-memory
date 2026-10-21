// Package memory is the Team Memory engine: scopes, access rules and the
// memory lifecycle. It must not depend on any transport adapter (MCP, HTTP).
package memory

import (
	"time"

	"github.com/google/uuid"
)

// Principal is an authenticated actor. Adapters construct it from a verified
// credential; the memory service never authenticates by itself.
type Principal struct {
	ID          uuid.UUID `json:"id"`
	Kind        string    `json:"kind"` // human | service
	DisplayName string    `json:"display_name"`
	// AuthMethod records how the principal authenticated (e.g. "pat").
	// Stage 04 uses it to keep approval out of reach of token-based calls.
	AuthMethod string `json:"auth_method"`
}

type Kind string

const (
	KindFact       Kind = "fact"
	KindDecision   Kind = "decision"
	KindConvention Kind = "convention"
	KindLesson     Kind = "lesson"
	KindContext    Kind = "context"
)

// Kinds lists the allowed memory kinds in display order.
var Kinds = []Kind{KindFact, KindDecision, KindConvention, KindLesson, KindContext}

type Status string

const (
	StatusActive     Status = "active"
	StatusSuperseded Status = "superseded"
	StatusArchived   Status = "archived"
)

type Trust string

const (
	TrustUnverified Trust = "unverified"
	// TrustCited means a source URI was supplied and passed syntax/scheme
	// validation. It does not mean the source was fetched or supports the claim.
	TrustCited Trust = "cited"
	// TrustApproved means a maintainer reviewed this exact revision in the
	// browser (stage 05). It is never set through MCP.
	TrustApproved Trust = "approved"
)

type Role string

const (
	RoleReader     Role = "reader"
	RoleWriter     Role = "writer"
	RoleMaintainer Role = "maintainer"
)

func (r Role) rank() int {
	switch r {
	case RoleReader:
		return 1
	case RoleWriter:
		return 2
	case RoleMaintainer:
		return 3
	}
	return 0
}

// AtLeast reports whether r grants at least the permissions of min.
func (r Role) AtLeast(min Role) bool { return r.rank() >= min.rank() && r.rank() > 0 }

// ParseRole validates a role name.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if r.rank() == 0 {
		return "", invalidf("role must be reader, writer or maintainer")
	}
	return r, nil
}

// Scope is a node in an organization's scope tree.
type Scope struct {
	ID       uuid.UUID  `json:"id"`
	OrgID    uuid.UUID  `json:"org_id"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	Level    string     `json:"level"`
	Slug     string     `json:"slug"`
	Name     string     `json:"name"`
	Depth    int        `json:"depth"`
	Path     string     `json:"path"`
}

// VisibleScope is a scope whose memories are visible from a requested scope,
// with Inherited set for ancestors.
type VisibleScope struct {
	Scope
	Distance  int  `json:"distance"`
	Inherited bool `json:"inherited"`
}

// OrgSettings holds per-organization tree configuration.
type OrgSettings struct {
	OrgID          uuid.UUID `json:"org_id"`
	AllowedLevels  []string  `json:"allowed_levels"`
	MaxDepth       int       `json:"max_depth"`
	OwnerPrincipal uuid.UUID `json:"owner_principal_id"`
}

// Memory is the current state of a memory as seen from a requested scope.
type Memory struct {
	ID             uuid.UUID   `json:"id"`
	ScopeID        uuid.UUID   `json:"scope_id"`
	OriginScope    string      `json:"origin_scope"`
	Inherited      bool        `json:"inherited"`
	Kind           Kind        `json:"kind"`
	Status         Status      `json:"status"`
	Trust          Trust       `json:"trust"`
	Revision       int         `json:"revision"`
	StateVersion   int64       `json:"state_version"`
	Title          string      `json:"title"`
	Content        string      `json:"content"`
	Rationale      string      `json:"rationale,omitempty"`
	SourceURI      string      `json:"source_uri,omitempty"`
	Overrides      []uuid.UUID `json:"overrides,omitempty"`
	SupersededBy   *uuid.UUID  `json:"superseded_by,omitempty"`
	PromotedFrom   *uuid.UUID  `json:"promoted_from,omitempty"`
	CreatedBy      uuid.UUID   `json:"created_by"`
	CreatedByName  string      `json:"created_by_name,omitempty"`
	RevisedBy      uuid.UUID   `json:"revised_by"`
	RevisedByName  string      `json:"revised_by_name,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	OverrideStatus string      `json:"override_status,omitempty"`
}

// SearchResult is one hit. Rank is a full-text rank from PostgreSQL
// (ts_rank_cd); it orders results but is not a calibrated probability.
type SearchResult struct {
	ID          uuid.UUID `json:"id"`
	OriginScope string    `json:"origin_scope"`
	Inherited   bool      `json:"inherited"`
	Kind        Kind      `json:"kind"`
	Status      Status    `json:"status"`
	Trust       Trust     `json:"trust"`
	Revision    int       `json:"revision"`
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	Rationale   string    `json:"rationale,omitempty"`
	SourceURI   string    `json:"source_uri,omitempty"`
	CreatedBy   uuid.UUID `json:"created_by"`
	UpdatedAt   time.Time `json:"updated_at"`
	Rank        float32   `json:"rank,omitempty"`
}

// Revision is an immutable content snapshot.
type Revision struct {
	Revision   int         `json:"revision"`
	Kind       Kind        `json:"kind"`
	Title      string      `json:"title"`
	Content    string      `json:"content"`
	Rationale  string      `json:"rationale,omitempty"`
	SourceURI  string      `json:"source_uri,omitempty"`
	Overrides  []uuid.UUID `json:"overrides,omitempty"`
	ChangedBy  uuid.UUID   `json:"changed_by"`
	ChangeNote string      `json:"change_note,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
}

// Event is an immutable lifecycle event bound to a revision.
type Event struct {
	ID        uuid.UUID      `json:"id"`
	Seq       int64          `json:"seq"`
	Revision  int            `json:"revision"`
	Type      string         `json:"type"`
	ActorID   uuid.UUID      `json:"actor_id"`
	Snapshot  map[string]any `json:"snapshot"`
	CreatedAt time.Time      `json:"created_at"`
}

// History separates content revisions from lifecycle events.
type History struct {
	Memory    Memory     `json:"memory"`
	Revisions []Revision `json:"revisions"`
	Events    []Event    `json:"events"`
}
