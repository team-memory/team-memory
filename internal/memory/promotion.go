package memory

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// Promotion moves an approved memory up exactly one level, with consent on
// both sides (review-decisions.md, "Promotion consent and visibility"):
//
//  1. A source maintainer submits an approved revision in a browser review,
//     consenting to share that snapshot with the parent scope's reviewers.
//  2. A destination maintainer accepts it in a browser review. Finalization
//     creates a parent memory from the snapshot with a destination approval,
//     and supersedes the source with reason "promotion", atomically.
//
// Destination reviewers see only the request snapshot. They never gain
// read access to the source scope; the source supersession runs through the
// narrow definer function tm_promotion_finalize_source, which rechecks the
// whole contract in SQL.

// PromotionSnapshot is exactly what destination reviewers may see.
type PromotionSnapshot struct {
	Kind        Kind      `json:"kind"`
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	Rationale   string    `json:"rationale,omitempty"`
	SourceURI   string    `json:"source_uri,omitempty"`
	Revision    int       `json:"revision"`
	SourceScope string    `json:"source_scope"`
	SubmittedBy string    `json:"submitted_by_name"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// PromotionRequest is a request as its participants see it. Source
// identifiers are present only for readers of the source scope.
type PromotionRequest struct {
	ID               uuid.UUID         `json:"id"`
	Status           string            `json:"status"`
	DestinationScope uuid.UUID         `json:"destination_scope_id"`
	SourceMemoryID   *uuid.UUID        `json:"source_memory_id,omitempty"`
	SourceScopeID    *uuid.UUID        `json:"source_scope_id,omitempty"`
	SubmittedBy      uuid.UUID         `json:"submitted_by"`
	Rationale        string            `json:"rationale,omitempty"`
	Snapshot         PromotionSnapshot `json:"snapshot"`
	DecidedBy        *uuid.UUID        `json:"decided_by,omitempty"`
	DecidedAt        *time.Time        `json:"decided_at,omitempty"`
	ResultMemoryID   *uuid.UUID        `json:"result_memory_id,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
}

// PromotionInput names the approved source revision to promote.
type PromotionInput struct {
	ID       uuid.UUID
	Revision int
	// Rationale explains to destination reviewers why the knowledge belongs
	// at the parent level.
	Rationale string
}

func (s *Service) promotionView(ctx context.Context, q *db.Queries, p Principal, r db.PromotionRequest) (PromotionRequest, error) {
	out := PromotionRequest{ID: r.ID, Status: r.Status, DestinationScope: r.DestinationScopeID, SubmittedBy: r.SubmittedBy,
		Rationale: r.Rationale, DecidedBy: r.DecidedBy, DecidedAt: r.DecidedAt, ResultMemoryID: r.ResultMemoryID, CreatedAt: r.CreatedAt}
	if err := json.Unmarshal(r.Snapshot, &out.Snapshot); err != nil {
		return out, err
	}
	ok, err := q.CanReadMemoriesOf(ctx, db.CanReadMemoriesOfParams{PrincipalID: p.ID, TargetScopeID: r.SourceScopeID})
	if err != nil {
		return out, err
	}
	if ok {
		out.SourceMemoryID, out.SourceScopeID = &r.SourceMemoryID, &r.SourceScopeID
	}
	return out, nil
}

