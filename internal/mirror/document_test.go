package mirror

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleDocument() Document {
	at := time.Date(2026, 10, 22, 9, 30, 0, 123456000, time.UTC)
	return Document{
		EventID: "7c2c3a52-5d0e-4c55-9f1e-6d0bd1a3c0e1", EventSeq: 42, Event: "approved", EventAt: at,
		ActorID: "a1", ActorName: "Ada \"Reviewer\" Lovelace",
		ID: "m1", ScopeID: "s1", ScopePath: "acme/platform/payments", Revision: 3, Kind: "decision",
		Status: "active", Trust: "approved", SourceURI: "https://issues.example.com/PAY-1",
		CreatedByID: "c1", CreatedByName: "Grace", CreatedAt: at.Add(-time.Hour),
		RevisedByID: "c1", RevisedByName: "Grace", RevisedAt: at.Add(-time.Minute), ChangeNote: "clarify",
		ApproverID: "a1", ApproverName: "Ada \"Reviewer\" Lovelace", ApprovedAt: at, ApprovedRev: 3,
		Overrides: []string{"o1"}, OverrideEffective: true,
		Title: "Use PostgreSQL: ACID\nfor payments", Content: "---\nnot: frontmatter\n---\n\n## Rationale\n\nstill content ✓",
		Rationale: "Transactions.\n\n## Rationale\n\nnested heading",
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	cases := map[string]func(*Document){
		"full":            func(*Document) {},
		"no rationale":    func(d *Document) { d.Rationale = "" },
		"trailing nl":     func(d *Document) { d.Content += "\n\n" },
		"zero times":      func(d *Document) { d.ApprovedAt, d.ApproverID, d.ApproverName, d.ApprovedRev = time.Time{}, "", "", 0 },
		"no overrides":    func(d *Document) { d.Overrides, d.OverrideEffective = nil, false },
		"superseded":      func(d *Document) { d.Status, d.SupersededBy, d.SupersedeReason = "superseded", "m2", "promotion" },
		"promoted origin": func(d *Document) { d.Event, d.Origin = "promoted", "promotion" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := sampleDocument()
			mutate(&d)
			b := Render(d)
			got, err := Parse(b)
			if err != nil {
				t.Fatalf("parse: %v\n%s", err, b)
			}
			if d.Overrides == nil {
				d.Overrides = []string{}
			}
			if got.Overrides == nil {
				got.Overrides = []string{}
			}
			if !reflect.DeepEqual(got, d) {
				t.Fatalf("round trip differs:\n got %+v\nwant %+v", got, d)
			}
			if string(Render(got)) != string(b) {
				t.Fatal("render is not stable")
			}
		})
	}
}

func TestRenderIsYAMLFrontmatter(t *testing.T) {
	b := string(Render(sampleDocument()))
	if !strings.HasPrefix(b, "---\nevent_id: \"7c2c3a52") {
		t.Fatalf("unexpected start:\n%s", b[:80])
	}
	for _, want := range []string{"\nkind: \"decision\"\n", "\ntrust: \"approved\"\n", "\nrevision: 3\n", "\noverrides: [\"o1\"]\n",
		"\n---\n\n# Use PostgreSQL: ACID for payments\n\n"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestParseRejectsEditedFiles(t *testing.T) {
	good := string(Render(sampleDocument()))
	for name, s := range map[string]string{
		"no frontmatter":  "# hello\n",
		"content changed": strings.Replace(good, "still content", "edited content!", 1),
		"appended text":   good + "more\n",
		"bad line":        strings.Replace(good, "trust: ", "trust=", 1),
	} {
		if _, err := Parse([]byte(s)); !errors.Is(err, ErrNotMirrorDocument) {
			t.Errorf("%s: err = %v, want ErrNotMirrorDocument", name, err)
		}
	}
}

func TestCommitMessageAndTrailers(t *testing.T) {
	d := sampleDocument()
	msg := CommitMessage(d)
	if !strings.HasPrefix(msg, "approve decision Use PostgreSQL: ACID for payments (revision 3)\n") {
		t.Fatalf("subject: %q", strings.SplitN(msg, "\n", 2)[0])
	}
	id, seq, ok := Trailers(msg)
	if !ok || id != d.EventID || seq != 42 {
		t.Fatalf("trailers = %q %d %t", id, seq, ok)
	}
	if _, _, ok := Trailers("manual edit\n"); ok {
		t.Fatal("foreign commit parsed as mirror commit")
	}
	for ev, verb := range map[string]string{"created": "create", "revised": "update", "archived": "archive", "superseded": "supersede", "promoted": "promote"} {
		d.Event = ev
		if d.Action() != verb {
			t.Errorf("%s -> %s", ev, d.Action())
		}
	}
}
