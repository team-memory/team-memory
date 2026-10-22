package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// go-git v5's file:// transport runs the git-upload-pack and
// git-receive-pack binaries. Serve file:// in process instead, so the
// service never needs or runs a git binary. The in-process server does not
// compare-and-swap reference updates; that is acceptable only because the
// exporter serializes pushes per repository (docs/decisions/git-library.md).
func init() {
	client.InstallProtocol("file", server.NewClient(server.NewFilesystemLoader(osfs.New(""))))
}

// GoGitConfig configures GoGit. Credentials are read from files (mounted
// Kubernetes Secrets) on every Open, so a rotated Secret takes effect
// without a restart. They are never logged.
type GoGitConfig struct {
	// RemoteTemplate is the remote URL with {repo} standing for the
	// repository key (the owning scope's id), e.g.
	// ssh://git@git.example.com/team-memory-mirror/{repo}.git
	RemoteTemplate string
	// Branch defaults to main.
	Branch string
	// CacheDir holds bare clones, one directory per repository. It may be
	// emptied at any time; clones are recreated.
	CacheDir string

	// SSH: a private key (deploy key) and a known_hosts file. Host keys are
	// always verified.
	SSHKeyFile        string
	SSHKnownHostsFile string
	// HTTPS: a token used as the basic-auth password, with TokenUser as the
	// user name (default x-access-token), and an optional CA bundle.
	TokenFile string
	TokenUser string
	CAFile    string
	// AllowHTTP permits plain http:// remotes (tests only).
	AllowHTTP bool
}

// GoGit implements Git with go-git.
type GoGit struct {
	cfg    GoGitConfig
	scheme string
}

var repoKey = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// NewGoGit validates cfg.
func NewGoGit(cfg GoGitConfig) (*GoGit, error) {
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.TokenUser == "" {
		cfg.TokenUser = "x-access-token"
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("mirror: a cache directory for working copies is required")
	}
	if !strings.Contains(cfg.RemoteTemplate, "{repo}") {
		return nil, errors.New("mirror: the remote template must contain {repo}")
	}
	ep, err := transport.NewEndpoint(strings.ReplaceAll(cfg.RemoteTemplate, "{repo}", "x"))
	if err != nil {
		return nil, fmt.Errorf("mirror: remote template: %w", err)
	}
	g := &GoGit{cfg: cfg, scheme: ep.Protocol}
	switch ep.Protocol {
	case "ssh":
		if cfg.SSHKeyFile == "" || cfg.SSHKnownHostsFile == "" {
			return nil, errors.New("mirror: ssh remotes need a deploy key file and a known_hosts file")
		}
	case "https":
		if cfg.TokenFile == "" {
			return nil, errors.New("mirror: https remotes need a token file")
		}
	case "http":
		if !cfg.AllowHTTP {
			return nil, errors.New("mirror: plain http remotes are refused; use https or ssh")
		}
	case "file":
	default:
		return nil, fmt.Errorf("mirror: unsupported remote scheme %q", ep.Protocol)
	}
	return g, nil
}

// RemoteURL returns the remote for a repository key.
func (g *GoGit) RemoteURL(repo string) string {
	return strings.ReplaceAll(g.cfg.RemoteTemplate, "{repo}", repo)
}

func (g *GoGit) auth() (transport.AuthMethod, []byte, error) {
	switch g.scheme {
	case "ssh":
		ep, _ := transport.NewEndpoint(g.RemoteURL("x"))
		user := ep.User
		if user == "" {
			user = "git"
		}
		keys, err := gitssh.NewPublicKeysFromFile(user, g.cfg.SSHKeyFile, "")
		if err != nil {
			return nil, nil, fmt.Errorf("mirror: read deploy key: %w", errors.Unwrap(err))
		}
		cb, err := gitssh.NewKnownHostsCallback(g.cfg.SSHKnownHostsFile)
		if err != nil {
			return nil, nil, fmt.Errorf("mirror: read known_hosts: %w", err)
		}
		keys.HostKeyCallback = cb
		return keys, nil, nil
	case "https", "http":
		var a transport.AuthMethod
		if g.cfg.TokenFile != "" {
			tok, err := os.ReadFile(g.cfg.TokenFile)
			if err != nil {
				return nil, nil, fmt.Errorf("mirror: read token file: %w", err)
			}
			a = &githttp.BasicAuth{Username: g.cfg.TokenUser, Password: strings.TrimSpace(string(tok))}
		}
		var ca []byte
		if g.cfg.CAFile != "" {
			b, err := os.ReadFile(g.cfg.CAFile)
			if err != nil {
				return nil, nil, fmt.Errorf("mirror: read CA bundle: %w", err)
			}
			ca = b
		}
		return a, ca, nil
	}
	return nil, nil, nil
}

