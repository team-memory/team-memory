package portal

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/secrets"
)

// --- memory page ----------------------------------------------------------

// maxDiffedRevisions bounds the work a page view does: any writer can add
// revisions, and each diff builds a table of up to 1500x1500 cells.
const maxDiffedRevisions = 20

type revView struct {
	memory.Revision
	Diff     []DiffLine
	DiffNote string
	Fields   []string // which of title/kind/rationale/source changed
	Author   string
}

type eventView struct {
	memory.Event
	Actor string
	Label string
}

type promoteOption struct {
	Parent        memory.Scope
	DestMaint     bool // the viewer can accept at the destination too
	ParentVisible bool
}

type memoryView struct {
	M          memory.Memory
	Role       memory.Role
	Revisions  []revView
	Events     []eventView
	Names      map[uuid.UUID]string
	Chain      []memory.Memory // supersession chain after this memory
	Promoted   *memory.Memory
	Review     bool // show the review panel
	Findings   []secrets.Finding
	Conflicts  []memory.ConflictCandidate
	Overrides  []memory.Memory // memories this one overrides
	Promote    *promoteOption
	CanWrite   bool
	From       string
	StateVer   int64
	Kinds      []memory.Kind
	MarkTarget map[uuid.UUID]bool
}

var eventLabels = map[string]string{
	"created": "Created", "revised": "New revision", "review_requested": "Review requested", "approved": "Approved",
	"review_rejected": "Review rejected", "superseded": "Superseded", "archived": "Archived",
	"promotion_finalized": "Promoted here", "promoted": "Promoted",
}

func (p *Portal) memoryPage(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	from := r.URL.Query().Get("scope")
	m, err := p.o.Memory.Get(r.Context(), q.p, id, from)
	if err != nil && from != "" {
		// A stale or foreign "viewed from" scope must not break the page or
		// reveal anything: fall back to a plain read.
		from = ""
		m, err = p.o.Memory.Get(r.Context(), q.p, id, "")
	}
	if err != nil {
		return err
	}
	h, err := p.o.Memory.History(r.Context(), q.p, id)
	if err != nil {
		return err
	}
	v := memoryView{M: m, From: from, StateVer: m.StateVersion, Kinds: memory.Kinds}
	v.Role, _, _ = p.o.Memory.EffectiveRoleOn(r.Context(), q.p, m.ScopeID.String())
	v.CanWrite = v.Role.AtLeast(memory.RoleWriter)

	ids := []uuid.UUID{m.CreatedBy, m.RevisedBy}
	for _, e := range h.Events {
		ids = append(ids, e.ActorID)
	}
	for _, rv := range h.Revisions {
		ids = append(ids, rv.ChangedBy)
	}
	if m.Review != nil {
		ids = append(ids, m.Review.Reviewer)
	}
	v.Names = p.o.Memory.DisplayNames(r.Context(), q.p, ids)

	for i, rv := range h.Revisions {
		x := revView{Revision: rv, Author: v.Names[rv.ChangedBy]}
		if i > 0 {
			prev := h.Revisions[i-1]
			if i >= len(h.Revisions)-maxDiffedRevisions {
				var tooBig bool
				x.Diff, tooBig = lineDiff(prev.Content, rv.Content)
				if tooBig {
					x.DiffNote = "Content is too large to diff here."
				}
			} else {
				x.DiffNote = "Diffs are shown for the most recent revisions only."
			}
			for _, c := range []struct {
				name string
				diff bool
			}{{"title", prev.Title != rv.Title}, {"kind", prev.Kind != rv.Kind}, {"rationale", prev.Rationale != rv.Rationale},
				{"source", prev.SourceURI != rv.SourceURI}, {"overrides", !sameIDs(prev.Overrides, rv.Overrides)}} {
				if c.diff {
					x.Fields = append(x.Fields, c.name)
				}
			}
		}
		v.Revisions = append(v.Revisions, x)
	}
	for i := len(h.Events) - 1; i >= 0; i-- { // newest first
		e := h.Events[i]
		label := eventLabels[e.Type]
		if label == "" {
			label = e.Type
		}
		v.Events = append(v.Events, eventView{Event: e, Actor: v.Names[e.ActorID], Label: label})
	}
	// Supersession chain, following only what the viewer can read.
	next := m.SupersededBy
	for hops := 0; next != nil && hops < 10; hops++ {
		n, err := p.o.Memory.Get(r.Context(), q.p, *next, "")
		if err != nil {
			break
		}
		v.Chain = append(v.Chain, n)
		next = n.SupersededBy
	}
	if m.PromotedFrom != nil {
		if src, err := p.o.Memory.Get(r.Context(), q.p, *m.PromotedFrom, ""); err == nil {
			v.Promoted = &src
		}
	}
	for _, oid := range m.Overrides {
		if o, err := p.o.Memory.Get(r.Context(), q.p, oid, ""); err == nil {
			v.Overrides = append(v.Overrides, o)
		}
	}
	if m.Status == memory.StatusActive && v.Role.AtLeast(memory.RoleMaintainer) {
		v.Review = true
		v.Findings, _ = p.o.Memory.SecretScan(r.Context(), q.p, id)
	}
	if m.Status == memory.StatusActive {
		v.Conflicts, _ = p.o.Memory.ConflictsFor(r.Context(), q.p, id)
		v.MarkTarget = map[uuid.UUID]bool{}
		for _, c := range v.Conflicts {
			// Only the deeper memory can override, and only a writer there.
			if c.Memory.ID == m.ID && v.CanWrite && !slicesContains(m.Overrides, c.Other.ID) {
				v.MarkTarget[c.Other.ID] = true
			}
		}
		if v.Review && m.ScopeID != uuid.Nil {
			if chain, err := p.o.Memory.VisibleScopes(r.Context(), q.p, m.ScopeID.String()); err == nil && len(chain) > 1 {
				opt := &promoteOption{Parent: chain[1].Scope, ParentVisible: true}
				if role, _, err := p.o.Memory.EffectiveRoleOn(r.Context(), q.p, chain[1].ID.String()); err == nil {
					opt.DestMaint = role.AtLeast(memory.RoleMaintainer)
				}
				v.Promote = opt
			}
		}
	}
	return p.show(q, "memory.html", m.Title, v)
}

