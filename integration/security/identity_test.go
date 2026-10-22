package security

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// TestLinkFromPortal: Connect SSO requires a portal session and a fresh
// upstream login, never moves an identity, and never changes ownership.
func TestLinkFromPortal(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ids := auth.NewIdentities(w.db.App)
	now := time.Now()
	id := auth.ExternalIdentity{Issuer: "https://idp.example.com", Subject: "owner-sub", AuthTime: now.Add(-time.Minute)}
	if err := ids.LinkFromPortal(ctx, w.owner, id, now); !is(err, memory.ErrBrowserReviewRequired) {
		t.Fatalf("link over a bearer token: %v", err)
	}
	stale := id
	stale.AuthTime = now.Add(-time.Hour)
	if err := ids.LinkFromPortal(ctx, portal(w.owner), stale, now); !is(err, auth.ErrStaleLogin) {
		t.Fatalf("link with an old upstream login: %v", err)
	}
	if err := ids.LinkFromPortal(ctx, portal(w.owner), auth.ExternalIdentity{Issuer: id.Issuer, Subject: id.Subject}, now); !is(err, auth.ErrStaleLogin) {
		t.Fatalf("link without auth_time: %v", err)
	}
	if err := ids.LinkFromPortal(ctx, portal(w.owner), id, now); err != nil {
		t.Fatal(err)
	}
	if err := ids.LinkFromPortal(ctx, portal(w.paymentsMaint), id, now); !is(err, auth.ErrIdentityLinked) {
		t.Fatalf("identity moved to another principal: %v", err)
	}
	var owner string
	_ = w.db.Admin.QueryRow(ctx, "SELECT owner_principal_id::text FROM org_settings WHERE org_id = $1", w.scopes["acme"].ID).Scan(&owner)
	if owner != w.owner.ID.String() {
		t.Fatal("linking changed ownership")
	}
}

// TestIdPGroupRoleSync: mapped groups grant roles at sign-in and lose them
// when the group disappears; manual grants are untouched; one principal
// cannot sync grants onto another.
func TestIdPGroupRoleSync(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	const iss = "https://idp.example.com"
	q := db.New(w.db.Admin)
	for _, m := range []db.InsertIdPGroupMappingParams{
		{Issuer: iss, GroupName: "payments-devs", ScopeID: w.scopes["acme/platform/payments"].ID, Role: "writer"},
		{Issuer: iss, GroupName: "billing-leads", ScopeID: w.scopes["acme/platform/billing"].ID, Role: "maintainer"},
		{Issuer: "https://other-idp.example.com", GroupName: "payments-devs", ScopeID: w.scopes["acme/data"].ID, Role: "maintainer", Cascade: true},
	} {
		if err := q.InsertIdPGroupMapping(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	ids := auth.NewIdentities(w.db.App)
	bob := w.principal("Bob", "human")
	w.grant("acme/platform/billing", bob, memory.RoleReader, false) // manual
	sync := func(groups ...string) {
		t.Helper()
		if err := ids.SyncGroupRoles(ctx, bob.ID, auth.ExternalIdentity{Issuer: iss, Subject: "bob", Groups: groups}); err != nil {
			t.Fatal(err)
		}
	}
	role := func(scope string) string {
		var r string
		_ = w.db.Admin.QueryRow(ctx, "SELECT role || '/' || source FROM scope_members WHERE principal_id = $1 AND scope_id = $2",
			bob.ID, w.scopes[scope].ID).Scan(&r)
		return r
	}
	sync("payments-devs", "billing-leads", "unmapped")
	if role("acme/platform/payments") != "writer/idp" || role("acme/platform/billing") != "reader/manual" || role("acme/data") != "" {
		t.Fatalf("after sync: payments %q billing %q data %q", role("acme/platform/payments"), role("acme/platform/billing"), role("acme/data"))
	}
	if _, err := w.svc.Create(ctx, bob, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "t", Content: "c"}); err != nil {
		t.Fatalf("synced writer cannot write: %v", err)
	}
	sync() // left every group
	if role("acme/platform/payments") != "" || role("acme/platform/billing") != "reader/manual" {
		t.Fatalf("after leaving groups: payments %q billing %q", role("acme/platform/payments"), role("acme/platform/billing"))
	}
	// Syncing grants onto someone else is refused by the definer function.
	err := w.asApp(w.paymentsWriter, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT tm_sync_idp_grants($1, $2, ARRAY['payments-devs'])", bob.ID, iss)
		return err
	})
	if err == nil {
		t.Fatal("principal synced grants for another principal")
	}
}