// SubmitPromotion records a source maintainer's consent to promote an
// approved revision to the parent scope. Trusted review service (portal
// only). Submitting the same revision again returns the live request.
func (s *Service) SubmitPromotion(ctx context.Context, p Principal, in PromotionInput) (PromotionRequest, error) {
	if in.Revision < 1 {
		return PromotionRequest{}, invalidf("revision is required")
	}
	if err := checkText("rationale", in.Rationale, 0, 2000); err != nil {
		return PromotionRequest{}, err
	}
	if !p.browserReview() {
		s.auditDenial(ctx, p, "promotion.submit", "memory", in.ID.String(), ErrBrowserReviewRequired)
		return PromotionRequest{}, ErrBrowserReviewRequired
	}
	var out PromotionRequest
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
		if Trust(m.Trust) != TrustApproved {
			return invalidf("only an approved revision can be promoted; approve revision %d first", in.Revision)
		}
		if live, err := tx.Q.GetLivePromotionRequest(ctx, db.GetLivePromotionRequestParams{
			SourceMemoryID: m.ID, SourceRevision: m.CurrentRevision}); err == nil {
			out, err = s.promotionView(ctx, tx.Q, p, live)
			return err
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		src, err := tx.Q.GetScopeByID(ctx, m.ScopeID)
		if err != nil {
			return err
		}
		if src.ParentID == nil {
			return invalidf("an organization's memories have no parent scope to be promoted to")
		}
		cur, err := tx.Q.GetMemoryView(ctx, m.ID)
		if err != nil {
			return err
		}
		c := content{kind: Kind(cur.Kind), title: cur.Title, content: cur.Content, rationale: cur.Rationale,
			sourceURI: deref(cur.SourceUri), changeNote: in.Rationale}
		if err := c.scan(); err != nil {
			return err
		}
		snap, err := json.Marshal(PromotionSnapshot{Kind: c.kind, Title: c.title, Content: c.content, Rationale: c.rationale,
			SourceURI: c.sourceURI, Revision: in.Revision, SourceScope: src.Path, SubmittedBy: p.DisplayName,
			SubmittedAt: time.Now().UTC()})
		if err != nil {
			return err
		}
		r, err := tx.Q.InsertPromotionRequest(ctx, db.InsertPromotionRequestParams{
			OrgID: m.OrgID, SourceMemoryID: m.ID, SourceRevision: m.CurrentRevision, SourceScopeID: m.ScopeID,
			DestinationScopeID: *src.ParentID, SubmittedBy: p.ID, Rationale: in.Rationale, Snapshot: snap,
		})
		if err != nil {
			return mapDBError(err)
		}
		if err := recordEvent(ctx, tx, m, "promotion_submitted", p.ID, map[string]any{
			"promotion_request": r.ID.String(), "destination_scope": src.ParentID.String()}); err != nil {
			return mapDBError(err)
		}
		if err := audit(ctx, tx, m.OrgID, p.ID, "promotion.submit", "promotion_request", r.ID.String(), &m.ScopeID,
			map[string]any{"memory": m.ID, "revision": in.Revision, "destination_scope": src.ParentID}); err != nil {
			return err
		}
		out, err = s.promotionView(ctx, tx.Q, p, r)
		return err
	})
	if err != nil {
		s.auditDenial(ctx, p, "promotion.submit", "memory", in.ID.String(), err)
	}
	return out, err
}

