package mirror

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
)

// newBareRemote creates an empty bare repository and returns a GoGit whose
// template points at a directory of such repositories.
func newBareRemote(t *testing.T, repos ...string) (*GoGit, string) {
	t.Helper()
	dir := t.TempDir()
	for _, r := range repos {
		if _, err := git.PlainInit(filepath.Join(dir, r+".git"), true); err != nil {
			t.Fatal(err)
		}
	}
	g, err := NewGoGit(GoGitConfig{RemoteTemplate: "file://" + dir + "/{repo}.git", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return g, dir
}

func sig(name string) Signature {
	return Signature{Name: name, Email: name + "@users.noreply.example.com", When: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
}

func TestGoGitLocalBareRepository(t *testing.T) {
	ctx := context.Background()
	g, dir := newBareRemote(t, "r1")
	a, err := g.Open(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(ctx); err != nil {
		t.Fatalf("sync empty remote: %v", err)
	}
	if h, _ := a.Head(); h != "" {
		t.Fatalf("empty remote has head %q", h)
	}
	sha1, err := a.Commit(map[string][]byte{"memories/fact/m1.md": []byte("one\n")}, "first\n\nTeam-Memory-Event: e1\nTeam-Memory-Event-Seq: 1\n", sig("ada"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Push(ctx); err != nil {
		t.Fatalf("first push creates the branch: %v", err)
	}

	// A second working copy (another worker) sees the commit.
	g2, err := NewGoGit(GoGitConfig{RemoteTemplate: "file://" + dir + "/{repo}.git", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := g2.Open(ctx, "r1")
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if h, _ := b.Head(); h != sha1 {
		t.Fatalf("second copy head %q, want %q", h, sha1)
	}
	got, err := b.ReadFile("memories/fact/m1.md")
	if err != nil || string(got) != "one\n" {
		t.Fatalf("read = %q, %v", got, err)
	}
	var authors []string
	_ = b.Log(func(c Commit) bool { authors = append(authors, c.Author.Name+" <"+c.Author.Email+">"); return true })
	if len(authors) != 1 || authors[0] != "ada <ada@users.noreply.example.com>" {
		t.Fatalf("authors = %v", authors)
	}

	// b pushes; a is now behind: its push is rejected as non-fast-forward.
	if _, err := b.Commit(map[string][]byte{"memories/fact/m2.md": []byte("two\n")}, "second", sig("bob")); err != nil {
		t.Fatal(err)
	}
	if err := b.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(map[string][]byte{"memories/fact/m3.md": []byte("three\n")}, "third", sig("ada")); err != nil {
		t.Fatal(err)
	}
	if err := a.Push(ctx); !errors.Is(err, ErrNonFastForward) {
		t.Fatalf("stale push: err = %v, want ErrNonFastForward", err)
	}
	// Sync drops a's unpushed commit and its file.
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReadFile("memories/fact/m3.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unpushed file survived sync: %v", err)
	}
	files, _ := a.ListFiles("memories")
	if len(files) != 2 {
		t.Fatalf("files after sync = %v", files)
	}
	// Removal and a kind change move the file.
	if _, err := a.Commit(map[string][]byte{"memories/fact/m2.md": nil, "memories/lesson/m2.md": []byte("two\n")}, "move", sig("ada")); err != nil {
		t.Fatal(err)
	}
	files, _ = a.ListFiles("memories")
	if len(files) != 2 || files[0] != "memories/fact/m1.md" || files[1] != "memories/lesson/m2.md" {
		t.Fatalf("files after move = %v", files)
	}
}

func TestGoGitRemoteDown(t *testing.T) {
	ctx := context.Background()
	g, _ := newBareRemote(t) // no repositories exist
	r, err := g.Open(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(ctx); !errors.Is(err, ErrRemote) {
		t.Fatalf("sync of a missing remote: err = %v, want ErrRemote", err)
	}
}

func TestGoGitConfigValidation(t *testing.T) {
	for name, cfg := range map[string]GoGitConfig{
		"no placeholder":  {RemoteTemplate: "file:///x.git", CacheDir: "/tmp"},
		"no cache":        {RemoteTemplate: "file:///{repo}.git"},
		"ssh without key": {RemoteTemplate: "ssh://git@git.example.com/m/{repo}.git", CacheDir: "/tmp"},
		"https no token":  {RemoteTemplate: "https://git.example.com/m/{repo}.git", CacheDir: "/tmp"},
		"plain http":      {RemoteTemplate: "http://git.example.com/m/{repo}.git", CacheDir: "/tmp", TokenFile: "/t"},
		"unknown scheme":  {RemoteTemplate: "ftp://git.example.com/m/{repo}.git", CacheDir: "/tmp"},
	} {
		if _, err := NewGoGit(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	g, _ := newBareRemote(t)
	for _, bad := range []string{"../etc", "A", "", "x/y"} {
		if _, err := g.Open(context.Background(), bad); err == nil {
			t.Errorf("repository key %q accepted", bad)
		}
	}
}