// Open returns the local clone of repo, creating it on first use.
func (g *GoGit) Open(ctx context.Context, repo string) (Repo, error) {
	if !repoKey.MatchString(repo) {
		return nil, fmt.Errorf("mirror: invalid repository key %q", repo)
	}
	auth, ca, err := g.auth()
	if err != nil {
		return nil, err
	}
	r := &goRepo{dir: filepath.Join(g.cfg.CacheDir, repo), url: g.RemoteURL(repo), branch: g.cfg.Branch, auth: auth, ca: ca}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// goRepo is a bare clone used without a worktree: commits are built
// directly from blob and tree objects. go-git's worktree operations (Add,
// status) scan every file and made exports quadratic in repository size
// (docs/decisions/git-library.md).
type goRepo struct {
	dir, url, branch string
	auth             transport.AuthMethod
	ca               []byte
	r                *git.Repository
	// files maps every path at head to its blob, loaded once per head.
	files     map[string]plumbing.Hash
	filesHead plumbing.Hash
	// base is the remote head at the last Sync or Push; pending holds the
	// objects written since, which is exactly what a push must send.
	base    plumbing.Hash
	pending map[plumbing.Hash]struct{}
}

func (r *goRepo) branchRef() plumbing.ReferenceName { return plumbing.NewBranchReferenceName(r.branch) }
func (r *goRepo) remoteRef() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName("origin", r.branch)
}

// open opens the cached clone or initializes an empty one with the remote
// configured. A clone of another remote is discarded.
func (r *goRepo) open() error {
	repo, err := git.PlainOpen(r.dir)
	if err == nil {
		if rem, rerr := repo.Remote("origin"); rerr == nil && len(rem.Config().URLs) == 1 && rem.Config().URLs[0] == r.url {
			r.r = repo
			return nil
		}
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		// A corrupt cache or one for another remote is disposable.
		if rerr := os.RemoveAll(r.dir); rerr != nil {
			return fmt.Errorf("mirror: reset cache: %w", rerr)
		}
	}
	return r.init()
}

func (r *goRepo) init() error {
	if err := os.MkdirAll(filepath.Dir(r.dir), 0o700); err != nil {
		return fmt.Errorf("mirror: cache directory: %w", err)
	}
	repo, err := git.PlainInitWithOptions(r.dir, &git.PlainInitOptions{
		Bare: true, InitOptions: git.InitOptions{DefaultBranch: r.branchRef()},
	})
	if err != nil {
		return fmt.Errorf("mirror: init cache: %w", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{r.url}}); err != nil {
		return fmt.Errorf("mirror: configure remote: %w", err)
	}
	r.r, r.files, r.filesHead, r.base, r.pending = repo, nil, plumbing.ZeroHash, plumbing.ZeroHash, nil
	return nil
}

func (r *goRepo) fetch(ctx context.Context) error {
	return r.r.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + r.branchRef().String() + ":" + r.remoteRef().String())},
		Auth:       r.auth, CABundle: r.ca, Tags: git.NoTags, Force: true,
	})
}

