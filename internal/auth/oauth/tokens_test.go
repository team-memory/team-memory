package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/oidc"

	"github.com/team-memory/team-memory/internal/auth"
)

const (
	testIssuer   = "https://memory.example.com"
	testResource = testIssuer + "/mcp"
)

// writeKey writes a P-256 key as <dir>/<kid>.pem and returns it.
func writeKey(t *testing.T, dir, kid string) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, kid+".pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, key any, alg jose.SignatureAlgorithm, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	jws, err := signer.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss": testIssuer, "sub": "6f1c1a8e-2a55-4d63-9c55-8b1b1b0a0c11", "aud": []string{testResource},
		"exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(), "nbf": now.Unix(),
		"scope": "memory", "client_id": "tmc_test", "token_use": "access",
	}
}

func TestAccessTokenValidation(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "k1")
	kr, err := LoadKeyring(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	v := NewAccessTokens(testIssuer, testResource, kr, nil).WithClock(func() time.Time { return now })

	if _, err := v.claims(sign(t, key, jose.ES256, "k1", validClaims(now))); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	// "aud" as a single string is accepted too.
	single := validClaims(now)
	single["aud"] = testResource
	if _, err := v.claims(sign(t, key, jose.ES256, "k1", single)); err != nil {
		t.Fatalf("string audience rejected: %v", err)
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mutate := func(f func(map[string]any)) map[string]any {
		c := validClaims(now)
		f(c)
		return c
	}
	cases := map[string]string{
		"wrong audience":           sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["aud"] = []string{"https://other.example.com/mcp"} })),
		"audience is the client":   sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["aud"] = "tmc_test" })),
		"audience is the issuer":   sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["aud"] = testIssuer })),
		"no audience":              sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { delete(c, "aud") })),
		"expired":                  sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() })),
		"no expiry":                sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { delete(c, "exp") })),
		"not yet valid":            sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() })),
		"wrong issuer":             sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["iss"] = "https://idp.example.com/realms/acme" })),
		"issuer with slash":        sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["iss"] = testIssuer + "/" })),
		"unknown key id":           sign(t, key, jose.ES256, "k2", validClaims(now)),
		"forged with another key":  sign(t, other, jose.ES256, "k1", validClaims(now)),
		"garbage":                  "not.a.token",
		"empty":                    "",
		"unsigned (alg none)":      unsigned(validClaims(now)),
		"signature stripped":       strings.Join(strings.Split(sign(t, key, jose.ES256, "k1", validClaims(now)), ".")[:2], ".") + ".",
		"upstream id token shaped": sign(t, other, jose.ES256, "upstream", map[string]any{"iss": "https://idp.example.com", "sub": "alice", "aud": "team-memory", "exp": now.Add(time.Hour).Unix()}),
		// Our own ID token: same issuer, key and audience, but no token_use.
		"team memory id token": sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { delete(c, "token_use"); delete(c, "scope"); c["azp"] = "tmc_test" })),
		"wrong token use":      sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["token_use"] = "id" })),
	}
	for name, tok := range cases {
		if _, err := v.claims(tok); !errors.Is(err, auth.ErrInvalidCredential) {
			t.Errorf("%s: accepted (err=%v)", name, err)
		}
	}

	// A valid token without the memory scope is a scope error, not a
	// principal. (Authenticate stops before touching the database.)
	noScope := sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["scope"] = "openid" }))
	if _, _, err := v.Authenticate(context.Background(), noScope); !errors.Is(err, auth.ErrInsufficientScope) {
		t.Fatalf("token without memory scope: %v", err)
	}
	badSub := sign(t, key, jose.ES256, "k1", mutate(func(c map[string]any) { c["sub"] = "alice" }))
	if _, _, err := v.Authenticate(context.Background(), badSub); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("non-principal subject: %v", err)
	}
}

func unsigned(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + "."
}

