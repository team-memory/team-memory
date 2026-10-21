package memory

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
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
)

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
