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

// Review workflow. memory_approve (any authenticated maintainer) only
// requests review of an exact revision. Approval and rejection are trusted
// review services: they require a human principal in a portal session
// (AuthPortal), which MCP and bearer-token API calls never are. RLS on
// approvals and the memories guard trigger enforce the same rule in the
// database. See docs/decisions/trust-rules.md.

// ReviewRequest is the result of RequestReview.
type ReviewRequest struct {
	MemoryID uuid.UUID `json:"memory_id"`
	Revision int       `json:"revision"`
	// Requested is true when this call recorded the request, false when the
	// revision was already requested (the call is idempotent).
	Requested bool `json:"review_requested"`
	// AlreadyRequested reports an earlier request for the same revision.
	AlreadyRequested bool `json:"already_requested"`
	// Approved is always false unless the revision was already approved
	// through browser review; this call never approves.
	Approved bool   `json:"approved"`
	Trust    Trust  `json:"trust"`
	Note     string `json:"note"`
}

const reviewRequestNote = "review requested; approval happens only through explicit maintainer review in the Team Memory portal"

// RequestReview records a review request for an exact revision. It never
// changes trust. Requires maintainer on the owning scope; writers'
// proposals already enter the review queue when created or updated.
// Repeating the request for the same revision is a no-op.
func (s *Service) RequestReview(ctx context.Context, p Principal, id uuid.UUID, revision int) (ReviewRequest, error) {
	if revision < 1 {
		return ReviewRequest{}, invalidf("revision is required")
	}
	out := ReviewRequest{MemoryID: id, Revision: revision, Note: reviewRequestNote}
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		m, err := lockForChange(ctx, tx, p, id, "", RoleMaintainer)
		if err != nil {
			return err
		}
		if Status(m.Status) != StatusActive {
			return ErrNotActive
		}
		if int(m.CurrentRevision) != revision {
			return &ConflictError{MemoryID: m.ID, ExpectedRevision: revision, CurrentRevision: int(m.CurrentRevision)}
		}
		out.Trust = Trust(m.Trust)
		if Trust(m.Trust) == TrustApproved {
			out.Approved = true
			return nil
		}
		n, err := tx.Q.InsertReviewRequest(ctx, db.InsertReviewRequestParams{
			OrgID: m.OrgID, ScopeID: m.ScopeID, MemoryID: m.ID, Revision: m.CurrentRevision, ActorID: p.ID,
			Snapshot: eventSnapshot(m, map[string]any{"requested_by": p.ID.String()}),
		})
		if err != nil {
			return mapDBError(err)
		}
		out.Requested, out.AlreadyRequested = n == 1, n == 0
		if n == 0 {
			return nil
		}
		return audit(ctx, tx, m.OrgID, p.ID, "memory.review_request", "memory", m.ID.String(), &m.ScopeID,
			map[string]any{"revision": revision})
	})
	if err != nil {
		s.auditDenial(ctx, p, "memory.review_request", "memory", id.String(), err)
	}
	return out, err
}

// ReviewInput identifies the exact revision a reviewer looked at.
type ReviewInput struct {
	ID       uuid.UUID
	Revision int
	// ExpectedStateVersion, when set, must match: it detects a lifecycle
	// change (for example another reviewer's decision) since the review
	// page was loaded.
	ExpectedStateVersion int64
	Note                 string
}

// Approve records a maintainer's approval of an exact revision, including
// any override links that revision carries, and sets trust to approved.
// Trusted review service: requires a human in a portal session with
// maintainer on the owning scope. Approving an already approved revision is
// a no-op.
func (s *Service) Approve(ctx context.Context, p Principal, in ReviewInput) (Memory, error) {
	return s.decide(ctx, p, in, "approved")
}

// Reject records a revision-bound rejection. The memory stays visible as
// unapproved knowledge (archive it to hide it) and leaves the review queue
// until a new revision is made. Rejecting the approved current revision
// withdraws the approval: trust returns to cited or unverified, and an
// override the revision carries stops taking effect.
func (s *Service) Reject(ctx context.Context, p Principal, in ReviewInput) (Memory, error) {
	return s.decide(ctx, p, in, "rejected")
}

