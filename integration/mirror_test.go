package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/mirror"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// mirrorFixture is a tree with the mirror enabled and one bare repository
// per scope under remoteDir.
type mirrorFixture struct {
	*tree
	remoteDir string
	git       *mirror.GoGit
	exporter  *mirror.Exporter
}

func newMirrorFixture(t *testing.T) *mirrorFixture {
	t.Helper()
	tr := newTree(t)
	dir := t.TempDir()
	for _, sc := range []memory.Scope{tr.org, tr.team, tr.payments, tr.billing} {
		if _, err := git.PlainInit(filepath.Join(dir, sc.ID.String()+".git"), true); err != nil {
			t.Fatal(err)
		}
	}
	g, err := mirror.NewGoGit(mirror.GoGitConfig{RemoteTemplate: "file://" + dir + "/{repo}.git", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The operator enables the mirror; the export runs as the mirror role.
	if err := db.New(tr.db.Operator).MirrorSetEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	ex := mirror.NewExporter(tr.db.Mirror, g)
	ex.EmailDomain = "users.noreply.example.com"
	return &mirrorFixture{tree: tr, remoteDir: dir, git: g, exporter: ex}
}

// drainTimeout bounds drain; the throughput benchmark raises it.
var drainTimeout = 20 * time.Second

// drain runs the export worker until no git_mirror job is pending or
// running, with retries made immediate.
func (f *mirrorFixture) drain(t *testing.T, ex *mirror.Exporter) {
	t.Helper()
	ctx := context.Background()
	w := ex.Worker(jobs.Config{BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond})
	deadline := time.Now().Add(drainTimeout)
	for time.Now().Before(deadline) {
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("run: %v", err)
		}
		var open int
		if err := f.db.Admin.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE kind = 'git_mirror' AND status IN ('pending', 'running')").Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("mirror jobs did not drain")
}

// history returns the commit messages of a scope's repository, oldest first.
func (f *mirrorFixture) history(t *testing.T, sc memory.Scope) []string {
	t.Helper()
	g, err := mirror.NewGoGit(mirror.GoGitConfig{RemoteTemplate: "file://" + f.remoteDir + "/{repo}.git", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.Open(context.Background(), sc.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	_ = r.Log(func(c mirror.Commit) bool { msgs = append([]string{c.Message}, msgs...); return true })
	return msgs
}

func (f *mirrorFixture) file(t *testing.T, sc memory.Scope, path string) mirror.Document {
	t.Helper()
	r, err := f.git.Open(context.Background(), sc.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := r.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	d, err := mirror.Parse(b)
	if err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, b)
	}
	return d
}

func subjects(msgs []string) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, strings.SplitN(m, "\n", 2)[0])
	}
	return out
}

// assertOneCommitPerEvent checks that every exportable event of the scope
// appears exactly once in its repository.
func (f *mirrorFixture) assertOneCommitPerEvent(t *testing.T, sc memory.Scope) {
	t.Helper()
	seen := map[string]int{}
	for _, m := range f.history(t, sc) {
		id, _, ok := mirror.Trailers(m)
		if !ok {
			t.Errorf("commit without event trailer: %q", m)
		}
		seen[id]++
	}
	rows, err := f.db.Admin.Query(context.Background(),
		"SELECT id::text FROM memory_events WHERE scope_id = $1 AND tm_mirror_exportable(event_type)", sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		n++
		if seen[id] != 1 {
			t.Errorf("event %s exported %d times, want 1", id, seen[id])
		}
	}
	if len(seen) != n {
		t.Errorf("%d commits for %d exportable events", len(seen), n)
	}
}

func (f *mirrorFixture) verify(t *testing.T) mirror.VerifyReport {
	t.Helper()
	g, err := mirror.NewGoGit(mirror.GoGitConfig{RemoteTemplate: "file://" + f.remoteDir + "/{repo}.git", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := mirror.Verify(context.Background(), f.db.Operator, g)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return rep
}

func TestMirrorLifecycleCommits(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	writer := f.member(t, "Wanda Writer", "acme/platform/payments", memory.RoleWriter, false)

	m, err := f.svc.Create(ctx, writer, memory.CreateInput{Scope: "acme/platform/payments", Kind: "decision",
		Title: "Use PostgreSQL for payments", Content: "ACID transactions.", Rationale: "We need transactions.",
		SourceURI: "https://issues.example.com/PAY-1"})
	if err != nil {
		t.Fatal(err)
	}
	content := "ACID transactions and row-level security."
	m, err = f.svc.Update(ctx, writer, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: &content, ChangeNote: "mention RLS"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, portal(f.owner), memory.ReviewInput{ID: m.ID, Revision: 2}); err != nil {
		t.Fatal(err)
	}
	old, repl, err := f.svc.Supersede(ctx, f.owner, memory.SupersedeInput{ID: m.ID, ExpectedRevision: 2, Reason: "replaced",
		Replacement: memory.CreateInput{Kind: "decision", Title: "Use PostgreSQL 17", Content: "Version 17 with pgvector."}})
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.Create(ctx, writer, memory.CreateInput{Scope: "acme/platform/payments", Kind: "lesson", Title: "Old lesson", Content: "Obsolete."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Archive(ctx, f.owner, memory.ArchiveInput{ID: n.ID, ExpectedStateVersion: n.StateVersion, Reason: "obsolete"}); err != nil {
		t.Fatal(err)
	}
	// A review request is not exported.
	if _, err := f.svc.RequestReview(ctx, f.owner, repl.ID, 1); err != nil {
		t.Fatal(err)
	}

	f.drain(t, f.exporter)
	got := subjects(f.history(t, f.payments))
	want := []string{
		"create decision Use PostgreSQL for payments (revision 1)",
		"update decision Use PostgreSQL for payments (revision 2)",
		"approve decision Use PostgreSQL for payments (revision 2)",
		"create decision Use PostgreSQL 17 (revision 1)",
		"supersede decision Use PostgreSQL for payments (revision 2)",
		"create lesson Old lesson (revision 1)",
		"archive lesson Old lesson (revision 1)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commits:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	f.assertOneCommitPerEvent(t, f.payments)

	d := f.file(t, f.payments, mirror.FilePath("decision", old.ID.String()))
	if d.Status != "superseded" || d.SupersededBy != repl.ID.String() || d.SupersedeReason != "replaced" || d.Revision != 2 ||
		d.Trust != "approved" || d.ApproverID != f.owner.ID.String() || d.ApprovedRev != 2 || d.CreatedByID != writer.ID.String() ||
		d.Content != content || d.SourceURI != "https://issues.example.com/PAY-1" || d.ActorID != f.owner.ID.String() {
		t.Fatalf("superseded file: %+v", d)
	}
	if a := f.file(t, f.payments, mirror.FilePath("lesson", n.ID.String())); a.Status != "archived" {
		t.Fatalf("archived file status %q", a.Status)
	}
	// Commit authors are the acting principals with no-reply addresses.
	r, _ := f.git.Open(ctx, f.payments.ID.String())
	_ = r.Sync(ctx)
	_ = r.Log(func(c mirror.Commit) bool {
		if !strings.HasSuffix(c.Author.Email, "@users.noreply.example.com") || c.Author.Name == "" {
			t.Errorf("author %q <%s>", c.Author.Name, c.Author.Email)
		}
		return true
	})
	// Every state row is pushed with its commit.
	var notPushed int
	_ = f.db.Admin.QueryRow(ctx, "SELECT count(*) FROM mirror_state WHERE delivery_state <> 'pushed' OR commit_sha IS NULL").Scan(&notPushed)
	if notPushed != 0 {
		t.Fatalf("%d mirror events not pushed", notPushed)
	}
	if rep := f.verify(t); len(rep.Drift) != 0 {
		t.Fatalf("drift after export: %+v", rep.Drift)
	}

	// Historical snapshot: the approve commit shows revision 2 approved even
	// though the memory is superseded now (delayed export used event data).
	msgs := f.history(t, f.payments)
	if !strings.Contains(msgs[2], "Trust: approved") || !strings.Contains(msgs[2], "Status: active") {
		t.Fatalf("approve commit: %q", msgs[2])
	}
}

func TestMirrorPromotionAndOverrides(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	src, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "convention",
		Title: "Retry idempotently", Content: "Every retry carries an idempotency key."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, portal(f.owner), memory.ReviewInput{ID: src.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	dst, err := f.svc.PromoteDirect(ctx, portal(f.owner), memory.PromotionInput{ID: src.ID, Revision: 1, Rationale: "team-wide"})
	if err != nil {
		t.Fatal(err)
	}
	// An approved override in billing of the promoted team memory.
	ov, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/billing", Kind: "convention",
		Title: "Billing retries", Content: "Billing retries never repeat charges.", Overrides: []uuid.UUID{dst.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, portal(f.owner), memory.ReviewInput{ID: ov.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}

	f.drain(t, f.exporter)
	team := subjects(f.history(t, f.team))
	if len(team) != 2 || team[0] != "promote convention Retry idempotently (revision 1)" || team[1] != "approve convention Retry idempotently (revision 1)" {
		t.Fatalf("team repository: %v", team)
	}
	pay := subjects(f.history(t, f.payments))
	if len(pay) != 3 || pay[2] != "supersede convention Retry idempotently (revision 1)" {
		t.Fatalf("payments repository: %v", pay)
	}
	td := f.file(t, f.team, mirror.FilePath("convention", dst.ID.String()))
	if td.Origin != "promotion" || td.Trust != "approved" {
		t.Fatalf("promoted file: %+v", td)
	}
	// The destination repository never names the source.
	for _, m := range f.history(t, f.team) {
		if strings.Contains(m, src.ID.String()) || strings.Contains(m, "payments") {
			t.Fatalf("team commit names the source: %q", m)
		}
	}
	sd := f.file(t, f.payments, mirror.FilePath("convention", src.ID.String()))
	if sd.Status != "superseded" || sd.SupersededBy != dst.ID.String() || sd.SupersedeReason != "promotion" {
		t.Fatalf("source file: %+v", sd)
	}
	od := f.file(t, f.billing, mirror.FilePath("convention", ov.ID.String()))
	if len(od.Overrides) != 1 || od.Overrides[0] != dst.ID.String() || !od.OverrideEffective {
		t.Fatalf("override file: %+v", od)
	}
	for _, sc := range []memory.Scope{f.team, f.payments, f.billing} {
		f.assertOneCommitPerEvent(t, sc)
	}
	if rep := f.verify(t); len(rep.Drift) != 0 {
		t.Fatalf("drift: %+v", rep.Drift)
	}
}

// flakyGit fails pushes on purpose: before pushing (remote down) or after
// a successful push (acknowledgement lost).
type flakyGit struct {
	mirror.Git
	failBefore, failAfter atomic.Int32
	pushes                atomic.Int32
}

type flakyRepo struct {
	mirror.Repo
	g *flakyGit
}

func (g *flakyGit) Open(ctx context.Context, repo string) (mirror.Repo, error) {
	r, err := g.Git.Open(ctx, repo)
	return &flakyRepo{Repo: r, g: g}, err
}

func (r *flakyRepo) Push(ctx context.Context) error {
	if r.g.failBefore.Add(-1) >= 0 {
		return mirror.ErrRemote
	}
	err := r.Repo.Push(ctx)
	if err == nil {
		r.g.pushes.Add(1)
		if r.g.failAfter.Add(-1) >= 0 {
			return errors.New("connection reset after push")
		}
	}
	return err
}

func TestMirrorRetriesWithoutDuplicates(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	fg := &flakyGit{Git: f.git}
	fg.failBefore.Store(2)
	fg.failAfter.Store(1)
	ex := mirror.NewExporter(f.db.Mirror, fg)
	for i := range 3 {
		if _, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact",
			Title: "Fact " + string(rune('A'+i)), Content: "Content."}); err != nil {
			t.Fatal(err)
		}
	}
	f.drain(t, ex)
	// Two failed attempts, then a push whose acknowledgement was lost: the
	// retry finds all three events in the remote history and pushes nothing.
	if fg.failAfter.Load() != 0 || fg.pushes.Load() != 1 {
		t.Fatalf("pushes = %d, failAfter = %d; the acknowledgement-loss path did not run as intended", fg.pushes.Load(), fg.failAfter.Load())
	}
	if n := len(f.history(t, f.payments)); n != 3 {
		t.Fatalf("%d commits for 3 events after retries", n)
	}
	f.assertOneCommitPerEvent(t, f.payments)
	var attempts int
	_ = f.db.Admin.QueryRow(ctx, "SELECT max(attempts) FROM mirror_state").Scan(&attempts)
	if attempts < 3 {
		t.Fatalf("attempts = %d, want retries recorded", attempts)
	}
	if rep := f.verify(t); len(rep.Drift) != 0 {
		t.Fatalf("drift: %+v", rep.Drift)
	}

	// Remote down for good: jobs dead-letter, the service keeps working,
	// and jobs retry-dead delivers once the remote is back.
	fg.failBefore.Store(1 << 20)
	if _, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "During outage", Content: "x"}); err != nil {
		t.Fatalf("writes must not depend on the mirror: %v", err)
	}
	if _, err := f.db.Admin.Exec(ctx, "UPDATE jobs SET max_attempts = 2 WHERE kind = 'git_mirror' AND status = 'pending'"); err != nil {
		t.Fatal(err)
	}
	f.drain(t, ex)
	var dead int
	_ = f.db.Admin.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE kind = 'git_mirror' AND status = 'dead'").Scan(&dead)
	if dead != 1 {
		t.Fatalf("dead jobs = %d, want 1", dead)
	}
	if rep := f.verify(t); len(rep.Drift) != 1 || rep.Drift[0].Kind != "undelivered" {
		t.Fatalf("verify during outage: %+v", rep.Drift)
	}
	fg.failBefore.Store(0)
	if _, err := db.New(f.db.Operator).RetryDeadJobs(ctx, mirror.JobKind); err != nil {
		t.Fatal(err)
	}
	f.drain(t, ex)
	f.assertOneCommitPerEvent(t, f.payments)
	if rep := f.verify(t); len(rep.Drift) != 0 {
		t.Fatalf("drift after recovery: %+v", rep.Drift)
	}
}

func TestMirrorVerifyDetectsDrift(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	m, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "Port", Content: "The API listens on 8080."})
	if err != nil {
		t.Fatal(err)
	}
	f.drain(t, f.exporter)
	if rep := f.verify(t); len(rep.Drift) != 0 {
		t.Fatalf("drift: %+v", rep.Drift)
	}

	// Someone edits the mirror directly.
	g, _ := mirror.NewGoGit(mirror.GoGitConfig{RemoteTemplate: "file://" + f.remoteDir + "/{repo}.git", CacheDir: t.TempDir()})
	r, _ := g.Open(ctx, f.payments.ID.String())
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	path := mirror.FilePath("fact", m.ID.String())
	b, _ := r.ReadFile(path)
	edited := strings.Replace(string(b), "8080", "9090", 1)
	if _, err := r.Commit(map[string][]byte{path: []byte(edited), "memories/fact/manual.md": []byte("hand-written\n")},
		"manual fix", mirror.Signature{Name: "Someone", Email: "someone@example.com", When: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(ctx); err != nil {
		t.Fatal(err)
	}
	rep := f.verify(t)
	kinds := map[string]int{}
	for _, d := range rep.Drift {
		kinds[d.Kind]++
	}
	if kinds["content_differs"] != 1 || kinds["unexpected_file"] != 1 || kinds["foreign_commit"] != 1 || len(rep.Drift) != 3 {
		t.Fatalf("drift = %+v", rep.Drift)
	}

	// The next export of that memory overwrites the edit (git is never
	// imported); the foreign commit and file stay flagged.
	content := "The API listens on 8081."
	if _, err := f.svc.Update(ctx, f.owner, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: &content}); err != nil {
		t.Fatal(err)
	}
	f.drain(t, f.exporter)
	if d := f.file(t, f.payments, path); d.Content != content {
		t.Fatalf("export did not overwrite the edit: %q", d.Content)
	}
	rep = f.verify(t)
	if len(rep.Drift) != 2 {
		t.Fatalf("drift after overwrite: %+v", rep.Drift)
	}

	// A principal rename is a note, not drift.
	if _, err := f.db.Admin.Exec(ctx, "UPDATE principals SET display_name = 'Renamed Owner' WHERE id = $1", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	rep = f.verify(t)
	if len(rep.Drift) != 2 || len(rep.Notes) == 0 {
		t.Fatalf("after rename: drift %+v notes %v", rep.Drift, rep.Notes)
	}
}

func TestMirrorSecretGateHoldsLaterEvents(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	m, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "Keys", Content: "Benign."})
	if err != nil {
		t.Fatal(err)
	}
	// A revision written before a scanner rule existed: inserted directly,
	// since the service rejects it today.
	secret := "the key is AKIAQ3EGRV7XKZ2MPLWT"
	if _, err := f.db.Admin.Exec(ctx, `
INSERT INTO memory_revisions (memory_id, revision, org_id, scope_id, kind, title, content, changed_by)
VALUES ($1, 2, $2, $3, 'fact', 'Keys', $4, $5)`, m.ID, f.org.ID, f.payments.ID, secret, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Admin.Exec(ctx, `UPDATE memories SET current_revision = 2, state_version = state_version + 1 WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Admin.Exec(ctx, `
INSERT INTO memory_events (org_id, scope_id, memory_id, revision, event_type, actor_id, snapshot)
VALUES ($1, $2, $3, 2, 'revised', $4, '{"status":"active","trust":"unverified"}')`, f.org.ID, f.payments.ID, m.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	cur, err := f.svc.Get(ctx, f.owner, m.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Archive(ctx, f.owner, memory.ArchiveInput{ID: m.ID, ExpectedStateVersion: cur.StateVersion}); err != nil {
		t.Fatal(err)
	}
	other, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "Other", Content: "Unaffected."})
	if err != nil {
		t.Fatal(err)
	}
	f.drain(t, f.exporter)

	states := map[string]string{}
	rows, _ := f.db.Admin.Query(ctx, `SELECT e.event_type || ':' || e.memory_id::text, s.delivery_state FROM mirror_state s JOIN memory_events e ON e.id = s.event_id`)
	for rows.Next() {
		var k, v string
		_ = rows.Scan(&k, &v)
		states[k] = v
	}
	rows.Close()
	if states["created:"+m.ID.String()] != "pushed" || states["revised:"+m.ID.String()] != "failed" ||
		states["archived:"+m.ID.String()] != "held" || states["created:"+other.ID.String()] != "pushed" {
		t.Fatalf("states: %v", states)
	}
	var dead int
	_ = f.db.Admin.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE kind = 'git_mirror' AND status = 'dead' AND last_error LIKE 'secret_detected%'").Scan(&dead)
	if dead != 1 {
		t.Fatalf("secret job dead-lettered: %d, want 1", dead)
	}
	for _, msg := range f.history(t, f.payments) {
		if strings.Contains(msg, "AKIA") {
			t.Fatal("secret reached the mirror")
		}
	}
	if d := f.file(t, f.payments, mirror.FilePath("fact", m.ID.String())); strings.Contains(d.Content, "AKIA") || d.Revision != 1 {
		t.Fatalf("file advanced past the blocked revision: %+v", d)
	}
	// Purge runbook: mark the memory purged; nothing of it is exported again.
	if _, err := db.New(f.db.Operator).MirrorMarkPurged(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.New(f.db.Operator).MirrorRequeueHeld(ctx, f.payments.ID.String()); n != 0 {
		t.Fatalf("requeued %d purged events", n)
	}
}

func TestMirrorDisasterRecoveryRebuild(t *testing.T) {
	f := newMirrorFixture(t)
	ctx := context.Background()
	a, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "decision", Title: "Approved one",
		Content: "Content A", Rationale: "Why A", SourceURI: "https://issues.example.com/A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Approve(ctx, portal(f.owner), memory.ReviewInput{ID: a.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	b, _ := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "Superseded one", Content: "B"})
	if _, _, err := f.svc.Supersede(ctx, f.owner, memory.SupersedeInput{ID: b.ID, ExpectedRevision: 1,
		Replacement: memory.CreateInput{Kind: "fact", Title: "Replacement", Content: "B2"}}); err != nil {
		t.Fatal(err)
	}
	c, _ := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "lesson", Title: "Archived one", Content: "C"})
	if _, err := f.svc.Archive(ctx, f.owner, memory.ArchiveInput{ID: c.ID, ExpectedStateVersion: c.StateVersion}); err != nil {
		t.Fatal(err)
	}
	f.drain(t, f.exporter)

	// Total database loss is simulated by restoring into a new scope.
	restored, err := f.admin.CreateScope(ctx, uuid.Nil, "acme/platform", "project", "payments-restored", "")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := f.git.Open(ctx, f.payments.ID.String())
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := mirror.Rebuild(ctx, memory.NewService(f.db.Operator), r, mirror.RebuildOptions{
		Scope: restored.Path, As: f.owner, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, it := range items {
		actions[it.OriginalID] = it.Action
	}
	if actions[a.ID.String()] != "created" || actions[b.ID.String()] != "skipped" || actions[c.ID.String()] != "archived" || len(items) != 4 {
		t.Fatalf("rebuild: %+v", items)
	}
	page, err := f.svc.Recent(ctx, f.owner, restored.Path, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page {
		if m.Trust == memory.TrustApproved {
			t.Fatalf("approval restored from git metadata: %+v", m)
		}
		if m.Title == "Approved one" && (m.Content != "Content A" || m.Rationale != "Why A" || m.SourceURI != "https://issues.example.com/A") {
			t.Fatalf("content not restored: %+v", m)
		}
	}
}

// TestMirrorThroughput measures export cost for one repository holding N
// memories (TEAM_MEMORY_MIRROR_BENCH=N), for docs/decisions/git-library.md.
func TestMirrorThroughput(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("TEAM_MEMORY_MIRROR_BENCH"))
	if n <= 0 {
		t.Skip("TEAM_MEMORY_MIRROR_BENCH not set")
	}
	drainTimeout = 30 * time.Minute
	f := newMirrorFixture(t)
	ctx := context.Background()
	for i := range n {
		if _, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact",
			Title: fmt.Sprintf("Fact %d", i), Content: strings.Repeat("Some realistic memory content. ", 20)}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	f.drain(t, f.exporter)
	bulk := time.Since(start)
	// Steady state: one more event on the large repository, from a warm
	// cache and from a cold one (a restarted pod).
	if _, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "one more", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	f.drain(t, f.exporter)
	warm := time.Since(start)
	g, _ := mirror.NewGoGit(mirror.GoGitConfig{RemoteTemplate: "file://" + f.remoteDir + "/{repo}.git", CacheDir: t.TempDir()})
	cold := mirror.NewExporter(f.db.Mirror, g)
	if _, err := f.svc.Create(ctx, f.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "and another", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	f.drain(t, cold)
	coldD := time.Since(start)
	t.Logf("memories=%d bulk_export=%s (%.1f events/s) next_event_warm=%s next_event_cold_clone=%s",
		n, bulk.Round(time.Millisecond), float64(n)/bulk.Seconds(), warm.Round(time.Millisecond), coldD.Round(time.Millisecond))
}
