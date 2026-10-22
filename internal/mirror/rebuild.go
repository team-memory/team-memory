package mirror

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
)

// RebuildOptions configures a disaster-recovery rebuild.
type RebuildOptions struct {
	// Scope is the scope (path or id) that receives the rebuilt memories,
	// normally a freshly created replacement of the lost scope.
	Scope string
	// As is the principal the new memories are attributed to: a writer in
	// Scope (and a maintainer when IncludeArchived archives memories).
	As memory.Principal
	// IncludeArchived also rebuilds archived memories, then archives them.
	IncludeArchived bool
	// DryRun parses and reports without writing.
	DryRun bool
}

// RebuildItem reports one file.
type RebuildItem struct {
	Path       string `json:"path"`
	OriginalID string `json:"original_id"`
	NewID      string `json:"new_id,omitempty"`
	Action     string `json:"action"` // created, archived, skipped
	Reason     string `json:"reason,omitempty"`
}

// Rebuild is the explicit disaster-recovery exception to the no-import
// rule: after total database loss it recreates a scope's memories from its
// mirror repository at Head. It restores content only. Every memory comes
// back as a new memory with a new id and a single revision, written by
// opts.As with trust derived from its citation; approvals, revision
// history, override links, embeddings, audit events and memberships are
// not restored, and the original approval metadata is quoted in the change
// note as history, never as an approval. Superseded memories are skipped
// (their replacements are rebuilt).
func Rebuild(ctx context.Context, svc *memory.Service, r Repo, opts RebuildOptions) ([]RebuildItem, error) {
	files, err := r.ListFiles("memories")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []RebuildItem
	for _, f := range files {
		b, err := r.ReadFile(f)
		if err != nil {
			return out, err
		}
		d, err := Parse(b)
		if err != nil {
			out = append(out, RebuildItem{Path: f, Action: "skipped", Reason: err.Error()})
			continue
		}
		item := RebuildItem{Path: f, OriginalID: d.ID}
		switch {
		case d.Status == "superseded":
			item.Action, item.Reason = "skipped", "superseded by "+d.SupersededBy
		case d.Status == "archived" && !opts.IncludeArchived:
			item.Action, item.Reason = "skipped", "archived"
		case opts.DryRun:
			item.Action, item.Reason = "skipped", "dry run"
		default:
			if err := rebuildOne(ctx, svc, d, opts, &item); err != nil {
				return out, fmt.Errorf("%s: %w", f, err)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func rebuildOne(ctx context.Context, svc *memory.Service, d Document, opts RebuildOptions, item *RebuildItem) error {
	note := fmt.Sprintf("restored from the git mirror after data loss: original memory %s revision %d, trust %q at event %s",
		d.ID, d.Revision, d.Trust, d.EventID)
	if d.ApproverID != "" {
		note += fmt.Sprintf("; was approved by %s at revision %d (approval not restored; review again)", d.ApproverName, d.ApprovedRev)
	}
	if len(note) > 1000 {
		note = note[:1000]
	}
	m, err := svc.Create(ctx, opts.As, memory.CreateInput{
		Scope: opts.Scope, Kind: d.Kind, Title: d.Title, Content: d.Content, Rationale: d.Rationale,
		SourceURI: d.SourceURI, ChangeNote: note,
	})
	if err != nil {
		return err
	}
	item.NewID, item.Action = m.ID.String(), "created"
	if d.Status == "archived" {
		if _, err := svc.Archive(ctx, opts.As, memory.ArchiveInput{ID: m.ID, ExpectedStateVersion: m.StateVersion,
			Reason: "archived in the mirror before data loss"}); err != nil {
			return err
		}
		item.Action = "archived"
	}
	return nil
}

func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("repository key %q is not a scope id", s)
	}
	return id, nil
}
