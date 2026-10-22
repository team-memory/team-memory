package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/secrets"
)

// JobKind is the jobs.kind of mirror exports.
const JobKind = "git_mirror"

// Metrics receives exporter events (Prometheus in production).
type Metrics interface {
	Exported(repo string, commits int, d time.Duration)
	PushFailed(repo string, err error)
	SecretDetected(repo string)
	Held(repo string)
}

type nopMetrics struct{}

func (nopMetrics) Exported(string, int, time.Duration) {}
func (nopMetrics) PushFailed(string, error)            {}
func (nopMetrics) SecretDetected(string)               {}
func (nopMetrics) Held(string)                         {}

// Exporter turns git_mirror jobs into commits. Delivery is at least once
// and idempotent: every commit carries its event id as a trailer, and an
// export first looks for the event in the remote history, so a retry after
// a push that succeeded but whose acknowledgement was lost writes nothing.
type Exporter struct {
	pool *pgxpool.Pool
	git  Git
	// EmailDomain builds author addresses <principal-id>@<domain>. Real
	// email addresses are never exported.
	EmailDomain string
	// LockTimeout bounds the wait for another worker exporting the same
	// repository (default 1m).
	LockTimeout time.Duration
	Metrics     Metrics
	Logger      *slog.Logger
}

// NewExporter returns an exporter. pool connects as a member of
// team_memory_mirror.
func NewExporter(pool *pgxpool.Pool, git Git) *Exporter {
	return &Exporter{pool: pool, git: git, EmailDomain: "users.noreply.team-memory.invalid", LockTimeout: time.Minute,
		Metrics: nopMetrics{}, Logger: slog.New(slog.DiscardHandler)}
}

// Worker returns a jobs worker for mirror exports.
func (e *Exporter) Worker(cfg jobs.Config) *jobs.Worker {
	return jobs.NewWorker(e.pool, JobKind, e.Handle, cfg)
}

type jobPayload struct {
	EventID    uuid.UUID `json:"event_id"`
	Repository string    `json:"repository"`
}

// errSecretDetected dead-letters the job: an operator must act (purge).
var errSecretDetected = errors.New("secret_detected: the event was not exported; see the purge runbook")

// Handle exports a batch of jobs. Each repository is exported once per
// batch, which delivers every undelivered event of that repository in
// order, including events whose own jobs are not in this batch.
func (e *Exporter) Handle(ctx context.Context, batch []jobs.Job) []jobs.Outcome {
	out := make([]jobs.Outcome, len(batch))
	payloads := make([]jobPayload, len(batch))
	byRepo := map[string][]int{}
	var order []string
	for i, j := range batch {
		if err := json.Unmarshal(j.Payload, &payloads[i]); err != nil || payloads[i].Repository == "" {
			out[i] = jobs.Outcome{Err: fmt.Errorf("bad git_mirror payload: %v", err), Permanent: true}
			continue
		}
		if _, ok := byRepo[payloads[i].Repository]; !ok {
			order = append(order, payloads[i].Repository)
		}
		byRepo[payloads[i].Repository] = append(byRepo[payloads[i].Repository], i)
	}
	for _, repo := range order {
		err := e.ExportRepository(ctx, repo)
		for _, i := range byRepo[repo] {
			out[i] = e.outcome(ctx, payloads[i], err)
		}
	}
	return out
}

// outcome decides a job's result from its event's delivery state after the
// repository export.
func (e *Exporter) outcome(ctx context.Context, p jobPayload, exportErr error) jobs.Outcome {
	st, err := db.New(e.pool).MirrorGetState(ctx, db.MirrorGetStateParams{EventID: p.EventID, Repository: p.Repository})
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Outcome{Err: errors.New("no mirror state for this event"), Permanent: true}
	}
	if err != nil {
		return jobs.Outcome{Err: err}
	}
	switch st.DeliveryState {
	case "pushed", "held", "purged":
		return jobs.Outcome{}
	case "failed":
		return jobs.Outcome{Err: errSecretDetected, Permanent: true}
	}
	if exportErr == nil {
		exportErr = errors.New("event not delivered")
	}
	return jobs.Outcome{Err: exportErr}
}