func (s *Service) decide(ctx context.Context, p Principal, in ReviewInput, decision string) (Memory, error) {
	action := "memory.approve"
	if decision == "rejected" {
		action = "memory.reject"
	}
	if in.Revision < 1 {
		return Memory{}, invalidf("revision is required")
	}
	if err := checkText("note", in.Note, 0, 2000); err != nil {
		return Memory{}, err
	}
	if !p.browserReview() {
		s.auditDenial(ctx, p, action, "memory", in.ID.String(), ErrBrowserReviewRequired)
		return Memory{}, ErrBrowserReviewRequired
	}
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		m, err := lockForChange(ctx, tx, p, in.ID, "", RoleMaintainer)
		if err != nil {
			return err
		}
		if Status(m.Status) != StatusActive {
			return ErrNotActive
		}
		if int(m.CurrentRevision) != in.Revision {
			return &ConflictError{MemoryID: m.ID, ExpectedRevision: in.Revision, CurrentRevision: int(m.CurrentRevision)}
		}
		if in.ExpectedStateVersion != 0 && m.StateVersion != in.ExpectedStateVersion {
			return &ConflictError{MemoryID: m.ID, ExpectedStateVersion: in.ExpectedStateVersion, CurrentStateVersion: m.StateVersion}
		}
		latest, err := tx.Q.LatestReviewOutcome(ctx, db.LatestReviewOutcomeParams{MemoryID: m.ID, Revision: m.CurrentRevision})
		switch {
		case err == nil && latest.Decision == decision:
			return nil // idempotent
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if decision == "approved" {
			// Recheck the stored content: scanning rules may have changed
			// since the revision was written, and approval widens trust.
			cur, err := tx.Q.GetMemoryView(ctx, m.ID)
			if err != nil {
				return err
			}
			c := content{title: cur.Title, content: cur.Content, rationale: cur.Rationale, sourceURI: deref(cur.SourceUri)}
			if err := c.scan(); err != nil {
				return err
			}
		}
		nextTrust := Trust(m.Trust)
		switch {
		case decision == "approved":
			nextTrust = TrustApproved
		case Trust(m.Trust) == TrustApproved:
			cur, err := tx.Q.GetMemoryView(ctx, m.ID)
			if err != nil {
				return err
			}
			nextTrust = trustFor(deref(cur.SourceUri))
		}
		evType := "approved"
		if decision == "rejected" {
			evType = "review_rejected"
		}
		snapshot := eventSnapshot(m, map[string]any{"trust": nextTrust, "reviewer": p.ID.String(), "decision": decision})
		ev, err := tx.Q.InsertMemoryEvent(ctx, db.InsertMemoryEventParams{
			OrgID: m.OrgID, ScopeID: m.ScopeID, MemoryID: m.ID, Revision: m.CurrentRevision,
			EventType: evType, ActorID: p.ID, Snapshot: snapshot,
		})
		if err != nil {
			return mapDBError(err)
		}
		if _, err := tx.Q.InsertApproval(ctx, db.InsertApprovalParams{
			OrgID: m.OrgID, ScopeID: m.ScopeID, MemoryID: m.ID, Revision: m.CurrentRevision, EventID: ev.ID,
			Decision: decision, ReviewerID: p.ID, Note: in.Note,
		}); err != nil {
			return mapDBError(err)
		}
		if nextTrust != Trust(m.Trust) {
			if _, err := tx.Q.SetMemoryTrust(ctx, db.SetMemoryTrustParams{ID: m.ID, Trust: string(nextTrust)}); err != nil {
				return mapDBError(err)
			}
		}
		return audit(ctx, tx, m.OrgID, p.ID, action, "memory", m.ID.String(), &m.ScopeID,
			map[string]any{"revision": in.Revision, "event_id": ev.ID})
	})
	if err != nil {
		s.auditDenial(ctx, p, action, "memory", in.ID.String(), err)
		return Memory{}, err
	}
	return s.Get(ctx, p, in.ID, "")
}

// QueueItem is one memory awaiting review.
type QueueItem struct {
	ID               uuid.UUID `json:"id"`
	OriginScope      string    `json:"origin_scope"`
	Kind             Kind      `json:"kind"`
	Trust            Trust     `json:"trust"`
	Revision         int       `json:"revision"`
	StateVersion     int64     `json:"state_version"`
	Title            string    `json:"title"`
	CreatedBy        uuid.UUID `json:"created_by"`
	RevisedBy        uuid.UUID `json:"revised_by"`
	UpdatedAt        time.Time `json:"updated_at"`
	Requested        bool      `json:"requested"`
	ProposesOverride bool      `json:"proposes_override"`
}

// ReviewQueue lists the unreviewed current revisions owned by one scope.
// Requires maintainer on it.
func (s *Service) ReviewQueue(ctx context.Context, p Principal, scopeRef string, limit int) ([]QueueItem, error) {
	var out []QueueItem
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
		rows, err := tx.Q.ReviewQueue(ctx, db.ReviewQueueParams{ScopeIds: []uuid.UUID{sc.ID}, MaxResults: int32(ClampLimit(limit))})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, QueueItem{ID: r.ID, OriginScope: r.ScopePath, Kind: Kind(r.Kind), Trust: Trust(r.Trust),
				Revision: int(r.CurrentRevision), StateVersion: r.StateVersion, Title: r.Title, CreatedBy: r.CreatedBy,
				RevisedBy: r.ChangedBy, UpdatedAt: r.UpdatedAt, Requested: r.Requested, ProposesOverride: r.ProposesOverride})
		}
		return nil
	})
	return out, err
}
