package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/team-memory/team-memory/internal/auth"
)

func TestInitAndTokens(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	res, err := auth.Init(ctx, pool, auth.InitOptions{OrgSlug: "acme", OrgName: "Acme", ProjectSlug: "default", OwnerName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProjectPath != "acme/default" {
		t.Fatalf("project path %q", res.ProjectPath)
	}

	// Rerunning init never replaces the owner.
	if _, err := auth.Init(ctx, pool, auth.InitOptions{OrgSlug: "evil", OrgName: "Evil", ProjectSlug: "x", OwnerName: "Attacker"}); !errors.Is(err, auth.ErrAlreadyInitialized) {
		t.Fatalf("second init: %v", err)
	}
	var owner string
	if err := pool.QueryRow(ctx, "SELECT owner_principal_id::text FROM org_settings").Scan(&owner); err != nil || owner != res.OwnerID.String() {
		t.Fatalf("owner changed: %v %s", err, owner)
	}

	pats := auth.NewPATs(pool)
	p, _, err := pats.Authenticate(ctx, res.Token.Secret)
	if err != nil || p.ID != res.OwnerID || p.AuthMethod != "pat" {
		t.Fatalf("authenticate: %v %+v", err, p)
	}

	// Only the hash is stored.
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM personal_access_tokens WHERE token_prefix = $1 OR encode(token_hash, 'escape') = $1", res.Token.Secret).Scan(&n); err != nil || n != 0 {
		t.Fatalf("token stored in plain text: %v %d", err, n)
	}

	for name, cred := range map[string]string{
		"empty":     "",
		"garbage":   "not-a-token",
		"unknown":   auth.TokenPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"truncated": res.Token.Secret[:len(res.Token.Secret)-1],
	} {
		if _, _, err := pats.Authenticate(ctx, cred); !errors.Is(err, auth.ErrInvalidCredential) {
			t.Errorf("%s credential: %v", name, err)
		}
	}

	// Expired tokens are rejected.
	short, err := pats.Issue(ctx, res.OwnerID, "short", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	later := pats.WithClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	if _, _, err := later.Authenticate(ctx, short.Secret); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("expired token accepted: %v", err)
	}

	// Revoked tokens are rejected.
	if err := pats.Revoke(ctx, res.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pats.Authenticate(ctx, res.Token.Secret); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	if _, err := pats.Issue(ctx, res.OwnerID, "too-long", 2*auth.MaxTokenTTL); err == nil {
		t.Fatal("over-long token lifetime accepted")
	}
}
