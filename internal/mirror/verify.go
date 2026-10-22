package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/postgres/db"
)

// Drift is one difference between the database and a mirror repository.
type Drift struct {
	Repository string `json:"repository"`
	ScopePath  string `json:"scope_path"`
	Kind       string `json:"kind"` // missing_file, content_differs, unexpected_file, undelivered, foreign_commit
	Path       string `json:"path,omitempty"`
	MemoryID   string `json:"memory_id,omitempty"`
	Detail     string `json:"detail"`
}

// VerifyReport is the result of Verify.
type VerifyReport struct {
	Repositories int      `json:"repositories"`
	Files        int      `json:"files"`
	Drift        []Drift  `json:"drift"`
	Notes        []string `json:"notes,omitempty"`
}

// Verify compares every mirror repository with the database. The expected
// file of each memory is the rendering of its latest pushed event; display
// names are compared separately, because commits are not rewritten when a
// principal is renamed. pool connects as the operator.
func Verify(ctx context.Context, pool *pgxpool.Pool, git Git) (VerifyReport, error) {
	q := db.New(pool)
	repos, err := q.MirrorRepositories(ctx)
	if err != nil {
		return VerifyReport{}, err
	}
	var rep VerifyReport
	for _, sc := range repos {
		if err := verifyRepo(ctx, q, git, sc.ScopeID.String(), sc.Path, &rep); err != nil {
			return rep, fmt.Errorf("verify %s (%s): %w", sc.Path, sc.ScopeID, err)
		}
		rep.Repositories++
	}
	return rep, nil
}

func verifyRepo(ctx context.Context, q *db.Queries, git Git, repo, scopePath string, rep *VerifyReport) error {
	add := func(kind, path, memory, detail string) {
		rep.Drift = append(rep.Drift, Drift{Repository: repo, ScopePath: scopePath, Kind: kind, Path: path, MemoryID: memory, Detail: detail})
	}
	scopeID, err := parseUUID(repo)
	if err != nil {
		return err
	}
	expected, err := q.MirrorExpectedEvents(ctx, db.MirrorExpectedEventsParams{Repository: repo, ScopeID: scopeID})
	if err != nil {
		return err
	}
	for _, e := range expected {
		if e.DeliveryState != "pushed" {
			add("undelivered", "", e.MemoryID.String(), fmt.Sprintf("latest exportable event %s (seq %d) is %s", e.EventID, e.Seq, e.DeliveryState))
		}
	}
	pushed, err := q.MirrorLatestPushed(ctx, repo)
	if err != nil {
		return err
	}
	r, err := git.Open(ctx, repo)
	if err != nil {
		return err
	}
	if err := r.Sync(ctx); err != nil {
		return err
	}
	files, err := r.ListFiles("memories")
	if err != nil {
		return err
	}
	rep.Files += len(files)
	seen := map[string]bool{}
	for _, p := range pushed {
		want, err := LoadDocument(ctx, q, p.EventID)
		if err != nil {
			return err
		}
		seen[want.Path()] = true
		got, err := r.ReadFile(want.Path())
		if errors.Is(err, fs.ErrNotExist) {
			add("missing_file", want.Path(), want.ID, fmt.Sprintf("event %s was acknowledged as pushed but the file is absent", want.EventID))
			continue
		}
		if err != nil {
			return err
		}
		wantBytes := Render(want)
		if bytes.Equal(got, wantBytes) {
			continue
		}
		gotDoc, perr := Parse(got)
		if perr == nil && sameIgnoringNames(gotDoc, want) {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: display names changed since export (not rewritten by design)", want.Path()))
			continue
		}
		detail := "file differs from the database rendering of event " + want.EventID
		if perr != nil {
			detail += " and is not a valid mirror document (edited in git?)"
		} else if gotDoc.EventID != want.EventID {
			detail += fmt.Sprintf(" (file is at event %s)", gotDoc.EventID)
		}
		add("content_differs", want.Path(), want.ID, detail)
	}
	sort.Strings(files)
	for _, f := range files {
		if !seen[f] {
			add("unexpected_file", f, "", "no pushed event in the database produces this file (added in git, or purged)")
		}
	}
	n := 0
	return r.Log(func(c Commit) bool {
		n++
		if _, _, ok := Trailers(c.Message); !ok {
			add("foreign_commit", "", "", fmt.Sprintf("commit %s by %q was not written by the mirror; git edits are never imported and are overwritten on the next export", c.SHA[:12], c.Author.Name))
		}
		return n < maxHistoryScan
	})
}

func sameIgnoringNames(a, b Document) bool {
	for _, d := range []*Document{&a, &b} {
		d.ActorName, d.CreatedByName, d.RevisedByName, d.ApproverName = "", "", "", ""
	}
	return bytes.Equal(Render(a), Render(b))
}