func slicesContains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func sameIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slicesContains(b, x) {
			return false
		}
	}
	return true
}

// --- actions --------------------------------------------------------------------

func formInt(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.PostForm.Get(name))
	return n
}

func formInt64(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PostForm.Get(name), 10, 64)
	return n
}

func (p *Portal) memURL(id uuid.UUID, r *http.Request) string {
	u := p.base + "/m/" + id.String()
	if s := r.PostForm.Get("scope"); s != "" && !strings.ContainsAny(s, "&?#% \"'<>") {
		u += "?scope=" + s
	}
	return u
}

func (p *Portal) approve(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	in := memory.ReviewInput{ID: id, Revision: formInt(r, "revision"), ExpectedStateVersion: formInt64(r, "state_version"),
		Note: strings.TrimSpace(r.PostForm.Get("note"))}
	if _, err := p.o.Memory.Approve(r.Context(), q.p, in); err != nil {
		if errors.Is(err, memory.ErrNotActive) {
			if done, rerr := p.alreadyPromoted(q, r, id); done {
				return rerr
			}
		}
		// A double submit carries the state version from before the first
		// click. If that revision is now approved, the intent is met.
		var c *memory.ConflictError
		if !errors.As(err, &c) || !p.alreadyDecided(r, q, id, in.Revision, "approved") {
			return err
		}
	}
	if r.PostForm.Get("promote") == "1" {
		// Promotion needs its own explicit consent.
		if r.PostForm.Get("consent") != "1" {
			return p.redirect(q, p.memURL(id, r)+sep(p.memURL(id, r))+"err=consent", "approved")
		}
		return p.doPromote(q, r, id, memory.PromotionInput{ID: id, Revision: in.Revision, Rationale: strings.TrimSpace(r.PostForm.Get("promote_rationale"))}, true)
	}
	return p.redirect(q, p.memURL(id, r), "approved")
}

// alreadyDecided reports whether the memory's current revision is the one
// the form named and already carries the wanted review decision.
func (p *Portal) alreadyDecided(r *http.Request, q *req, id uuid.UUID, revision int, decision string) bool {
	m, err := p.o.Memory.Get(r.Context(), q.p, id, "")
	return err == nil && m.Revision == revision && m.Review != nil && m.Review.Decision == decision &&
		(decision != "approved" || m.Trust == memory.TrustApproved)
}

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

func (p *Portal) reject(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if note == "" {
		return &memory.ValidationError{Msg: "a note is required when rejecting, so the author knows what to change"}
	}
	in := memory.ReviewInput{ID: id, Revision: formInt(r, "revision"), ExpectedStateVersion: formInt64(r, "state_version"), Note: note}
	if _, err := p.o.Memory.Reject(r.Context(), q.p, in); err != nil {
		var c *memory.ConflictError
		if !errors.As(err, &c) || !p.alreadyDecided(r, q, id, in.Revision, "rejected") {
			return err
		}
	}
	return p.redirect(q, p.memURL(id, r), "rejected")
}

