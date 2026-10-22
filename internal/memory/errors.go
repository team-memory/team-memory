package memory

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/secrets"
)

var (
	// ErrNotFound is also returned when the principal may not read the target,
	// so callers cannot probe for the existence of hidden scopes or memories.
	ErrNotFound = errors.New("not found")
	// ErrForbidden means the principal can see the target but lacks the role
	// needed for the action.
	ErrForbidden = errors.New("forbidden")
	// ErrInherited means a change was attempted from a descendant scope on a
	// memory owned by an ancestor. Inherited memories are read-only.
	ErrInherited = errors.New("inherited memories can only be changed from their owning scope")
	// ErrNotActive means the memory is archived or superseded.
	ErrNotActive = errors.New("memory is not active")
	// ErrNotImplemented marks contract methods whose behaviour lands later.
	ErrNotImplemented = errors.New("not implemented")
	// ErrBrowserReviewRequired means the action completes a review or a
	// promotion and the caller is not a human in a portal session. MCP and
	// bearer-token API calls always get this, whatever the principal's role.
	ErrBrowserReviewRequired = errors.New("this action requires explicit review in the Team Memory portal")
	// ErrPromotionStale means the source changed (new revision, archived,
	// approval withdrawn, submitter lost authority) since the request was
	// submitted; it has been marked stale and must be resubmitted.
	ErrPromotionStale = errors.New("promotion request is stale; resubmit it from the source scope")
	// ErrPromotionClosed means the request was already rejected, cancelled
	// or marked stale.
	ErrPromotionClosed = errors.New("promotion request is closed")
	// ErrRateLimited means the principal exceeded its request budget.
	ErrRateLimited = errors.New("rate limit exceeded")
)

// SecretError means input looked like it contained a credential. Findings
// name fields and rule types only, never the matched text.
type SecretError struct {
	Findings []secrets.Finding
}

func (e *SecretError) Error() string {
	parts := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		parts = append(parts, f.Field+": "+f.Rule)
	}
	return "possible secret detected (" + strings.Join(parts, ", ") + "); remove it, rotate the credential if it was real, and retry"
}

// ValidationError describes invalid input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return "invalid input: " + e.Msg }

func invalidf(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// ConflictError is returned when an expected revision or state version does
// not match the current one (optimistic concurrency).
type ConflictError struct {
	MemoryID             uuid.UUID
	ExpectedRevision     int
	CurrentRevision      int
	ExpectedStateVersion int64
	CurrentStateVersion  int64
}

func (e *ConflictError) Error() string {
	if e.ExpectedStateVersion != 0 {
		return fmt.Sprintf("conflict: memory %s is at state version %d, expected %d",
			e.MemoryID, e.CurrentStateVersion, e.ExpectedStateVersion)
	}
	return fmt.Sprintf("conflict: memory %s is at revision %d, expected %d",
		e.MemoryID, e.CurrentRevision, e.ExpectedRevision)
}
