package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

type requestIDKey struct{}

// WithRequestID attaches the transport's request ID to ctx; audit events
// written while handling the request carry it.
func WithRequestID(ctx context.Context, id string) context.Context {
	if len(id) > 100 {
		id = id[:100]
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request ID attached to ctx, if any.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func requestIDPtr(ctx context.Context) *string {
	if id := RequestID(ctx); id != "" {
		return &id
	}
	return nil
}

// AuditEvent is one entry as InsertAudit writes it. Details must hold
// identifiers and outcomes only, never memory content or credentials.
type AuditEvent struct {
	OrgID      *uuid.UUID
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	ScopeID    *uuid.UUID
	Outcome    string // success | denied | error
	Details    map[string]any
}

// InsertAudit appends an audit event inside q's transaction.
func InsertAudit(ctx context.Context, q *db.Queries, e AuditEvent) error {
	details := map[string]any{}
	for k, v := range e.Details {
		details[k] = v
	}
	details["attribution"] = AttributionFrom(ctx)
	b, err := jsonBytes(details)
	if err != nil {
		return err
	}
	if e.Outcome == "" {
		e.Outcome = "success"
	}
	if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		OrgID: e.OrgID, ActorID: e.ActorID, Action: e.Action, TargetType: e.TargetType,
		TargetID: e.TargetID, ScopeID: e.ScopeID, Outcome: e.Outcome, Details: b, RequestID: requestIDPtr(ctx),
	}); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

func audit(ctx context.Context, tx postgres.Tx, org uuid.UUID, actor uuid.UUID, action, targetType, targetID string, scope *uuid.UUID, details map[string]any) error {
	var actorPtr *uuid.UUID
	if actor != uuid.Nil {
		actorPtr = &actor
	}
	return InsertAudit(ctx, tx.Q, AuditEvent{OrgID: &org, ActorID: actorPtr, Action: action, TargetType: targetType,
		TargetID: targetID, ScopeID: scope, Details: details})
}

// isDenial reports whether err is an authorization refusal worth auditing.
// Not-found is included: for targets that exist but are hidden it is the
// refusal, and the audit row does not reveal which case applied.
func isDenial(err error) bool {
	return errors.Is(err, ErrForbidden) || errors.Is(err, ErrInherited) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrBrowserReviewRequired)
}

// auditDenial records a refused action in its own transaction, because the
// action's transaction rolls back. It never fails the request: an audit
// write failure is logged. The row carries the target as the caller named
// it and no scope, so maintainers' audit reads do not reveal probes of
// hidden targets; operators see every row.
func (s *Service) auditDenial(ctx context.Context, p Principal, action, targetType, targetID string, cause error) {
	if !isDenial(cause) {
		return
	}
	reason := "not_found"
	switch {
	case errors.Is(cause, ErrForbidden):
		reason = "forbidden"
	case errors.Is(cause, ErrInherited):
		reason = "inherited_read_only"
	case errors.Is(cause, ErrBrowserReviewRequired):
		reason = "browser_review_required"
	}
	err := postgres.WithActorTx(context.WithoutCancel(ctx), s.pool, p.actor(), func(tx postgres.Tx) error {
		actor := p.ID
		return InsertAudit(ctx, tx.Q, AuditEvent{ActorID: &actor, Action: action, TargetType: targetType, TargetID: targetID,
			Outcome: "denied", Details: map[string]any{"reason": reason, "auth_method": p.AuthMethod}})
	})
	if err != nil {
		s.logger().Error("audit denial", "action", action, "err", err)
	}
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}