// FinalizePromotion accepts a pending request as a destination maintainer
// (portal only) and returns the new parent memory. It is idempotent: an
// accepted request returns its result. A request whose source changed is
// marked stale and ErrPromotionStale is returned.
func (s *Service) FinalizePromotion(ctx context.Context, p Principal, requestID uuid.UUID, note string) (Memory, error) {
	if err := checkText("note", note, 0, 2000); err != nil {
		return Memory{}, err
	}
	if !p.browserReview() {
		s.auditDenial(ctx, p, "promotion.finalize", "promotion_request", requestID.String(), ErrBrowserReviewRequired)
		return Memory{}, ErrBrowserReviewRequired
	}
	var resultID uuid.UUID
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		r, err := tx.Q.LockPromotionRequest(ctx, requestID)
		if err != nil {
			return mapDBError(err)
		}
		if err := requireRole(ctx, tx.Q, p, r.DestinationScopeID, RoleMaintainer); err != nil {
			if errors.Is(err, ErrInherited) || errors.Is(err, ErrNotFound) {
				// Visible as a source maintainer or submitter, but not a
				// destination reviewer.
				return ErrForbidden
			}
			return err
		}
		switch r.Status {
		case "accepted":
			resultID = *r.ResultMemoryID
			return nil
		case "pending":
		default:
			return ErrPromotionClosed
		}
		var snap PromotionSnapshot
		if err := json.Unmarshal(r.Snapshot, &snap); err != nil {
			return err
		}
		// The change note reaches every reader of the destination, most of
		// whom cannot see the source scope, so it does not name it; the
		// snapshot (shown to destination reviewers only) does.
		c := content{kind: snap.Kind, title: snap.Title, content: snap.Content, rationale: snap.Rationale,
			sourceURI: snap.SourceURI, changeNote: "promoted from a child scope"}
		if err := c.validate(); err != nil {
			return err
		}
		rev := r.SourceRevision
		dst, err := tx.Q.InsertPromotedMemory(ctx, db.InsertPromotedMemoryParams{
			OrgID: r.OrgID, ScopeID: r.DestinationScopeID, Kind: string(c.kind), Trust: string(trustFor(c.sourceURI)),
			CreatedBy: p.ID, PromotedFromMemoryID: &r.SourceMemoryID, PromotedFromRevision: &rev,
		})
		if err != nil {
			return mapDBError(err)
		}
		if err := insertRevision(ctx, tx, dst, 1, c, p.ID); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, dst, "promotion_finalized", p.ID, map[string]any{
			"promotion_request": r.ID.String(), "promoted_from_revision": rev}); err != nil {
			return mapDBError(err)
		}
		ev, err := tx.Q.InsertMemoryEvent(ctx, db.InsertMemoryEventParams{
			OrgID: dst.OrgID, ScopeID: dst.ScopeID, MemoryID: dst.ID, Revision: 1, EventType: "approved", ActorID: p.ID,
			Snapshot: eventSnapshot(dst, map[string]any{"trust": TrustApproved, "reviewer": p.ID.String(), "decision": "approved"}),
		})
		if err != nil {
			return mapDBError(err)
		}
		if _, err := tx.Q.InsertApproval(ctx, db.InsertApprovalParams{
			OrgID: dst.OrgID, ScopeID: dst.ScopeID, MemoryID: dst.ID, Revision: 1, EventID: ev.ID,
			Decision: "approved", ReviewerID: p.ID, Note: note,
		}); err != nil {
			return mapDBError(err)
		}
		if _, err := tx.Q.SetMemoryTrust(ctx, db.SetMemoryTrustParams{ID: dst.ID, Trust: string(TrustApproved)}); err != nil {
			return mapDBError(err)
		}
		if err := tx.Q.FinalizePromotionSource(ctx, db.FinalizePromotionSourceParams{RequestID: r.ID, ResultMemoryID: dst.ID}); err != nil {
			return mapPromotionError(err)
		}
		resultID = dst.ID
		return audit(ctx, tx, r.OrgID, p.ID, "promotion.finalize", "promotion_request", r.ID.String(), &r.DestinationScopeID,
			map[string]any{"result_memory": dst.ID, "source_revision": rev})
	})
	if errors.Is(err, ErrPromotionStale) {
		s.markStale(ctx, p, requestID)
	}
	if err != nil {
		s.auditDenial(ctx, p, "promotion.finalize", "promotion_request", requestID.String(), err)
		return Memory{}, err
	}
	return s.Get(ctx, p, resultID, "")
}

func mapPromotionError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "TMS01":
			return ErrPromotionStale
		case "TMS02":
			return ErrPromotionClosed
		case "P0002", "42501": // no_data_found, insufficient_privilege
			return ErrForbidden
		}
	}
	return mapDBError(err)
}

// markStale records that a request can no longer be finalized. It runs
// after the finalization transaction rolled back.
func (s *Service) markStale(ctx context.Context, p Principal, requestID uuid.UUID) {
	err := postgres.WithActorTx(context.WithoutCancel(ctx), s.pool, p.actor(), func(tx postgres.Tx) error {
		r, err := tx.Q.DecidePromotionRequest(ctx, db.DecidePromotionRequestParams{ID: requestID, Status: "stale", DecidedBy: &p.ID})
		if err != nil {
			return err
		}
		return audit(ctx, tx, r.OrgID, p.ID, "promotion.stale", "promotion_request", r.ID.String(), &r.DestinationScopeID, nil)
	})
	if err != nil {
		s.logger().Error("mark promotion request stale", "request", requestID, "err", err)
	}
}

