package portal

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

// --- home: the scope tree navigator ------------------------------------

type treeNode struct {
	memory.Scope
	Depth int // capped, for the indentation class
}

type homeData struct {
	Scopes []treeNode
}

// home lists the scopes the viewer can browse into: those with a
// membership and the descendants of cascading grants. Ancestors that are
// readable only by inheritance are not listed, and a scope the viewer
// cannot read is not revealed, including its name.
func (p *Portal) home(w http.ResponseWriter, r *http.Request, q *req) error {
	scopes, err := p.o.Memory.BrowsableScopes(r.Context(), q.p)
	if err != nil {
		return err
	}
	slices.SortFunc(scopes, func(a, b memory.Scope) int { return strings.Compare(a.Path, b.Path) })
	// Indent relative to the shallowest listed scope.
	base := 0
	if len(scopes) > 0 {
		base = scopes[0].Depth
		for _, s := range scopes {
			base = min(base, s.Depth)
		}
	}
	d := homeData{}
	for _, s := range scopes {
		d.Scopes = append(d.Scopes, treeNode{Scope: s, Depth: min(s.Depth-base, 8)})
	}
	return p.show(q, "home.html", "Scopes", d)
}

// --- scope page ---------------------------------------------------------

type scopeView struct {
	Page       memory.ScopePage
	Tab        string
	Filter     filterForm
	Names      map[uuid.UUID]string
	OwnCount   string
	InhCount   string
	Hidden     int  // inherited memories collapsed by overrides
	ShowOver   bool // the toggle
	Overridden map[uuid.UUID]*overrideNote
	Overrides  map[uuid.UUID][]overrideNote // overriding memory -> what it overrides
	CanReview  bool
	CanWrite   bool
	Kinds      []memory.Kind
	Query      string
	Limit      int
}

type overrideNote struct {
	Memory memory.SearchResult
	By     memory.SearchResult
}

type filterForm struct {
	Kind, Trust, Status, Since, Until string
}

func (f filterForm) values() url2 {
	v := url2{}
	for k, x := range map[string]string{"kind": f.Kind, "trust": f.Trust, "status": f.Status, "since": f.Since, "until": f.Until} {
		if x != "" {
			v[k] = x
		}
	}
	return v
}

type url2 map[string]string