func (p *Portal) archive(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	if _, err := p.o.Memory.Archive(r.Context(), q.p, memory.ArchiveInput{ID: id, ExpectedStateVersion: formInt64(r, "state_version"),
		Reason: strings.TrimSpace(r.PostForm.Get("reason"))}); err != nil {
		return err
	}
	return p.redirect(q, p.memURL(id, r), "archived")
}

func (p *Portal) supersede(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	cur, err := p.o.Memory.Get(r.Context(), q.p, id, "")
	if err != nil {
		return err
	}
	f := r.PostForm
	old, repl, err := p.o.Memory.Supersede(r.Context(), q.p, memory.SupersedeInput{ID: id, ExpectedRevision: formInt(r, "revision"),
		Reason: strings.TrimSpace(f.Get("reason")), Scope: cur.OriginScope,
		Replacement: memory.CreateInput{Scope: cur.OriginScope, Kind: f.Get("kind"), Title: strings.TrimSpace(f.Get("title")),
			Content: f.Get("content"), Rationale: f.Get("rationale"), SourceURI: strings.TrimSpace(f.Get("source_uri")),
			ChangeNote: "Replaces " + old2(cur.Title)}})
	_ = old
	if err != nil {
		return err
	}
	return p.redirect(q, p.base+"/m/"+repl.ID.String(), "superseded")
}

func old2(t string) string { return clip(t, 80) }

func (p *Portal) promote(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	if r.PostForm.Get("consent") != "1" {
		return &memory.ValidationError{Msg: "tick the box to consent to sharing this snapshot with the parent scope's reviewers"}
	}
	return p.doPromote(q, r, id, memory.PromotionInput{ID: id, Revision: formInt(r, "revision"),
		Rationale: strings.TrimSpace(r.PostForm.Get("promote_rationale"))}, false)
}

// destMaintainer reports, from the database and not from the form, whether
// the viewer also maintains the parent of the memory's scope.
func (p *Portal) destMaintainer(q *req, r *http.Request, id uuid.UUID) bool {
	m, err := p.o.Memory.Get(r.Context(), q.p, id, "")
	if err != nil {
		return false
	}
	chain, err := p.o.Memory.VisibleScopes(r.Context(), q.p, m.ScopeID.String())
	if err != nil || len(chain) < 2 {
		return false
	}
	role, _, err := p.o.Memory.EffectiveRoleOn(r.Context(), q.p, chain[1].ID.String())
	return err == nil && role.AtLeast(memory.RoleMaintainer)
}

// doPromote completes the promotion in one step when the viewer holds both
// authorities, and files a request otherwise. A repeated submit lands on the
// same result.
func (p *Portal) doPromote(q *req, r *http.Request, id uuid.UUID, pin memory.PromotionInput, afterApprove bool) error {
	fail := func(err error) error {
		if errors.Is(err, memory.ErrNotActive) {
			if done, rerr := p.alreadyPromoted(q, r, id); done {
				return rerr
			}
		}
		if afterApprove && (errors.Is(err, memory.ErrPromotionStale) || errors.Is(err, memory.ErrForbidden) || errors.Is(err, memory.ErrNotFound)) {
			u := p.memURL(id, r)
			return p.redirect(q, u+sep(u)+"err=promote_failed", "approved")
		}
		return err
	}
	if p.destMaintainer(q, r, id) {
		if _, err := p.o.Memory.PromoteDirect(r.Context(), q.p, pin); err != nil {
			// The request exists once submitted; if acceptance was refused
			// (authority changed meanwhile), send the person to it.
			if errors.Is(err, memory.ErrForbidden) || errors.Is(err, memory.ErrNotFound) {
				if req, serr := p.o.Memory.SubmitPromotion(r.Context(), q.p, pin); serr == nil {
					return p.redirect(q, p.base+"/promotions/"+req.ID.String(), "requested")
				}
			}
			return fail(err)
		}
		return p.redirect(q, p.memURL(id, r), "promoted")
	}
	req, err := p.o.Memory.SubmitPromotion(r.Context(), q.p, pin)
	if err != nil {
		return fail(err)
	}
	return p.redirect(q, p.base+"/promotions/"+req.ID.String(), "requested")
}

// alreadyPromoted handles a double submit: if the memory was just promoted
// (superseded with a replacement the viewer can read), go to the result.
func (p *Portal) alreadyPromoted(q *req, r *http.Request, id uuid.UUID) (bool, error) {
	m, err := p.o.Memory.Get(r.Context(), q.p, id, "")
	if err != nil || m.Status != memory.StatusSuperseded || m.SupersededBy == nil {
		return false, nil
	}
	return true, p.redirect(q, p.base+"/m/"+m.SupersededBy.String(), "promoted")
}

