package integration

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

type seen struct {
	origin    string
	inherited bool
}

func searchAll(t *testing.T, tr *tree, p memory.Principal, scope string) (map[string]seen, error) {
	t.Helper()
	res, err := tr.search.Search(context.Background(), p, search.Input{Scope: scope, Query: "guidance", Limit: 50})
	if err != nil {
		return nil, err
	}
	out := map[string]seen{}
	for _, r := range res.Results {
		out[r.Title] = seen{r.OriginScope, r.Inherited}
	}
	return out, nil
}

func keys(m map[string]seen) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestScopeInheritance(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	tr.create(t, tr.owner, "acme", "convention", "org guidance", "organization guidance applies everywhere")
	tr.create(t, tr.owner, "acme/platform", "convention", "team guidance", "team guidance for platform")
	tr.create(t, tr.owner, "acme/platform/payments", "decision", "payments guidance", "payments project guidance")
	tr.create(t, tr.owner, "acme/platform/billing", "decision", "billing guidance", "billing project guidance")

	projectReader := tr.member(t, "Project reader", "acme/platform/payments", memory.RoleReader, false)
	teamReader := tr.member(t, "Team reader", "acme/platform", memory.RoleReader, false)
	orgOnly := tr.member(t, "Org only", "acme", memory.RoleReader, false)
	orgCascade := tr.member(t, "Org cascade", "acme", memory.RoleReader, true)
	sibling := tr.member(t, "Billing reader", "acme/platform/billing", memory.RoleReader, false)

	t.Run("project reader sees project, team and org with origin and inherited flags", func(t *testing.T) {
		got, err := searchAll(t, tr, projectReader, "acme/platform/payments")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]seen{
			"payments guidance": {"acme/platform/payments", false},
			"team guidance":     {"acme/platform", true},
			"org guidance":      {"acme", true},
		}
		if len(got) != len(want) {
			t.Fatalf("got %v", keys(got))
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: got %+v want %+v", k, got[k], v)
			}
		}
		rec, err := tr.svc.Recent(ctx, projectReader, "acme/platform/payments", nil, 50)
		if err != nil || len(rec) != 3 {
			t.Fatalf("recent: %v %d", err, len(rec))
		}
	})

	t.Run("team reader never sees project memories", func(t *testing.T) {
		got, err := searchAll(t, tr, teamReader, "acme/platform")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(keys(got), ",") != "org guidance,team guidance" {
			t.Fatalf("got %v", keys(got))
		}
		if _, err := searchAll(t, tr, teamReader, "acme/platform/payments"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("non-cascading team grant must not open project: %v", err)
		}
	})

	t.Run("org-only member never sees team or project memories", func(t *testing.T) {
		got, err := searchAll(t, tr, orgOnly, "acme")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(keys(got), ",") != "org guidance" {
			t.Fatalf("got %v", keys(got))
		}
		for _, s := range []string{"acme/platform", "acme/platform/payments"} {
			if _, err := searchAll(t, tr, orgOnly, s); !errors.Is(err, memory.ErrNotFound) {
				t.Fatalf("non-cascading org grant opened %s: %v", s, err)
			}
		}
		browse, err := tr.svc.BrowsableScopes(ctx, orgOnly)
		if err != nil || len(browse) != 1 || browse[0].Path != "acme" {
			t.Fatalf("browsable: %v %+v", err, browse)
		}
	})

	t.Run("cascading org grant confers access to descendants", func(t *testing.T) {
		got, err := searchAll(t, tr, orgCascade, "acme/platform/payments")
		if err != nil || len(got) != 3 {
			t.Fatalf("cascade into project: %v %v", err, keys(got))
		}
		browse, err := tr.svc.BrowsableScopes(ctx, orgCascade)
		if err != nil || len(browse) != 4 {
			t.Fatalf("browsable: %v %+v", err, browse)
		}
		// Even with cascade, a query at org level does not include descendants.
		got, _ = searchAll(t, tr, orgCascade, "acme")
		if strings.Join(keys(got), ",") != "org guidance" {
			t.Fatalf("org-level view leaked descendants: %v", keys(got))
		}
	})

	t.Run("sibling project never sees another project's memories", func(t *testing.T) {
		got, err := searchAll(t, tr, sibling, "acme/platform/billing")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got["payments guidance"]; ok {
			t.Fatal("billing reader saw payments memory")
		}
		if _, err := searchAll(t, tr, sibling, "acme/platform/payments"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("sibling opened payments: %v", err)
		}
	})

	t.Run("get by id respects the same rules", func(t *testing.T) {
		paymentsID := tr.find(t, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "payments"})[0].ID
		orgID := tr.find(t, tr.owner, search.Input{Scope: "acme", Query: "organization"})[0].ID
		for _, p := range []memory.Principal{teamReader, orgOnly, sibling} {
			if _, err := tr.svc.Get(ctx, p, paymentsID, ""); !errors.Is(err, memory.ErrNotFound) {
				t.Errorf("%s fetched a project memory: %v", p.DisplayName, err)
			}
			if _, err := tr.svc.History(ctx, p, paymentsID); !errors.Is(err, memory.ErrNotFound) {
				t.Errorf("%s read project history: %v", p.DisplayName, err)
			}
		}
		m, err := tr.svc.Get(ctx, projectReader, orgID, "acme/platform/payments")
		if err != nil || !m.Inherited || m.OriginScope != "acme" {
			t.Fatalf("inherited get: %v %+v", err, m)
		}
		// Viewing from a scope the memory is not visible from is not found.
		if _, err := tr.svc.Get(ctx, tr.owner, paymentsID, "acme/platform/billing"); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("payments memory visible from billing: %v", err)
		}
	})
}