// ExportRepository delivers every undelivered event of one repository, in
// event order, holding the repository's advisory lock throughout.
func (e *Exporter) ExportRepository(ctx context.Context, repo string) error {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	lctx, cancel := context.WithTimeout(ctx, e.LockTimeout)
	_, err = conn.Exec(lctx, "SELECT pg_advisory_lock(hashtextextended('tm_mirror:' || $1::text, 0))", repo)
	cancel()
	if err != nil {
		return fmt.Errorf("mirror: lock repository %s: %w", repo, err)
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer ucancel()
		if _, err := conn.Exec(uctx, "SELECT pg_advisory_unlock(hashtextextended('tm_mirror:' || $1::text, 0))", repo); err != nil {
			// The lock dies with the session; drop the connection.
			_ = conn.Conn().Close(uctx)
		}
	}()

	q := db.New(e.pool)
	pending, err := q.MirrorUndelivered(ctx, repo)
	if err != nil || len(pending) == 0 {
		return err
	}
	if err := q.MirrorCountAttempt(ctx, repo); err != nil {
		return err
	}
	start := time.Now()
	r, err := e.git.Open(ctx, repo)
	if err != nil {
		return e.pushFailed(ctx, repo, pending, err)
	}
	// One retry after a non-fast-forward rejection: sync and rebuild the
	// commits on the new remote head.
	for attempt := 0; ; attempt++ {
		n, err := e.exportOnce(ctx, q, r, repo, pending)
		if errors.Is(err, ErrNonFastForward) && attempt == 0 {
			e.Logger.Warn("mirror push rejected as non-fast-forward; resyncing", "repository", repo)
			continue
		}
		if err != nil {
			return e.pushFailed(ctx, repo, pending, err)
		}
		e.Metrics.Exported(repo, n, time.Since(start))
		return nil
	}
}

func (e *Exporter) pushFailed(ctx context.Context, repo string, pending []db.MirrorUndeliveredRow, cause error) error {
	e.Metrics.PushFailed(repo, cause)
	msg := truncate(cause.Error())
	for _, p := range pending {
		_ = db.New(e.pool).MirrorMarkState(ctx, db.MirrorMarkStateParams{
			DeliveryState: p.DeliveryState, LastError: &msg, EventID: p.EventID, Repository: repo})
	}
	return cause
}

type delivered struct {
	event uuid.UUID
	sha   string
}

func (e *Exporter) exportOnce(ctx context.Context, q *db.Queries, r Repo, repo string, pending []db.MirrorUndeliveredRow) (int, error) {
	if err := r.Sync(ctx); err != nil {
		return 0, err
	}
	present, err := eventsInHistory(r, pending[0].EventSeq)
	if err != nil {
		return 0, err
	}
	var done []delivered
	commits := 0
	for _, p := range pending {
		if sha, ok := present[p.EventID.String()]; ok {
			done = append(done, delivered{p.EventID, sha})
			continue
		}
		blocked, err := q.MirrorMemoryBlocked(ctx, db.MirrorMemoryBlockedParams{MemoryID: p.MemoryID, Repository: repo, EventSeq: p.EventSeq})
		if err != nil {
			return commits, err
		}
		if blocked {
			reason := "held: an earlier event of this memory was not exported"
			if err := q.MirrorMarkState(ctx, db.MirrorMarkStateParams{DeliveryState: "held", LastError: &reason, EventID: p.EventID, Repository: repo}); err != nil {
				return commits, err
			}
			e.Metrics.Held(repo)
			continue
		}
		doc, err := LoadDocument(ctx, q, p.EventID)
		if err != nil {
			return commits, err
		}
		if findings := ScanDocument(doc); len(findings) > 0 {
			reason := "secret_detected: " + findingNames(findings)
			if err := q.MirrorMarkState(ctx, db.MirrorMarkStateParams{DeliveryState: "failed", LastError: &reason, EventID: p.EventID, Repository: repo}); err != nil {
				return commits, err
			}
			e.Metrics.SecretDetected(repo)
			e.Logger.Error("mirror export blocked: secret detected; follow the purge runbook",
				"repository", repo, "event", p.EventID, "memory", p.MemoryID, "rules", findingNames(findings))
			continue
		}
		changes, err := e.changesFor(r, doc)
		if err != nil {
			return commits, err
		}
		sha, err := r.Commit(changes, CommitMessage(doc), Signature{
			Name: doc.ActorName, Email: doc.ActorID + "@" + e.EmailDomain, When: doc.EventAt})
		if err != nil {
			return commits, err
		}
		commits++
		if err := q.MirrorMarkState(ctx, db.MirrorMarkStateParams{DeliveryState: "committed", CommitSha: &sha, EventID: p.EventID, Repository: repo}); err != nil {
			return commits, err
		}
		done = append(done, delivered{p.EventID, sha})
	}
	if commits > 0 {
		if err := r.Push(ctx); err != nil {
			return commits, err
		}
	}
	for _, d := range done {
		sha := d.sha
		if err := q.MirrorMarkState(ctx, db.MirrorMarkStateParams{DeliveryState: "pushed", CommitSha: &sha, EventID: d.event, Repository: repo}); err != nil {
			return commits, err
		}
	}
	return commits, nil
}