// RejectPromotion declines a pending request as a destination maintainer
// (portal only). The source memory is unaffected.
func (s *Service) RejectPromotion(ctx context.Context, p Principal, requestID uuid.UUID) (PromotionRequest, error) {
	if !p.browserReview() {
		s.auditDenial(ctx, p, "promotion.reject", "promotion_request", requestID.String(), ErrBrowserReviewRequired)
		return PromotionRequest{}, ErrBrowserReviewRequired
	}
	return s.closePromotion(ctx, p, requestID, "rejected", "promotion.reject")
}

// CancelPromotion withdraws a pending request; the submitter or a source
// maintainer may cancel. Cancelling withdraws consent, so it needs no
// browser review.
func (s *Service) CancelPromotion(ctx context.Context, p Principal, requestID uuid.UUID) (PromotionRequest, error) {
	return s.closePromotion(ctx, p, requestID, "cancelled", "promotion.cancel")
}

func (s *Service) closePromotion(ctx context.Context, p Principal, requestID uuid.UUID, status, action string) (PromotionRequest, error) {
	var out PromotionRequest
	err := postgres.WithActorTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		r, err := tx.Q.LockPromotionRequest(ctx, requestID)
		if err != nil {
			return mapDBError(err)
		}
		switch status {
		case "rejected":
			if err := requireRole(ctx, tx.Q, p, r.DestinationScopeID, RoleMaintainer); err != nil {
				return ErrForbidden
			}
		case "cancelled":
			if r.SubmittedBy != p.ID {
				if err := requireRole(ctx, tx.Q, p, r.SourceScopeID, RoleMaintainer); err != nil {
					return ErrForbidden
				}
			}
		}
		if r.Status != "pending" {
			return ErrPromotionClosed
		}
		r, err = tx.Q.DecidePromotionRequest(ctx, db.DecidePromotionRequestParams{ID: r.ID, Status: status, DecidedBy: &p.ID})
		if err != nil {
			return mapPromotionError(err)
		}
		if err := audit(ctx, tx, r.OrgID, p.ID, action, "promotion_request", r.ID.String(), &r.DestinationScopeID, nil); err != nil {
			return err
		}
		out, err = s.promotionView(ctx, tx.Q, p, r)
		return err
	})
	if err != nil {
		s.auditDenial(ctx, p, action, "promotion_request", requestID.String(), err)
	}
	return out, err
}

// GetPromotion returns a request visible to the principal (submitter,
// source maintainer or destination maintainer).
func (s *Service) GetPromotion(ctx context.Context, p Principal, requestID uuid.UUID) (PromotionRequest, error) {
	var out PromotionRequest
	err := postgres.WithActorReadTx(ctx, s.pool, p.actor(), func(tx postgres.Tx) error {
		r, err := tx.Q.GetPromotionRequest(ctx, requestID)
		if err != nil {
			return mapDBError(err)
		}
		out, err = s.promotionView(ctx, tx.Q, p, r)
		return err
	})
	return out, err
}

// PendingPromotions lists requests awaiting a destination maintainer of
// scopeRef.
func (s *Service) PendingPromotions(ctx context.Context, p Principal, scopeRef string) ([]PromotionRequest, error) {
	var out []PromotionRequest
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
		rows, err := tx.Q.ListPromotionRequestsForDestinations(ctx, []uuid.UUID{sc.ID})
		if err != nil {
			return err
		}
		for _, r := range rows {
			v, err := s.promotionView(ctx, tx.Q, p, r)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// PromoteDirect is submission and acceptance as one explicit browser
// action, for a principal who maintains both the source and its parent.
func (s *Service) PromoteDirect(ctx context.Context, p Principal, in PromotionInput) (Memory, error) {
	r, err := s.SubmitPromotion(ctx, p, in)
	if err != nil {
		return Memory{}, err
	}
	return s.FinalizePromotion(ctx, p, r.ID, "")
}