func TestInheritedMemoriesReadOnlyFromDescendants(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	org := tr.create(t, tr.owner, "acme", "convention", "Org rule", "use structured logging")
	projectWriter := tr.member(t, "Project writer", "acme/platform/payments", memory.RoleWriter, false)

	// A project writer can read the inherited memory but not change it.
	if _, err := tr.svc.Update(ctx, projectWriter, memory.UpdateInput{ID: org.ID, ExpectedRevision: 1, Title: strp("hijacked")}); !errors.Is(err, memory.ErrInherited) {
		t.Fatalf("update inherited: %v", err)
	}
	if _, _, err := tr.svc.Supersede(ctx, projectWriter, memory.SupersedeInput{ID: org.ID, ExpectedRevision: 1,
		Replacement: memory.CreateInput{Title: "x", Content: "y"}}); !errors.Is(err, memory.ErrInherited) {
		t.Fatalf("supersede inherited: %v", err)
	}
	// Even an org maintainer cannot change it while working in a descendant scope.
	if _, err := tr.svc.Update(ctx, tr.owner, memory.UpdateInput{ID: org.ID, ExpectedRevision: 1, Scope: "acme/platform/payments", Title: strp("x")}); !errors.Is(err, memory.ErrInherited) {
		t.Fatalf("update from descendant scope: %v", err)
	}
	if _, err := tr.svc.Update(ctx, tr.owner, memory.UpdateInput{ID: org.ID, ExpectedRevision: 1, Scope: "acme", Title: strp("Org rule v2")}); err != nil {
		t.Fatalf("update from owning scope: %v", err)
	}
	// The working-scope check must not reveal whether other scopes exist.
	own := tr.create(t, projectWriter, "acme/platform/payments", "fact", "Own fact", "payments fact")
	for _, ref := range []string{"acme/platform/billing", "otherorg/secret", "acme/platform/payments/missing"} {
		if _, err := tr.svc.Update(ctx, projectWriter, memory.UpdateInput{ID: own.ID, ExpectedRevision: 1, Scope: ref, Title: strp("x")}); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("working scope %q: want not found, got %v", ref, err)
		}
	}
	// Invisible memories are not found, whatever scope is given.
	billing := tr.create(t, tr.owner, "acme/platform/billing", "fact", "Billing fact", "billing")
	if _, err := tr.svc.Update(ctx, projectWriter, memory.UpdateInput{ID: billing.ID, ExpectedRevision: 1, Title: strp("x")}); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("update invisible memory: %v", err)
	}

	// A project writer cannot create in an ancestor scope.
	if _, err := tr.svc.Create(ctx, projectWriter, memory.CreateInput{Scope: "acme", Kind: "fact", Title: "x", Content: "y"}); !errors.Is(err, memory.ErrForbidden) {
		t.Fatalf("create in ancestor: %v", err)
	}
	// Readers cannot write.
	reader := tr.member(t, "Reader", "acme/platform/payments", memory.RoleReader, false)
	if _, err := tr.svc.Create(ctx, reader, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "x", Content: "y"}); !errors.Is(err, memory.ErrForbidden) {
		t.Fatalf("reader create: %v", err)
	}
}

