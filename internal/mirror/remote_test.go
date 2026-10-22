package mirror

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// remoteEnv loads deploy/dev-git/.local/env (written by setup.sh), or
// skips. These tests prove the two authenticated transports against a real
// git server; a file:// remote cannot.
func remoteEnv(t *testing.T) map[string]string {
	t.Helper()
	path := os.Getenv("TEAM_MEMORY_MIRROR_TEST_ENV")
	if path == "" {
		t.Skip("TEAM_MEMORY_MIRROR_TEST_ENV not set; run deploy/dev-git/setup.sh and point it at deploy/dev-git/.local/env")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			env[strings.TrimPrefix(k, "TEAM_MEMORY_MIRROR_TEST_")] = v
		}
	}
	return env
}

// testBranch makes each run independent of earlier runs against the same
// long-lived dev server.
func testBranch(t *testing.T) string {
	return fmt.Sprintf("test-%s-%d", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), time.Now().UnixNano())
}

func pushOne(ctx context.Context, g *GoGit, repo, file string) (string, error) {
	r, err := g.Open(ctx, repo)
	if err != nil {
		return "", err
	}
	if err := r.Sync(ctx); err != nil {
		return "", err
	}
	sha, err := r.Commit(map[string][]byte{file: []byte(file + "\n")}, "test commit\n", sig("mirror"))
	if err != nil {
		return "", err
	}
	return sha, r.Push(ctx)
}

func TestMirrorHTTPSTokenRemote(t *testing.T) {
	env := remoteEnv(t)
	ctx := context.Background()
	branch := testBranch(t)
	cfg := GoGitConfig{RemoteTemplate: env["HTTPS"], Branch: branch, CacheDir: t.TempDir(),
		TokenFile: env["TOKEN_FILE"], TokenUser: env["TOKEN_USER"], CAFile: env["CA_FILE"]}
	g, err := NewGoGit(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := pushOne(ctx, g, "https-repo", "memories/fact/a.md")
	if err != nil {
		t.Fatalf("authenticated https push: %v", err)
	}

	// A second working copy (another worker) reads it back.
	cfg2 := cfg
	cfg2.CacheDir = t.TempDir()
	g2, _ := NewGoGit(cfg2)
	b, _ := g2.Open(ctx, "https-repo")
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if h, _ := b.Head(); h != sha {
		t.Fatalf("head %s, want %s", h, sha)
	}

	// Server-backed non-fast-forward: b pushes, a (stale) is rejected, and
	// a sync then push succeeds.
	a, _ := g.Open(ctx, "https-repo")
	if _, err := b.Commit(map[string][]byte{"memories/fact/b.md": []byte("b\n")}, "b", sig("b")); err != nil {
		t.Fatal(err)
	}
	if err := b.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(map[string][]byte{"memories/fact/c.md": []byte("c\n")}, "c", sig("a")); err != nil {
		t.Fatal(err)
	}
	if err := a.Push(ctx); !errors.Is(err, ErrNonFastForward) {
		t.Fatalf("stale push to the server: %v, want ErrNonFastForward", err)
	}
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(map[string][]byte{"memories/fact/c.md": []byte("c\n")}, "c", sig("a")); err != nil {
		t.Fatal(err)
	}
	if err := a.Push(ctx); err != nil {
		t.Fatalf("push after sync: %v", err)
	}

	token, _ := os.ReadFile(env["TOKEN_FILE"])
	t.Run("wrong token", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "token")
		_ = os.WriteFile(bad, []byte("0000000000000000000000000000000000000000"), 0o600)
		c := cfg
		c.TokenFile, c.CacheDir = bad, t.TempDir()
		g, _ := NewGoGit(c)
		_, err := pushOne(ctx, g, "https-repo", "memories/fact/x.md")
		if !errors.Is(err, ErrRemote) {
			t.Fatalf("err = %v, want ErrRemote", err)
		}
		if strings.Contains(err.Error(), strings.TrimSpace(string(token))) {
			t.Fatal("error message contains the token")
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		c := cfg
		c.CAFile, c.CacheDir = "", t.TempDir()
		g, _ := NewGoGit(c)
		if _, err := pushOne(ctx, g, "https-repo", "memories/fact/x.md"); !errors.Is(err, ErrRemote) {
			t.Fatalf("err = %v, want ErrRemote (TLS verification)", err)
		}
	})
	t.Run("rotated token file takes effect without restart", func(t *testing.T) {
		rot := filepath.Join(t.TempDir(), "token")
		_ = os.WriteFile(rot, token, 0o600)
		c := cfg
		c.TokenFile, c.CacheDir = rot, t.TempDir()
		g, _ := NewGoGit(c)
		r, _ := g.Open(ctx, "https-repo")
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(rot, []byte("revoked"), 0o600)
		r, _ = g.Open(ctx, "https-repo")
		if err := r.Sync(ctx); !errors.Is(err, ErrRemote) {
			t.Fatalf("after rotation to a bad token: %v", err)
		}
	})
}

func TestMirrorSSHDeployKeyRemote(t *testing.T) {
	env := remoteEnv(t)
	ctx := context.Background()
	cfg := GoGitConfig{RemoteTemplate: env["SSH"], Branch: testBranch(t), CacheDir: t.TempDir(),
		SSHKeyFile: env["SSH_KEY_FILE"], SSHKnownHostsFile: env["KNOWN_HOSTS"]}
	g, err := NewGoGit(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := pushOne(ctx, g, "ssh-repo", "memories/fact/a.md")
	if err != nil {
		t.Fatalf("deploy key push: %v", err)
	}
	cfg2 := cfg
	cfg2.CacheDir = t.TempDir()
	g2, _ := NewGoGit(cfg2)
	r, _ := g2.Open(ctx, "ssh-repo")
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.Head(); h != sha {
		t.Fatalf("head %s, want %s", h, sha)
	}

	t.Run("key scoped to one repository", func(t *testing.T) {
		c := cfg
		c.CacheDir = t.TempDir()
		g, _ := NewGoGit(c)
		if _, err := pushOne(ctx, g, "https-repo", "memories/fact/x.md"); !errors.Is(err, ErrRemote) {
			t.Fatalf("deploy key reached another repository: %v", err)
		}
	})
	t.Run("unregistered key", func(t *testing.T) {
		c := cfg
		c.SSHKeyFile, c.CacheDir = env["OTHER_KEY_FILE"], t.TempDir()
		g, _ := NewGoGit(c)
		if _, err := pushOne(ctx, g, "ssh-repo", "memories/fact/x.md"); !errors.Is(err, ErrRemote) {
			t.Fatalf("err = %v, want ErrRemote", err)
		}
	})
	t.Run("unknown host key", func(t *testing.T) {
		kh := filepath.Join(t.TempDir(), "known_hosts")
		pub, _ := os.ReadFile(env["OTHER_KEY_FILE"] + ".pub") // any key that is not the server's
		_ = os.WriteFile(kh, []byte("[localhost]:2222 "+strings.Join(strings.Fields(string(pub))[:2], " ")+"\n"), 0o600)
		c := cfg
		c.SSHKnownHostsFile, c.CacheDir = kh, t.TempDir()
		g, _ := NewGoGit(c)
		if _, err := pushOne(ctx, g, "ssh-repo", "memories/fact/x.md"); !errors.Is(err, ErrRemote) {
			t.Fatalf("err = %v, want ErrRemote (host key mismatch)", err)
		}
	})
}
