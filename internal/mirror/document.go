// Package mirror exports memory lifecycle events to one private git
// repository per owning scope: one commit per exportable event, built from
// the event's historical snapshot. PostgreSQL stays the source of truth; the
// mirror is never imported during normal operation (see
// docs/decisions/mirror-security.md and docs/operations/mirror.md).
//
// Git access goes through the small Git/Repo interfaces in git.go so the
// library (go-git, gogit.go) can be swapped.
package mirror

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Document is the exported state of one memory as of one event. It renders
// to memories/<kind>/<memory-id>.md: frontmatter, then the title, content
// and rationale.
type Document struct {
	EventID   string    `json:"event_id"`
	EventSeq  int64     `json:"event_seq"`
	Event     string    `json:"event"` // created, revised, approved, archived, superseded, promoted
	EventAt   time.Time `json:"event_at"`
	ActorID   string    `json:"actor_id"`
	ActorName string    `json:"actor_name"`

	ID        string `json:"id"`
	ScopeID   string `json:"scope_id"`
	ScopePath string `json:"scope_path"`
	Revision  int32  `json:"revision"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Trust     string `json:"trust"`
	SourceURI string `json:"source_uri"`

	// Author of the memory (first revision) and of this revision's content.
	CreatedByID     string    `json:"created_by_id"`
	CreatedByName   string    `json:"created_by_name"`
	CreatedAt       time.Time `json:"created_at"`
	RevisedByID     string    `json:"revised_by_id"`
	RevisedByName   string    `json:"revised_by_name"`
	RevisedAt       time.Time `json:"revised_at"`
	ChangeNote      string    `json:"change_note"`
	ApproverID      string    `json:"approver_id"`
	ApproverName    string    `json:"approver_name"`
	ApprovedAt      time.Time `json:"approved_at"`
	ApprovedRev     int32     `json:"approved_revision"`
	SupersededBy    string    `json:"superseded_by"`
	SupersedeReason string    `json:"supersede_reason"`
	// Overrides are the revision's override links. OverrideEffective is true
	// when this exact revision was approved at the time of the event (the
	// only way an override takes effect).
	Overrides         []string `json:"overrides"`
	OverrideEffective bool     `json:"override_effective"`
	// Origin is "promotion" for a memory created by promoting a child
	// scope's memory. The source is deliberately not named here: the link to
	// the source exists only in the source scope's repository (its
	// supersession commit names this memory as the replacement).
	Origin string `json:"origin"`

	Title     string `json:"title"`
	Content   string `json:"-"`
	Rationale string `json:"-"`
}

// Path is the file the document lives in.
func (d Document) Path() string { return FilePath(d.Kind, d.ID) }

// FilePath returns memories/<kind>/<memory-id>.md.
func FilePath(kind, id string) string { return "memories/" + kind + "/" + id + ".md" }

// Action is the verb in the commit message.
func (d Document) Action() string {
	switch d.Event {
	case "created":
		return "create"
	case "revised":
		return "update"
	case "approved":
		return "approve"
	case "archived":
		return "archive"
	case "superseded":
		return "supersede"
	case "promoted":
		return "promote"
	}
	return d.Event
}

// frontmatter lists the keys in output order. Every value is written as a
// JSON scalar or array, which is also valid YAML, so the file parses with
// any YAML reader and round-trips exactly through Parse.
var frontmatter = []string{
	"event_id", "event_seq", "event", "event_at", "actor_id", "actor_name",
	"id", "scope_id", "scope_path", "revision", "kind", "status", "trust", "source_uri",
	"created_by_id", "created_by_name", "created_at", "revised_by_id", "revised_by_name", "revised_at", "change_note",
	"approver_id", "approver_name", "approved_at", "approved_revision",
	"superseded_by", "supersede_reason", "overrides", "override_effective", "origin",
	"title", "content_bytes", "rationale_bytes",
}

const rationaleHeading = "\n\n## Rationale\n\n"

// Render returns the file content. It is a pure function of d, so the same
// event always renders the same bytes (verify relies on that).
func Render(d Document) []byte {
	if d.Overrides == nil {
		d.Overrides = []string{}
	}
	fields := map[string]any{}
	raw, _ := json.Marshal(d)
	_ = json.Unmarshal(raw, &fields)
	fields["content_bytes"] = len(d.Content)
	fields["rationale_bytes"] = len(d.Rationale)
	for _, k := range []string{"event_at", "created_at", "revised_at", "approved_at"} {
		fields[k] = timeString(map[string]time.Time{
			"event_at": d.EventAt, "created_at": d.CreatedAt, "revised_at": d.RevisedAt, "approved_at": d.ApprovedAt}[k])
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	for _, k := range frontmatter {
		v, _ := json.Marshal(fields[k])
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	b.WriteString("---\n\n# ")
	b.WriteString(oneLine(d.Title))
	b.WriteString("\n\n")
	b.WriteString(d.Content)
	if d.Rationale != "" {
		b.WriteString(rationaleHeading)
		b.WriteString(d.Rationale)
	}
	b.WriteString("\n")
	return b.Bytes()
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func oneLine(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ") }

// ErrNotMirrorDocument means a file is not in the format Render writes.
var ErrNotMirrorDocument = errors.New("not a team memory mirror document")

// Parse reads a file written by Render. It is used by verify and by the
// disaster-recovery rebuild; it never feeds normal operation.
func Parse(b []byte) (Document, error) {
	var d Document
	s := string(b)
	if !strings.HasPrefix(s, "---\n") {
		return d, ErrNotMirrorDocument
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		return d, ErrNotMirrorDocument
	}
	head, body := s[4:4+end+1], s[4+end+5:]
	fields := map[string]json.RawMessage{}
	for _, line := range strings.Split(strings.TrimSuffix(head, "\n"), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			return d, fmt.Errorf("%w: bad frontmatter line %q", ErrNotMirrorDocument, line)
		}
		fields[k] = json.RawMessage(v)
	}
	var n, rn int
	if err := json.Unmarshal(fields["content_bytes"], &n); err != nil {
		return d, fmt.Errorf("%w: content_bytes: %v", ErrNotMirrorDocument, err)
	}
	if err := json.Unmarshal(fields["rationale_bytes"], &rn); err != nil {
		return d, fmt.Errorf("%w: rationale_bytes: %v", ErrNotMirrorDocument, err)
	}
	times := map[string]*time.Time{"event_at": &d.EventAt, "created_at": &d.CreatedAt, "revised_at": &d.RevisedAt, "approved_at": &d.ApprovedAt}
	for k, dst := range times {
		var ts string
		if err := json.Unmarshal(fields[k], &ts); err != nil {
			return d, fmt.Errorf("%w: %s: %v", ErrNotMirrorDocument, k, err)
		}
		delete(fields, k)
		if ts == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return d, fmt.Errorf("%w: %s: %v", ErrNotMirrorDocument, k, err)
		}
		*dst = t
	}
	raw, _ := json.Marshal(fields)
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("%w: %v", ErrNotMirrorDocument, err)
	}
	prefix := "\n# " + oneLine(d.Title) + "\n\n"
	if !strings.HasPrefix(body, prefix) || len(body) < len(prefix)+n+1 {
		return d, fmt.Errorf("%w: body does not match its frontmatter", ErrNotMirrorDocument)
	}
	body = body[len(prefix):]
	d.Content, body = body[:n], body[n:]
	switch {
	case rn == 0 && body == "\n":
	case rn > 0 && strings.HasPrefix(body, rationaleHeading) && len(body) == len(rationaleHeading)+rn+1 && strings.HasSuffix(body, "\n"):
		d.Rationale = body[len(rationaleHeading) : len(body)-1]
	default:
		return d, fmt.Errorf("%w: unexpected text after the content", ErrNotMirrorDocument)
	}
	return d, nil
}

// Trailer keys in commit messages. The event trailer makes exports
// idempotent: a retry finds the commit and does not write another.
const (
	TrailerEvent    = "Team-Memory-Event"
	TrailerEventSeq = "Team-Memory-Event-Seq"
	TrailerMemory   = "Team-Memory-Memory"
)

// CommitMessage states the action and revision, with trailers.
func CommitMessage(d Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s (revision %d)\n\n", d.Action(), d.Kind, truncateRunes(oneLine(d.Title), 72), d.Revision)
	fmt.Fprintf(&b, "Status: %s\nTrust: %s\n", d.Status, d.Trust)
	if d.ChangeNote != "" && (d.Event == "created" || d.Event == "revised") {
		fmt.Fprintf(&b, "Change note: %s\n", truncateRunes(oneLine(d.ChangeNote), 200))
	}
	fmt.Fprintf(&b, "\n%s: %s\n%s: %s\n%s: %d\n", TrailerMemory, d.ID, TrailerEvent, d.EventID, TrailerEventSeq, d.EventSeq)
	return b.String()
}

// Trailers extracts the event id and sequence from a commit message; ok is
// false for commits the mirror did not write.
func Trailers(msg string) (eventID string, seq int64, ok bool) {
	for _, line := range strings.Split(msg, "\n") {
		if v, found := strings.CutPrefix(line, TrailerEvent+": "); found {
			eventID = strings.TrimSpace(v)
		}
		if v, found := strings.CutPrefix(line, TrailerEventSeq+": "); found {
			_, _ = fmt.Sscan(strings.TrimSpace(v), &seq)
		}
	}
	return eventID, seq, eventID != "" && seq > 0
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