func (p *Portal) markOverride(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	target, err := uuid.Parse(r.PostForm.Get("overridden"))
	if err != nil {
		return &memory.ValidationError{Msg: "choose the inherited memory to override"}
	}
	cur, err := p.o.Memory.Get(r.Context(), q.p, id, "")
	if err != nil {
		return err
	}
	links := append(append([]uuid.UUID(nil), cur.Overrides...), target)
	if slicesContains(cur.Overrides, target) {
		links = cur.Overrides
	}
	_, err = p.o.Memory.Update(r.Context(), q.p, memory.UpdateInput{ID: id, ExpectedRevision: formInt(r, "revision"),
		Overrides: &links, ChangeNote: "Propose override of memory " + target.String()[:8]})
	if err != nil {
		return err
	}
	return p.redirect(q, p.memURL(id, r), "override")
}

// --- review queue ---------------------------------------------------------------------

type queueRow struct {
	memory.QueueItem
	Conflicts int
	Author    string
}

type queueView struct {
	Scope     memory.Scope
	Items     []queueRow
	Promotion []memory.PromotionRequest
}

func (p *Portal) reviewPage(w http.ResponseWriter, r *http.Request, q *req) error {
	sp := r.PathValue("path")
	items, err := p.o.Memory.ReviewQueue(r.Context(), q.p, sp, 50)
	if err != nil {
		return err
	}
	_, sc, err := p.o.Memory.EffectiveRoleOn(r.Context(), q.p, sp)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for _, it := range items {
		ids = append(ids, it.CreatedBy, it.RevisedBy)
	}
	names := p.o.Memory.DisplayNames(r.Context(), q.p, ids)
	v := queueView{Scope: sc}
	for i, it := range items {
		row := queueRow{QueueItem: it, Author: names[it.RevisedBy]}
		if i < 25 {
			c, _ := p.o.Memory.ConflictsFor(r.Context(), q.p, it.ID)
			row.Conflicts = len(c)
		}
		v.Items = append(v.Items, row)
	}
	v.Promotion, err = p.o.Memory.PendingPromotions(r.Context(), q.p, sp)
	if err != nil {
		return err
	}
	return p.show(q, "review.html", "Review queue · "+sc.Name, v)
}

// --- promotion requests -------------------------------------------------------------------

type promotionView struct {
	Req       memory.PromotionRequest
	Dest      *memory.Scope
	DestMaint bool
	Members   int // -1 when the viewer may not see membership
	Findings  []secrets.Finding
	Source    bool // the viewer can read the source memory
	Decided   bool
}

func (p *Portal) promotionPage(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	pr, err := p.o.Memory.GetPromotion(r.Context(), q.p, id)
	if err != nil {
		return err
	}
	v := promotionView{Req: pr, Members: -1, Source: pr.SourceMemoryID != nil, Decided: pr.Status != "pending"}
	if role, sc, err := p.o.Memory.EffectiveRoleOn(r.Context(), q.p, pr.DestinationScope.String()); err == nil {
		v.Dest = &sc
		v.DestMaint = role.AtLeast(memory.RoleMaintainer)
		if v.DestMaint {
			// Membership counts only for those entitled to list members.
			if ms, err := p.o.Memory.ListMembers(r.Context(), q.p, sc.ID.String()); err == nil {
				v.Members = len(ms)
			}
		}
	}
	s := pr.Snapshot
	v.Findings = secrets.Scan(map[string]string{"title": s.Title, "content": s.Content, "rationale": s.Rationale, "source_uri": s.SourceURI})
	return p.show(q, "promotion.html", "Promotion request", v)
}

func (p *Portal) promotionAccept(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	m, err := p.o.Memory.FinalizePromotion(r.Context(), q.p, id, strings.TrimSpace(r.PostForm.Get("note")))
	if err != nil {
		return err
	}
	return p.redirect(q, p.base+"/m/"+m.ID.String(), "accepted")
}

func (p *Portal) promotionReject(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	if _, err := p.o.Memory.RejectPromotion(r.Context(), q.p, id); err != nil {
		return err
	}
	return p.redirect(q, p.base+"/promotions/"+id.String(), "prejected")
}

func (p *Portal) promotionCancel(w http.ResponseWriter, r *http.Request, q *req) error {
	id, err := q.uuidParam("id")
	if err != nil {
		return err
	}
	if _, err := p.o.Memory.CancelPromotion(r.Context(), q.p, id); err != nil {
		return err
	}
	return p.redirect(q, p.base+"/promotions/"+id.String(), "cancelled")
}
