package oauth

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/memory"
)

// leeway tolerates clock differences between replicas for nbf. Expiry is
// strict.
const leeway = 30 * time.Second

// tokenUseAccess is the token_use claim of Team Memory access tokens. ID
// tokens share the issuer, keys and audience, so the verifier requires it.
const tokenUseAccess = "access"

// AccessTokens verifies Team Memory access tokens. A token is accepted only
// if it is signed by a configured key, its issuer is exactly this server,
// its audience contains the canonical MCP resource URI, it has not expired,
// it carries the memory scope, and its subject is an enabled principal.
// Tokens from upstream providers fail the key, issuer and audience checks.
type AccessTokens struct {
	issuer     string
	resource   string
	keys       *Keyring
	identities *auth.Identities
	now        func() time.Time
}

// NewAccessTokens builds a verifier. It is exported for tests and for the
// server wiring; most callers use Server.Verifier.
func NewAccessTokens(issuer, resource string, keys *Keyring, identities *auth.Identities) *AccessTokens {
	return &AccessTokens{issuer: issuer, resource: resource, keys: keys, identities: identities, now: time.Now}
}

// WithClock returns a copy using now as the clock (for tests).
func (v *AccessTokens) WithClock(now func() time.Time) *AccessTokens {
	cp := *v
	cp.now = now
	return &cp
}

type accessClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	Expiry    *int64   `json:"exp"`
	NotBefore *int64   `json:"nbf"`
	Scope     string   `json:"scope"`
	ClientID  string   `json:"client_id"`
	TokenUse  string   `json:"token_use"`
}

// audience accepts the string and array forms of "aud".
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = []string{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	*a = l
	return nil
}

// Claims parses and checks a token without consulting the database.
func (v *AccessTokens) claims(raw string) (accessClaims, error) {
	var c accessClaims
	if len(raw) > 8192 || strings.Count(raw, ".") != 2 {
		return c, auth.ErrInvalidCredential
	}
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256, jose.RS256})
	if err != nil || len(jws.Signatures) != 1 {
		return c, auth.ErrInvalidCredential
	}
	h := jws.Signatures[0].Protected
	key, alg, ok := v.keys.verificationKey(h.KeyID)
	if !ok || h.Algorithm != string(alg) {
		return c, auth.ErrInvalidCredential
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return c, auth.ErrInvalidCredential
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, auth.ErrInvalidCredential
	}
	now := v.now()
	switch {
	case c.Issuer != v.issuer:
		return c, auth.ErrInvalidCredential
	case !slices.Contains(c.Audience, v.resource):
		return c, auth.ErrInvalidCredential
	case c.Expiry == nil || !now.Before(time.Unix(*c.Expiry, 0)):
		return c, auth.ErrInvalidCredential
	case c.NotBefore != nil && now.Add(leeway).Before(time.Unix(*c.NotBefore, 0)):
		return c, auth.ErrInvalidCredential
	case c.TokenUse != tokenUseAccess:
		// An ID token or any other JWT signed by our keys.
		return c, auth.ErrInvalidCredential
	}
	return c, nil
}

// Authenticate implements auth.Authenticator.
func (v *AccessTokens) Authenticate(ctx context.Context, raw string) (memory.Principal, time.Time, error) {
	c, err := v.claims(raw)
	if err != nil {
		return memory.Principal{}, time.Time{}, err
	}
	if !slices.Contains(strings.Fields(c.Scope), ScopeMemory) {
		return memory.Principal{}, time.Time{}, auth.ErrInsufficientScope
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return memory.Principal{}, time.Time{}, auth.ErrInvalidCredential
	}
	p, err := v.identities.Principal(ctx, id)
	if err != nil {
		return memory.Principal{}, time.Time{}, err
	}
	p.AuthMethod = "oauth"
	return p, time.Unix(*c.Expiry, 0), nil
}