// changesFor writes the document and removes the memory's file under any
// other kind directory (a revision may change the kind).
func (e *Exporter) changesFor(r Repo, d Document) (map[string][]byte, error) {
	changes := map[string][]byte{d.Path(): Render(d)}
	files, err := r.ListFiles("memories")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if strings.HasSuffix(f, "/"+d.ID+".md") && f != d.Path() {
			changes[f] = nil
		}
	}
	return changes, nil
}

// maxHistoryScan bounds the search for already-pushed events when the
// history contains commits the mirror did not write.
const maxHistoryScan = 100000

// eventsInHistory maps event ids to commit hashes for mirror commits from
// HEAD back to the first commit older than minSeq. Commits are written in
// event order per repository, so older history cannot hold pending events.
func eventsInHistory(r Repo, minSeq int64) (map[string]string, error) {
	present := map[string]string{}
	n := 0
	err := r.Log(func(c Commit) bool {
		n++
		id, seq, ok := Trailers(c.Message)
		if ok {
			if seq < minSeq {
				return false
			}
			present[id] = c.SHA
		}
		return n < maxHistoryScan
	})
	return present, err
}

// LoadDocument builds the document for one event from immutable rows.
func LoadDocument(ctx context.Context, q *db.Queries, eventID uuid.UUID) (Document, error) {
	row, err := q.MirrorEventExport(ctx, eventID)
	if err != nil {
		return Document{}, fmt.Errorf("mirror: load event %s: %w", eventID, err)
	}
	var snap struct {
		Status       string `json:"status"`
		Trust        string `json:"trust"`
		SupersededBy string `json:"superseded_by"`
		Replacement  string `json:"replacement"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(row.Snapshot, &snap); err != nil {
		return Document{}, fmt.Errorf("mirror: event %s snapshot: %w", eventID, err)
	}
	d := Document{
		EventID: row.ID.String(), EventSeq: row.Seq, Event: row.EventType, EventAt: row.EventAt,
		ActorID: row.ActorID.String(), ActorName: row.ActorName,
		ID: row.MemoryID.String(), ScopeID: row.ScopeID.String(), ScopePath: row.ScopePath,
		Revision: row.Revision, Kind: row.Kind, Status: snap.Status, Trust: snap.Trust,
		CreatedByID: row.CreatedBy.String(), CreatedByName: row.CreatedByName, CreatedAt: row.MemoryCreatedAt,
		RevisedByID: row.ChangedBy.String(), RevisedByName: row.ChangedByName, RevisedAt: row.RevisedAt,
		ChangeNote: row.ChangeNote, Title: row.Title, Content: row.Content, Rationale: row.Rationale,
	}
	if row.EventType == "promotion_finalized" {
		d.Event = "promoted"
	}
	if row.Promoted {
		d.Origin = "promotion"
	}
	if row.SourceUri != nil {
		d.SourceURI = *row.SourceUri
	}
	if d.Status == "superseded" {
		d.SupersededBy = snap.SupersededBy
		if d.SupersededBy == "" {
			d.SupersededBy = snap.Replacement
		}
		d.SupersedeReason = snap.Reason
	}
	approvals, err := q.MirrorApprovalAsOf(ctx, db.MirrorApprovalAsOfParams{MemoryID: row.MemoryID, Revision: row.Revision, Seq: row.Seq})
	if err != nil {
		return Document{}, err
	}
	if len(approvals) == 1 {
		a := approvals[0]
		d.ApproverID, d.ApproverName, d.ApprovedAt, d.ApprovedRev = a.ReviewerID.String(), a.ReviewerName, a.DecidedAt, row.Revision
	}
	ovs, err := q.MirrorRevisionOverrides(ctx, db.MirrorRevisionOverridesParams{MemoryID: row.MemoryID, Revision: row.Revision})
	if err != nil {
		return Document{}, err
	}
	for _, o := range ovs {
		d.Overrides = append(d.Overrides, o.String())
	}
	d.OverrideEffective = len(ovs) > 0 && len(approvals) == 1 && d.Status == "active"
	return d, nil
}

// ScanDocument rescans every exported free-text field with the current
// secret rules (mirror-security.md, "Gate").
func ScanDocument(d Document) []secrets.Finding {
	return secrets.Scan(map[string]string{
		"title": d.Title, "content": d.Content, "rationale": d.Rationale, "source_uri": d.SourceURI,
		"change_note": d.ChangeNote, "scope_path": d.ScopePath,
		"actor_name": d.ActorName, "created_by_name": d.CreatedByName, "revised_by_name": d.RevisedByName, "approver_name": d.ApproverName,
	})
}

func findingNames(fs []secrets.Finding) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, f.Field+":"+f.Rule)
	}
	return strings.Join(parts, ",")
}

func truncate(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}