func parseDate(s string, endOfDay bool) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	if endOfDay {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func (p *Portal) scopePage(w http.ResponseWriter, r *http.Request, q *req) error {
	sp := r.PathValue("path")
	f := filterForm{Kind: r.URL.Query().Get("kind"), Trust: r.URL.Query().Get("trust"), Status: r.URL.Query().Get("status"),
		Since: r.URL.Query().Get("since"), Until: r.URL.Query().Get("until")}
	bf := memory.BrowseFilter{Since: parseDate(f.Since, false), Until: parseDate(f.Until, true), Limit: memory.MaxLimit}
	if f.Kind != "" {
		bf.Kinds = []string{f.Kind}
	}
	if f.Trust != "" {
		bf.Trusts = []string{f.Trust}
	}
	if f.Status != "" {
		bf.Statuses = []string{f.Status}
	}
	pg, err := p.o.Memory.ScopePage(r.Context(), q.p, sp, bf)
	if err != nil {
		return err
	}
	tab := r.URL.Query().Get("tab")
	if tab != "inherited" {
		tab = "own"
	}
	v := scopeView{Page: pg, Tab: tab, Filter: f, Kinds: memory.Kinds, ShowOver: r.URL.Query().Get("overridden") == "1",
		Limit: memory.MaxLimit, CanReview: pg.Role.AtLeast(memory.RoleMaintainer), CanWrite: pg.Role.AtLeast(memory.RoleWriter)}
	var ids []uuid.UUID
	inhCount := 0
	for _, m := range pg.Own {
		ids = append(ids, m.CreatedBy)
	}
	for _, g := range pg.Inherited {
		for _, m := range g.Memories {
			inhCount++
			ids = append(ids, m.CreatedBy)
		}
	}
	// Overrides come from the service (they do not depend on which memories
	// the filters list): an inherited memory is collapsed whenever an
	// approved memory in a lower scope overrides it.
	v.Overridden = map[uuid.UUID]*overrideNote{}
	v.Overrides = map[uuid.UUID][]overrideNote{}
	for overridden, by := range pg.Overridden {
		m, ok1 := pg.Referenced[overridden]
		o, ok2 := pg.Referenced[by[0]]
		if !ok1 || !ok2 {
			continue
		}
		v.Overridden[overridden] = &overrideNote{Memory: m, By: o}
		v.Overrides[o.ID] = append(v.Overrides[o.ID], overrideNote{Memory: m, By: o})
	}
	if !v.ShowOver {
		var groups []memory.InheritedGroup
		for _, g := range pg.Inherited {
			kept := g.Memories[:0:0]
			for _, m := range g.Memories {
				if _, hidden := v.Overridden[m.ID]; hidden {
					v.Hidden++
					continue
				}
				kept = append(kept, m)
			}
			if len(kept) > 0 {
				groups = append(groups, memory.InheritedGroup{Scope: g.Scope, Memories: kept})
			}
		}
		v.Page.Inherited = groups
	}
	v.OwnCount = count(len(pg.Own), pg.OwnTruncated)
	shown := inhCount
	if !v.ShowOver {
		shown -= v.Hidden
	}
	v.InhCount = count(shown, pg.InheritedTruncated)
	v.Names = p.o.Memory.DisplayNames(r.Context(), q.p, ids)
	return p.show(q, "scope.html", pg.Scope.Name, v)
}

func count(n int, truncated bool) string {
	if truncated {
		return itoa(n) + "+"
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

// --- search -----------------------------------------------------------------

type searchView struct {
	Scope   string
	Query   string
	Mode    string
	Kind    string
	Resp    *search.Response
	Names   map[uuid.UUID]string
	Kinds   []memory.Kind
	Ran     bool
	Similar map[uuid.UUID][]memory.ConflictCandidate
	Role    memory.Role
	Overlap bool
}

func (p *Portal) searchPage(w http.ResponseWriter, r *http.Request, q *req) error {
	v := searchView{Scope: r.URL.Query().Get("scope"), Query: strings.TrimSpace(r.URL.Query().Get("q")),
		Mode: r.URL.Query().Get("mode"), Kind: r.URL.Query().Get("kind"), Kinds: memory.Kinds}
	if v.Scope == "" {
		scopes, err := p.o.Memory.BrowsableScopes(r.Context(), q.p)
		if err != nil {
			return err
		}
		if len(scopes) == 0 {
			return p.show(q, "search.html", "Search", v)
		}
		slices.SortFunc(scopes, func(a, b memory.Scope) int { return b.Depth - a.Depth })
		v.Scope = scopes[0].Path
	}
	if v.Query != "" {
		in := search.Input{Scope: v.Scope, Query: v.Query, Limit: 30, IncludeOverridden: r.URL.Query().Get("overridden") == "1",
			IncludeSuperseded: r.URL.Query().Get("superseded") == "1"}
		switch v.Mode {
		case "fulltext":
			in.Mode = search.ModeFullText
		case "vector":
			in.Mode = search.ModeVector
		default:
			v.Mode = "hybrid"
		}
		if v.Kind != "" {
			in.Kinds = []string{v.Kind}
		}
		resp, err := p.o.Search.Search(r.Context(), q.p, in)
		if err != nil {
			return err
		}
		v.Resp, v.Ran = &resp, true
		var ids []uuid.UUID
		for _, x := range resp.Results {
			ids = append(ids, x.CreatedBy)
			if x.ApprovedBy != nil {
				ids = append(ids, *x.ApprovedBy)
			}
		}
		v.Names = p.o.Memory.DisplayNames(r.Context(), q.p, ids)
		// Show a similar inherited memory beside a result from a lower scope.
		v.Similar = map[uuid.UUID][]memory.ConflictCandidate{}
		for i, x := range resp.Results {
			if i >= 10 || x.Inherited {
				continue
			}
			if cs, err := p.o.Memory.ConflictsFor(r.Context(), q.p, x.ID); err == nil {
				for _, c := range cs {
					if c.Memory.ID == x.ID {
						v.Similar[x.ID] = append(v.Similar[x.ID], c)
					}
				}
			}
		}
	}
	return p.show(q, "search.html", "Search", v)
}

// --- members ---------------------------------------------------------------

type membersView struct {
	Scope   memory.Scope
	Members []memory.Member
}

func (p *Portal) membersPage(w http.ResponseWriter, r *http.Request, q *req) error {
	sp := r.PathValue("path")
	ms, err := p.o.Memory.ListMembers(r.Context(), q.p, sp)
	if err != nil {
		return err
	}
	_, sc, err := p.o.Memory.EffectiveRoleOn(r.Context(), q.p, sp)
	if err != nil {
		return err
	}
	return p.show(q, "members.html", "Members of "+sc.Name, membersView{Scope: sc, Members: ms})
}