func TestKeyRotation(t *testing.T) {
	dir := t.TempDir()
	old := writeKey(t, dir, "2026-09")
	now := time.Now()
	oldTok := sign(t, old, jose.ES256, "2026-09", validClaims(now))

	newKey := writeKey(t, dir, "2026-10")
	if _, err := LoadKeyring(dir, ""); err == nil {
		t.Fatal("two keys without active_signing_key accepted")
	}
	kr, err := LoadKeyring(dir, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if kr.Active().ID() != "2026-10" || len(kr.Public()) != 2 {
		t.Fatalf("active %s, published %d", kr.Active().ID(), len(kr.Public()))
	}
	v := NewAccessTokens(testIssuer, testResource, kr, nil).WithClock(func() time.Time { return now })
	for name, tok := range map[string]string{"old": oldTok, "new": sign(t, newKey, jose.ES256, "2026-10", validClaims(now))} {
		if _, err := v.claims(tok); err != nil {
			t.Errorf("%s key token rejected during rotation: %v", name, err)
		}
	}
	// After the old file is removed, its tokens stop verifying.
	_ = os.Remove(filepath.Join(dir, "2026-09.pem"))
	kr, _ = LoadKeyring(dir, "2026-10")
	v = NewAccessTokens(testIssuer, testResource, kr, nil).WithClock(func() time.Time { return now })
	if _, err := v.claims(oldTok); err == nil {
		t.Fatal("token of a removed key accepted")
	}
	if _, err := LoadKeyring(dir, "missing"); err == nil {
		t.Fatal("missing active key accepted")
	}
	if _, err := LoadKeyring(t.TempDir(), ""); err == nil {
		t.Fatal("empty key directory accepted")
	}
}

func TestSameResource(t *testing.T) {
	for _, c := range []struct {
		a    string
		want bool
	}{
		{testResource, true},
		{testResource + "/", true},
		{"HTTPS://Memory.Example.COM/mcp", true},
		{"https://memory.example.com:443/mcp", true},
		{testIssuer, false},
		{testIssuer + "/", false},
		{"https://memory.example.com/mcp/other", false},
		{"https://memory.example.com/mcp?x=1", false},
		{"https://memory.example.com/mcp#f", false},
		{"http://memory.example.com/mcp", false},
		{"https://memory.example.com.evil.test/mcp", false},
		{"", false},
	} {
		if got := sameResource(c.a, testResource); got != c.want {
			t.Errorf("sameResource(%q) = %v", c.a, got)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	base := func() Config {
		return Config{
			Issuer: testIssuer, SigningKeysDir: "/keys",
			Registration: Registration{Dynamic: true},
			Providers:    []Provider{{ID: "corp", Issuer: "https://idp.example.com", ClientID: "tm", ClientSecretFile: "/s"}},
		}
	}
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if time.Duration(c.AccessTokenTTL) != defaultAccessTTL || c.Providers[0].Scopes[0] != "openid" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	for name, f := range map[string]func(*Config){
		"http issuer":          func(c *Config) { c.Issuer = "http://memory.example.com" },
		"http on non-loopback": func(c *Config) { c.Issuer = "http://memory.example.com"; c.AllowInsecureHTTP = true },
		"issuer with path":     func(c *Config) { c.Issuer = testIssuer + "/tm" },
		"no keys":              func(c *Config) { c.SigningKeysDir = "" },
		"no providers":         func(c *Config) { c.Providers = nil },
		"no registration":      func(c *Config) { c.Registration = Registration{} },
		"no secret file":       func(c *Config) { c.Providers[0].ClientSecretFile = "" },
		"bad provider id":      func(c *Config) { c.Providers[0].ID = "Corp IdP" },
		"duplicate provider":   func(c *Config) { c.Providers = append(c.Providers, c.Providers[0]) },
		"http provider":        func(c *Config) { c.Providers[0].Issuer = "http://idp.example.com" },
		"scopes without openid": func(c *Config) {
			c.Providers[0].Scopes = []string{"email"}
		},
		"access ttl too long": func(c *Config) { c.AccessTokenTTL = Duration(2 * time.Hour) },
		"bad domain":          func(c *Config) { c.Providers[0].AllowedEmailDomains = []string{"@example.com"} },
	} {
		c := base()
		f(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	local := base()
	local.Issuer, local.AllowInsecureHTTP = "http://localhost:8080", true
	local.Providers[0].Issuer = "http://localhost:8180/realms/dev"
	if err := local.Validate(); err != nil {
		t.Fatalf("loopback dev config: %v", err)
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sso.json")
	_ = os.WriteFile(p, []byte(`{"signing_keys_dir":"/k","client_secret":"inline","providers":[]}`), 0o600)
	if _, err := LoadConfig(p, testIssuer); err == nil || !strings.Contains(err.Error(), "client_secret") {
		t.Fatalf("unknown field accepted: %v", err)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), testIssuer); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestRefreshRotationAndReuse(t *testing.T) {
	cfg := &Config{AccessTokenTTL: Duration(time.Minute), RefreshTokenTTL: Duration(time.Hour), Issuer: testIssuer}
	now := time.Now()
	clock := func() time.Time { return now }
	s := newStore(cfg, nil, nil, newMemState(clock))
	s.now = clock
	ctx := context.Background()
	a := &authRequest{clientID: "c", subject: "p", resource: testResource, scopes: []string{"memory", "offline_access"}}
	_, rt1, _, err := s.CreateAccessAndRefreshTokens(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.TokenRequestByRefreshToken(ctx, rt1)
	if err != nil || req.GetAudience()[0] != testResource {
		t.Fatalf("refresh lookup: %v", err)
	}
	_, rt2, _, err := s.CreateAccessAndRefreshTokens(ctx, req, rt1)
	if err != nil || rt2 == rt1 {
		t.Fatalf("rotation: %v", err)
	}
	// Within the grace window a repeat of rt1 is a client race: it fails,
	// but the family (rt2) survives.
	if _, err := s.TokenRequestByRefreshToken(ctx, rt1); err == nil {
		t.Fatal("rotated refresh token accepted")
	}
	if _, err := s.TokenRequestByRefreshToken(ctx, rt2); err != nil {
		t.Fatalf("family revoked inside the grace window: %v", err)
	}
	// After the window, replaying rt1 is theft: the family is revoked.
	now = now.Add(refreshGrace + time.Second)
	if _, err := s.TokenRequestByRefreshToken(ctx, rt1); err == nil {
		t.Fatal("rotated refresh token accepted")
	}
	if _, err := s.TokenRequestByRefreshToken(ctx, rt2); err == nil {
		t.Fatal("family survived refresh token reuse")
	}
}

func TestCreateAuthRequestRequiresPKCE(t *testing.T) {
	s := newStore(&Config{Issuer: testIssuer}, nil, nil, newMemState(time.Now))
	ctx := context.Background()
	for _, method := range []string{"", "plain"} {
		r := &oidcAuthRequest{CodeChallenge: "x", CodeChallengeMethod: method}
		if _, err := s.CreateAuthRequest(ctx, r.req(), ""); err == nil {
			t.Errorf("method %q accepted", method)
		}
	}
	r := &oidcAuthRequest{CodeChallenge: "x", CodeChallengeMethod: "S256"}
	req := r.req()
	req.Scopes = []string{"openid"}
	a, err := s.CreateAuthRequest(ctx, req, "")
	if err != nil || a.GetAudience()[0] != testResource {
		t.Fatalf("S256 request: %v %v", err, a)
	}
	if !slices.Contains(a.GetScopes(), ScopeMemory) {
		t.Fatalf("memory scope not added: %v", a.GetScopes())
	}
}

func TestPublicIP(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.100.100.100": false, "::1": false, "fd00::1": false,
		"fe80::1": false, "0.0.0.0": false, "224.0.0.1": false,
		"::ffff:10.0.0.1": false, "::ffff:93.184.215.14": true, "0.1.2.3": false,
		"198.18.0.1": false, "240.0.0.1": false, "192.0.0.8": false,
		"64:ff9b::a00:1": false, "64:ff9b::a9fe:a9fe": false, "2002:a00:1::": false, "2001:0:a00:1::": false,
	} {
		if got := publicIP(parseIP(addr)); got != want {
			t.Errorf("publicIP(%s) = %v", addr, got)
		}
	}
}

func TestCacheTTL(t *testing.T) {
	for h, want := range map[string]time.Duration{
		"": cimdDefaultTTL, "max-age=600": 10 * time.Minute, "max-age=1": time.Second,
		"public, max-age=999999": cimdMaxTTL, "no-store": 0,
		"max-age=600, no-store": 0, "no-store, max-age=600": 0,
		"max-age=600, no-cache": 0, "max-age=0": 0,
		"max-age=\"600\"":              10 * time.Minute,
		"max-age=18446744073709551615": cimdMaxTTL,
	} {
		if got := cacheTTL(h); got != want {
			t.Errorf("cacheTTL(%q) = %s", h, got)
		}
	}
}

func TestRegistrationBodyValidation(t *testing.T) {
	valid := `{"redirect_uris":["http://127.0.0.1/callback"]}`
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"valid":                {valid, http.StatusCreated},
		"whitespace":           {valid + "\n ", http.StatusCreated},
		"trailing JSON":        {valid + `{}`, http.StatusBadRequest},
		"trailing garbage":     {valid + "garbage", http.StatusBadRequest},
		"oversized whitespace": {valid + strings.Repeat(" ", 16<<10), http.StatusBadRequest},
		"oversized field":      {`{"redirect_uris":["http://127.0.0.1/callback"],"extra":"` + strings.Repeat("x", 16<<10) + `"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			r := newClientRegistry(Registration{Dynamic: true}, "", newMemState(time.Now))
			w := httptest.NewRecorder()
			r.register(w, httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func TestMetadataCachePolicy(t *testing.T) {
	for name, headers := range map[string][]string{
		"cacheable": {"max-age=600"},
		"no store":  {"max-age=600", "no-store"},
		"no cache":  {"max-age=600, no-cache"},
		"zero age":  {"max-age=0"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				for _, h := range headers {
					w.Header().Add("Cache-Control", h)
				}
				_ = json.NewEncoder(w).Encode(clientMetadata{
					ClientID:   "https://" + req.Host + req.URL.Path,
					ClientName: "test", RedirectURIs: []string{"http://127.0.0.1/callback"},
				})
			}))
			defer srv.Close()
			r := newClientRegistry(Registration{ClientIDMetadataDocuments: true}, "", newMemState(time.Now))
			r.fetch = srv.Client()
			for i := 0; i < 2; i++ {
				if _, err := r.get(context.Background(), srv.URL+"/client.json"); err != nil {
					t.Fatal(err)
				}
			}
			want := 2
			if name == "cacheable" {
				want = 1
			}
			if calls != want {
				t.Fatalf("metadata fetches = %d, want %d", calls, want)
			}
		})
	}
}

type oidcAuthRequest struct{ CodeChallenge, CodeChallengeMethod string }

func (r *oidcAuthRequest) req() *oidc.AuthRequest {
	return &oidc.AuthRequest{ClientID: "c", RedirectURI: "http://127.0.0.1/cb", Scopes: []string{"memory"},
		ResponseType: oidc.ResponseTypeCode, CodeChallenge: r.CodeChallenge, CodeChallengeMethod: oidc.CodeChallengeMethod(r.CodeChallengeMethod)}
}

func parseIP(s string) net.IP { return net.ParseIP(s) }

// TestRefreshConcurrentReuse: two token requests that both looked a refresh
// token up before either rotated it. Only one may rotate. Inside the grace
// window the loser fails without ending the family, so the client keeps the
// session through the successor the winner received.
func TestRefreshConcurrentReuse(t *testing.T) {
	cfg := &Config{AccessTokenTTL: Duration(time.Minute), RefreshTokenTTL: Duration(time.Hour), Issuer: testIssuer}
	s := newStore(cfg, nil, nil, newMemState(time.Now))
	ctx := context.Background()
	a := &authRequest{clientID: "c", subject: "p", resource: testResource, scopes: []string{"memory", "offline_access"}}
	_, rt1, _, err := s.CreateAccessAndRefreshTokens(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err1 := s.TokenRequestByRefreshToken(ctx, rt1)
	second, err2 := s.TokenRequestByRefreshToken(ctx, rt1)
	if err1 != nil || err2 != nil {
		t.Fatalf("lookups: %v %v", err1, err2)
	}
	_, rt2, _, err := s.CreateAccessAndRefreshTokens(ctx, first, rt1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.CreateAccessAndRefreshTokens(ctx, second, rt1); err == nil {
		t.Fatal("refresh token rotated twice")
	}
	if _, err := s.TokenRequestByRefreshToken(ctx, rt2); err != nil {
		t.Fatalf("a concurrent duplicate refresh ended the session: %v", err)
	}
}

// TestAuthCodeSingleUse: a code resolves once, so concurrent redemptions
// cannot both reach token issuance.
func TestAuthCodeSingleUse(t *testing.T) {
	s := newStore(&Config{Issuer: testIssuer}, nil, nil, newMemState(time.Now))
	ctx := context.Background()
	r := &oidcAuthRequest{CodeChallenge: "x", CodeChallengeMethod: "S256"}
	a, err := s.CreateAuthRequest(ctx, r.req(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAuthCode(ctx, a.GetID(), "code-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthRequestByCode(ctx, "code-1"); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if _, err := s.AuthRequestByCode(ctx, "code-1"); err == nil {
		t.Fatal("code redeemed twice")
	}
}

// newTestServer builds the facade with no upstream providers, enough to
// drive op's endpoints directly.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	writeKey(t, dir, "k1")
	cfg := &Config{
		Issuer: testIssuer, SigningKeysDir: dir,
		AccessTokenTTL: Duration(time.Minute), RefreshTokenTTL: Duration(time.Hour),
		Registration: Registration{Dynamic: true},
	}
	s, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tmc_a", "tmc_b"} {
		if err := s.clients.state.putClient(context.Background(), &client{id: id, redirectURIs: []string{"http://127.0.0.1/cb"}, source: "dcr"}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// TestRevokeRefreshToken drives op's revocation endpoint: revoking a
// refresh token ends its family, and another client cannot revoke it.
func TestRevokeRefreshToken(t *testing.T) {
	s := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	ctx := context.Background()
	issue := func() string {
		a := &authRequest{clientID: "tmc_a", subject: "p", resource: testResource, scopes: []string{"memory", "offline_access"}}
		_, rt, _, err := s.store.CreateAccessAndRefreshTokens(ctx, a, "")
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	revoke := func(clientID, token string) int {
		form := url.Values{"token": {token}, "client_id": {clientID}, "token_type_hint": {"refresh_token"}}
		req := httptest.NewRequest(http.MethodPost, testIssuer+pathRevoke, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	rt := issue()
	if code := revoke("tmc_b", rt); code != http.StatusOK {
		t.Fatalf("revoke by another client: %d", code)
	}
	if _, err := s.store.TokenRequestByRefreshToken(ctx, rt); err != nil {
		t.Fatal("another client revoked the refresh token")
	}
	if code := revoke("tmc_a", rt); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if _, err := s.store.TokenRequestByRefreshToken(ctx, rt); err == nil {
		t.Fatal("revoked refresh token still accepted")
	}

	// end_session is not offered: it must not delete refresh tokens.
	rt = issue()
	req := httptest.NewRequest(http.MethodGet, testIssuer+"/oauth/end_session?client_id=tmc_a", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound || rec.Code == http.StatusOK {
		t.Fatalf("end_session answered %d", rec.Code)
	}
	if _, err := s.store.TokenRequestByRefreshToken(ctx, rt); err != nil {
		t.Fatal("end_session removed a refresh token")
	}
}

// TestMetadataClientRefusesLoopback: the CIMD fetcher's dialer refuses a
// loopback address even when the URL is otherwise acceptable.
func TestMetadataClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	resp, err := publicOnlyHTTPClient().Get(srv.URL + "/client.json")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("metadata client connected to a loopback address")
	}
	if !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAddIssuer: RFC 9207 iss is added to authorization responses (code or
// error) that leave for a client, and nowhere else.
func TestAddIssuer(t *testing.T) {
	for loc, want := range map[string]string{
		"http://127.0.0.1:1234/cb?code=abc&state=s":    "http://127.0.0.1:1234/cb?code=abc&iss=" + url.QueryEscape(testIssuer) + "&state=s",
		"http://127.0.0.1:1234/cb?error=access_denied": "http://127.0.0.1:1234/cb?error=access_denied&iss=" + url.QueryEscape(testIssuer),
		"http://127.0.0.1:1234/cb?state=s":             "http://127.0.0.1:1234/cb?state=s",
		testIssuer + "/oauth/login?authRequestID=x":    testIssuer + "/oauth/login?authRequestID=x",
		// The redirect to the upstream provider is a request, not a response.
		"https://idp.example.com/auth?response_type=code&state=u": "https://idp.example.com/auth?response_type=code&state=u",
	} {
		if got := addIssuer(loc, testIssuer); got != want {
			t.Errorf("addIssuer(%q) = %q, want %q", loc, got, want)
		}
	}
}