// TestIdPGroupSyncIsPerIssuer: a principal linked to two providers that
// both sync group roles keeps each provider's grants when signing in
// through the other. When both map the same scope, the grant stays owned
// by the provider that created it until that provider stops justifying it.
func TestIdPGroupSyncIsPerIssuer(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	const issA, issB = "https://idp-a.example.com", "https://idp-b.example.com"
	q := db.New(w.db.Admin)
	for _, m := range []db.InsertIdPGroupMappingParams{
		{Issuer: issA, GroupName: "payments-devs", ScopeID: w.scopes["acme/platform/payments"].ID, Role: "writer"},
		{Issuer: issB, GroupName: "data-team", ScopeID: w.scopes["acme/data"].ID, Role: "writer", Cascade: true},
		// Both providers map billing, with different roles.
		{Issuer: issA, GroupName: "billing", ScopeID: w.scopes["acme/platform/billing"].ID, Role: "reader"},
		{Issuer: issB, GroupName: "billing", ScopeID: w.scopes["acme/platform/billing"].ID, Role: "maintainer"},
	} {
		if err := q.InsertIdPGroupMapping(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	ids := auth.NewIdentities(w.db.App)
	carol := w.principal("Carol", "human")
	sync := func(issuer string, groups ...string) {
		t.Helper()
		if err := ids.SyncGroupRoles(ctx, carol.ID, auth.ExternalIdentity{Issuer: issuer, Subject: "carol", Groups: groups}); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(scope string) string {
		var g string
		_ = w.db.Admin.QueryRow(ctx, "SELECT role || '/' || coalesce(idp_issuer, 'manual') FROM scope_members WHERE principal_id = $1 AND scope_id = $2",
			carol.ID, w.scopes[scope].ID).Scan(&g)
		return g
	}
	check := func(step string, want map[string]string) {
		t.Helper()
		for scope, g := range want {
			if got := grant(scope); got != g {
				t.Errorf("%s: %s grant %q, want %q", step, scope, got, g)
			}
		}
	}

	sync(issA, "payments-devs", "billing")
	sync(issB, "data-team", "billing")
	check("after both sign-ins", map[string]string{
		"acme/platform/payments": "writer/" + issA,
		"acme/data":              "writer/" + issB,
		// A created it first; B's stronger mapping does not take it over.
		"acme/platform/billing": "reader/" + issA,
	})

	// Signing in through A with no groups removes only A's grants.
	sync(issA)
	check("A with no groups", map[string]string{
		"acme/platform/payments": "",
		"acme/data":              "writer/" + issB,
		"acme/platform/billing":  "",
	})
	// B's next sign-in now applies its billing mapping.
	sync(issB, "data-team", "billing")
	check("B again", map[string]string{"acme/platform/billing": "maintainer/" + issB, "acme/data": "writer/" + issB})

	// A sign-in through A that still maps billing leaves B's grant alone.
	sync(issA, "billing")
	check("A maps billing too", map[string]string{"acme/platform/billing": "maintainer/" + issB})

	// A manual grant is never changed or removed by either provider, and
	// clears the issuer when it replaces an IdP grant.
	if err := w.admin.AddMember(ctx, uuid.Nil, "acme/data", carol.ID, memory.RoleReader, false); err != nil {
		t.Fatal(err)
	}
	sync(issB)
	check("manual grant survives", map[string]string{"acme/data": "reader/manual", "acme/platform/billing": ""})
	if _, err := w.db.Admin.Exec(ctx, "UPDATE scope_members SET idp_issuer = $1 WHERE principal_id = $2 AND source = 'manual'", issA, carol.ID); err == nil {
		t.Fatal("a manual grant accepted an issuer")
	}
}
