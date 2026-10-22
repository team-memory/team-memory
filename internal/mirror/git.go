package mirror

import (
	"context"
	"errors"
	"time"
)

// Git opens mirror repositories by key (the owning scope's id). The only
// implementation is GoGit (go-git); nothing in the service runs a git
// binary.
type Git interface {
	Open(ctx context.Context, repo string) (Repo, error)
}

// Repo is a local clone of one remote mirror repository on one branch
// (GoGit keeps a bare clone and writes objects directly). It is not safe
// for concurrent use; the exporter serializes all work on a repository with
// a PostgreSQL advisory lock.
type Repo interface {
	// Sync makes the local branch equal to the remote branch, discarding
	// local commits that were never pushed. An empty remote yields an empty
	// branch.
	Sync(ctx context.Context) error
	// Head returns the current commit, or "" for an empty branch.
	Head() (string, error)
	// Log calls fn for each commit from Head backwards until fn returns
	// false or the history ends.
	Log(fn func(Commit) bool) error
	// ReadFile returns a file at Head (fs.ErrNotExist when absent).
	ReadFile(path string) ([]byte, error)
	// ListFiles returns the paths of all files at Head under dir.
	ListFiles(dir string) ([]string, error)
	// Commit writes changes (nil content removes the path) and commits
	// them. It returns the new commit's hash.
	Commit(changes map[string][]byte, msg string, author Signature) (string, error)
	// Push sends the branch to the remote. It returns ErrNonFastForward when
	// the remote moved, and ErrRemote wrapping the cause for other failures.
	Push(ctx context.Context) error
}

// Commit is one commit in a mirror repository.
type Commit struct {
	SHA     string
	Message string
	Author  Signature
}

// Signature names a commit author.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

var (
	// ErrNonFastForward means the remote branch moved since the last sync:
	// someone else pushed. The exporter syncs and retries.
	ErrNonFastForward = errors.New("mirror push rejected: remote branch moved (non-fast-forward)")
	// ErrRemote wraps transport failures (remote down, authentication).
	ErrRemote = errors.New("mirror remote unavailable")
)