func (r *goRepo) Sync(ctx context.Context) error {
	err := r.fetch(ctx)
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) && !errors.Is(err, transport.ErrEmptyRemoteRepository) &&
		!isMissingRef(err) && ctx.Err() == nil {
		// The clone is a disposable cache. Some servers (go-git's own
		// in-process file:// server among them) fail a fetch whose
		// negotiation names an object they lack; a fresh clone has none, so
		// retry once from scratch before reporting the remote.
		if rerr := os.RemoveAll(r.dir); rerr != nil {
			return fmt.Errorf("mirror: reset cache: %w", rerr)
		}
		if ierr := r.init(); ierr != nil {
			return ierr
		}
		err = r.fetch(ctx)
	}
	empty := false
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
	case errors.Is(err, transport.ErrEmptyRemoteRepository), isMissingRef(err):
		empty = true
	default:
		return fmt.Errorf("%w: fetch: %w", ErrRemote, err)
	}
	if !empty {
		ref, err := r.r.Reference(r.remoteRef(), true)
		switch {
		case errors.Is(err, plumbing.ErrReferenceNotFound):
			// Fetched nothing for the branch: treat as empty below.
		case err != nil:
			return fmt.Errorf("mirror: read remote branch: %w", err)
		default:
			// Point the branch at the remote head; unpushed local commits
			// become unreachable.
			r.base, r.pending = ref.Hash(), nil
			return r.r.Storer.SetReference(plumbing.NewHashReference(r.branchRef(), ref.Hash()))
		}
	}
	// Empty remote: start over from nothing.
	if err := os.RemoveAll(r.dir); err != nil {
		return fmt.Errorf("mirror: reset cache: %w", err)
	}
	return r.init()
}

func isMissingRef(err error) bool {
	var nm git.NoMatchingRefSpecError
	return errors.As(err, &nm)
}

func (r *goRepo) head() (*object.Commit, error) {
	ref, err := r.r.Reference(r.branchRef(), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.r.CommitObject(ref.Hash())
}

func (r *goRepo) Head() (string, error) {
	c, err := r.head()
	if err != nil || c == nil {
		return "", err
	}
	return c.Hash.String(), nil
}

// tree returns the flat path -> blob map at head, without reading blobs.
func (r *goRepo) tree() (map[string]plumbing.Hash, *object.Commit, error) {
	c, err := r.head()
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return map[string]plumbing.Hash{}, nil, nil
	}
	if r.files != nil && r.filesHead == c.Hash {
		return r.files, c, nil
	}
	t, err := c.Tree()
	if err != nil {
		return nil, nil, err
	}
	files := map[string]plumbing.Hash{}
	w := object.NewTreeWalker(t, true, nil)
	defer w.Close()
	for {
		name, e, err := w.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if e.Mode.IsFile() {
			files[name] = e.Hash
		}
	}
	r.files, r.filesHead = files, c.Hash
	return files, c, nil
}

func (r *goRepo) Log(fn func(Commit) bool) error {
	c, err := r.head()
	if err != nil || c == nil {
		return err
	}
	iter, err := r.r.Log(&git.LogOptions{From: c.Hash})
	if err != nil {
		return err
	}
	defer iter.Close()
	err = iter.ForEach(func(c *object.Commit) error {
		if !fn(Commit{SHA: c.Hash.String(), Message: c.Message,
			Author: Signature{Name: c.Author.Name, Email: c.Author.Email, When: c.Author.When}}) {
			return storer.ErrStop
		}
		return nil
	})
	if errors.Is(err, storer.ErrStop) {
		return nil
	}
	return err
}

func (r *goRepo) ReadFile(p string) ([]byte, error) {
	files, _, err := r.tree()
	if err != nil {
		return nil, err
	}
	h, ok := files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	b, err := r.r.BlobObject(h)
	if err != nil {
		return nil, err
	}
	rd, err := b.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rd.Close() }()
	return io.ReadAll(rd)
}

func (r *goRepo) ListFiles(dir string) ([]string, error) {
	files, _, err := r.tree()
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	for p := range files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *goRepo) Commit(changes map[string][]byte, msg string, author Signature) (string, error) {
	cur, parent, err := r.tree()
	if err != nil {
		return "", err
	}
	files := make(map[string]plumbing.Hash, len(cur)+len(changes))
	for p, h := range cur {
		files[p] = h
	}
	for p, content := range changes {
		if p != path.Clean(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "..") || strings.HasPrefix(p, ".git") || p == "." {
			return "", fmt.Errorf("mirror: refusing to write path %q", p)
		}
		if content == nil {
			delete(files, p)
			continue
		}
		h, err := r.writeBlob(content)
		if err != nil {
			return "", err
		}
		files[p] = h
	}
	root, err := r.writeTree(files, "")
	if err != nil {
		return "", err
	}
	sig := object.Signature{Name: author.Name, Email: author.Email, When: author.When}
	c := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: root}
	if parent != nil {
		c.ParentHashes = []plumbing.Hash{parent.Hash}
	}
	obj := r.r.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return "", err
	}
	h, err := r.store(obj)
	if err != nil {
		return "", fmt.Errorf("mirror: commit: %w", err)
	}
	if err := r.r.Storer.SetReference(plumbing.NewHashReference(r.branchRef(), h)); err != nil {
		return "", err
	}
	r.files, r.filesHead = files, h
	return h.String(), nil
}