func TestOverridesAreModeledButDoNotHide(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	org := tr.create(t, tr.owner, "acme", "convention", "guidance on logging", "use JSON logs")
	sib := tr.create(t, tr.owner, "acme/platform/billing", "convention", "billing guidance", "billing rule")

	m, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "convention",
		Title: "payments guidance on logging", Content: "payments uses logfmt", Overrides: []uuid.UUID{org.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Overrides) != 1 || m.Overrides[0] != org.ID || m.OverrideStatus != "proposed" {
		t.Fatalf("override not recorded: %+v", m)
	}
	got, _ := searchAll(t, tr, tr.owner, "acme/platform/payments")
	if _, ok := got["guidance on logging"]; !ok {
		t.Fatal("drafted override hid inherited guidance")
	}
	// Only inherited memories can be overridden.
	var ve *memory.ValidationError
	if _, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact",
		Title: "x", Content: "y", Overrides: []uuid.UUID{sib.ID}}); !errors.As(err, &ve) {
		t.Fatalf("override of sibling memory: %v", err)
	}
	if _, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme", Kind: "fact",
		Title: "x", Content: "y", Overrides: []uuid.UUID{org.ID}}); !errors.As(err, &ve) {
		t.Fatalf("self-scope override: %v", err)
	}
}

func TestTreeRules(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	var ve *memory.ValidationError

	// Default max depth is 5 levels: acme/platform/payments is level 3.
	if _, err := tr.svc.CreateScope(ctx, uuid.Nil, "acme/platform/payments", "project", "api", ""); err != nil {
		t.Fatalf("level 4: %v", err)
	}
	if _, err := tr.svc.CreateScope(ctx, uuid.Nil, "acme/platform/payments/api", "project", "v2", ""); err != nil {
		t.Fatalf("level 5: %v", err)
	}
	if _, err := tr.svc.CreateScope(ctx, uuid.Nil, "acme/platform/payments/api/v2", "project", "too-deep", ""); !errors.As(err, &ve) {
		t.Fatalf("level 6 should be rejected: %v", err)
	}

	// Levels must be in the organization's allowed set until configured.
	if _, err := tr.svc.CreateScope(ctx, uuid.Nil, "acme", "division", "emea", ""); !errors.As(err, &ve) {
		t.Fatalf("unknown level: %v", err)
	}
	if _, err := tr.svc.ConfigureOrg(ctx, "acme", []string{"organization", "division", "team", "project"}, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.svc.CreateScope(ctx, uuid.Nil, "acme", "division", "emea", ""); err != nil {
		t.Fatalf("admin-defined level: %v", err)
	}
	if _, err := tr.svc.ConfigureOrg(ctx, "acme", []string{"organization", "team", "project"}, 6); !errors.As(err, &ve) {
		t.Fatalf("removing a used level should fail: %v", err)
	}
	if _, err := tr.svc.ConfigureOrg(ctx, "acme", []string{"organization", "division", "team", "project"}, 3); !errors.As(err, &ve) {
		t.Fatalf("lowering max depth below existing scopes should fail: %v", err)
	}

	// Single parent and no moves: the parent cannot be changed after creation.
	if _, err := tr.pool.Exec(ctx, "UPDATE scopes SET parent_id = $1 WHERE id = $2", tr.org.ID, tr.payments.ID); err == nil {
		t.Fatal("moving a scope should be rejected")
	}
	// Only the organization root may lack a parent, and a root is its own org.
	if _, err := tr.pool.Exec(ctx, `INSERT INTO scopes (org_id, parent_id, level, slug, name, depth, path)
		VALUES ($1, NULL, 'team', 'orphan', 'orphan', 1, 'acme/orphan')`, tr.org.ID); err == nil {
		t.Fatal("parentless non-root scope should be rejected")
	}
	// Duplicate slug under the same parent.
	if _, err := tr.svc.CreateScope(ctx, uuid.Nil, "acme", "team", "platform", ""); !errors.As(err, &ve) {
		t.Fatalf("duplicate slug: %v", err)
	}
	// Scopes cannot be deleted.
	if _, err := tr.pool.Exec(ctx, "DELETE FROM scopes WHERE id = $1", tr.billing.ID); err == nil {
		t.Fatal("deleting a scope should be rejected")
	}
}