func (r *goRepo) writeBlob(b []byte) (plumbing.Hash, error) {
	obj := r.r.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(b); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.store(obj)
}

// store writes an object and remembers it for the next push.
func (r *goRepo) store(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	h, err := r.r.Storer.SetEncodedObject(obj)
	if err != nil {
		return h, err
	}
	if r.pending == nil {
		r.pending = map[plumbing.Hash]struct{}{}
	}
	r.pending[h] = struct{}{}
	return h, nil
}

// writeTree writes the tree for the paths under prefix (recursively) and
// returns its hash. Entries are sorted as git requires: a directory sorts
// as if its name ended in "/".
func (r *goRepo) writeTree(files map[string]plumbing.Hash, prefix string) (plumbing.Hash, error) {
	blobs := map[string]plumbing.Hash{}
	dirs := map[string]bool{}
	for p, h := range files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if name, _, nested := strings.Cut(rest, "/"); nested {
			dirs[name] = true
		} else {
			blobs[rest] = h
		}
	}
	var entries []object.TreeEntry
	for name, h := range blobs {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
	}
	for name := range dirs {
		h, err := r.writeTree(files, prefix+name+"/")
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
	}
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return key(entries[i]) < key(entries[j]) })
	obj := r.r.Storer.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.store(obj)
}

// Push sends exactly the objects written since the last sync in one
// packfile and updates the branch with the remote head it was built on as
// the expected old value. go-git's own Push computes the objects to send by
// walking every tree of the remote's history, which made each push slower
// as history grew (docs/decisions/git-library.md); the transport, packfile
// encoder and protocol are still go-git's.
func (r *goRepo) Push(ctx context.Context) error {
	head, err := r.r.Reference(r.branchRef(), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if head.Hash() == r.base {
		return nil
	}
	ep, err := transport.NewEndpoint(r.url)
	if err != nil {
		return err
	}
	ep.CaBundle = r.ca
	cl, err := client.NewClient(ep)
	if err != nil {
		return err
	}
	sess, err := cl.NewReceivePackSession(ep, r.auth)
	if err != nil {
		return fmt.Errorf("%w: push: %w", ErrRemote, err)
	}
	defer func() { _ = sess.Close() }()
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: push: %w", ErrRemote, err)
	}
	refs, err := ar.AllReferences()
	if err != nil {
		return fmt.Errorf("%w: push: %w", ErrRemote, err)
	}
	remote := plumbing.ZeroHash
	if ref, ok := refs[r.branchRef()]; ok {
		remote = ref.Hash()
	}
	if remote != r.base {
		return ErrNonFastForward
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{Name: r.branchRef(), Old: remote, New: head.Hash()}}
	hashes := make([]plumbing.Hash, 0, len(r.pending))
	for h := range r.pending {
		hashes = append(hashes, h)
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, r.r.Storer, false).Encode(hashes, 0); err != nil {
		return fmt.Errorf("mirror: encode packfile: %w", err)
	}
	req.Packfile = io.NopCloser(&pack)
	rs, err := sess.ReceivePack(ctx, req)
	if err == nil && rs != nil {
		err = rs.Error()
	}
	if err != nil {
		if isNonFastForward(err) {
			return ErrNonFastForward
		}
		return fmt.Errorf("%w: push: %w", ErrRemote, err)
	}
	r.base, r.pending = head.Hash(), nil
	return r.r.Storer.SetReference(plumbing.NewHashReference(r.remoteRef(), head.Hash()))
}

// isNonFastForward recognizes a server-side rejection reported in the
// push status (a server that enforces compare-and-swap after go-git's own
// client-side check passed).
func isNonFastForward(err error) bool {
	s := err.Error()
	return strings.Contains(s, "non-fast-forward") || strings.Contains(s, "fetch first")
}
