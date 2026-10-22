package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/auth/oauth"
	"github.com/team-memory/team-memory/internal/memory"
	portalpkg "github.com/team-memory/team-memory/internal/portal"
	"github.com/team-memory/team-memory/internal/server"
)

// fakeIdP is a minimal OpenID provider with no dynamic client registration,
// like most enterprise IdPs. Its authorize endpoint logs in the configured
// user immediately.
type fakeIdP struct {
	srv          *httptest.Server
	key          *ecdsa.PrivateKey
	clientID     string
	clientSecret string

	mu    sync.Mutex
	user  map[string]any // claims for the next login
	codes map[string]fakeCode
}

type fakeCode struct {
	nonce, challenge, redirect string
	claims                     map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f := &fakeIdP{key: key, clientID: "team-memory", clientSecret: "upstream-secret-" + uuid.NewString(), codes: map[string]fakeCode{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		// No registration_endpoint and no CIMD support.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"ES256"}, "code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "idp", Algorithm: "ES256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != f.clientID || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		code := uuid.NewString()
		f.mu.Lock()
		f.codes[code] = fakeCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), claims: f.user}
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		f.mu.Lock()
		c, found := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if id != f.clientID || secret != f.clientSecret || !found || c.redirect != r.PostForm.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims := map[string]any{"iss": f.srv.URL, "aud": f.clientID, "exp": time.Now().Add(5 * time.Minute).Unix(),
			"iat": time.Now().Unix(), "nonce": c.nonce}
		for k, v := range c.claims {
			claims[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": f.sign(claims), "token_type": "Bearer", "expires_in": 300, "id_token": f.sign(claims),
		})
	})
	return f
}

func (f *fakeIdP) sign(claims map[string]any) string {
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "idp"))
	b, _ := json.Marshal(claims)
	jws, _ := signer.Sign(b)
	s, _ := jws.CompactSerialize()
	return s
}

func (f *fakeIdP) loginAs(claims map[string]any) {
	f.mu.Lock()
	f.user = claims
	f.mu.Unlock()
}

// ssoEnv is a Team Memory server with SSO pointing at a fake IdP.
type ssoEnv struct {
	url, configPath, secretPath string
	idp                         *fakeIdP
	tr                          *tree
	handler                     *server.Server
}

func writeSigningKey(t *testing.T, dir string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if err := os.WriteFile(filepath.Join(dir, "test-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newSSOEnv starts Team Memory with SSO. edit may change the configuration
// before the server starts.
func newSSOEnv(t *testing.T, edit func(cfg map[string]any, secretPath string)) *ssoEnv {
	t.Helper()
	env := &ssoEnv{idp: newFakeIdP(t), tr: newTree(t)}
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	_ = os.Mkdir(keys, 0o700)
	writeSigningKey(t, keys)
	env.secretPath = filepath.Join(dir, "client-secret")
	_ = os.WriteFile(env.secretPath, []byte(env.idp.clientSecret+"\n"), 0o600)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env.url = "http://" + ln.Addr().String()
	cfg := map[string]any{
		"signing_keys_dir":    keys,
		"allow_insecure_http": true,
		"client_registration": map[string]any{"dynamic": true, "client_id_metadata_documents": true},
		"providers": []map[string]any{{
			"id": "corp", "display_name": "Corp SSO", "issuer": env.idp.srv.URL, "client_id": env.idp.clientID,
			"client_secret_file": env.secretPath, "allowed_email_domains": []string{"example.com"},
		}},
	}
	if edit != nil {
		edit(cfg, env.secretPath)
	}
	b, _ := json.Marshal(cfg)
	env.configPath = filepath.Join(dir, "sso.json")
	_ = os.WriteFile(env.configPath, b, 0o600)

	env.handler = server.New(server.Options{
		Pool: env.tr.pool, Search: env.tr.search, SSOConfigPath: env.configPath, PublicURL: env.url,
		OAuth: oauth.Options{MetadataClient: cimdClient}, Portal: ssoPortalConfig,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env.handler.Start(ctx)
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: env.handler}}
	srv.Start()
	t.Cleanup(srv.Close)
	return env
}

// cimdClient fetches client metadata documents in tests, where they are
// served from loopback TLS servers. Set per test.
var cimdClient *http.Client

// ssoPortalConfig configures the portal of servers newSSOEnv starts.
var ssoPortalConfig portalpkg.Config

func (e *ssoEnv) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := e.handler.Ready(context.Background())
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// browser follows the login like a user agent: it accepts the consent page
// and stops at the MCP client's loopback redirect.
type browser struct {
	t        *testing.T
	client   *http.Client
	redirect string
	consent  func(page string) url.Values // nil: approve with the first provider
}

func newBrowser(t *testing.T, redirect string) *browser {
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, redirect: redirect}
	b.client = &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), redirect) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return b
}

var hiddenField = regexp.MustCompile(`name="(authRequestID|csrf)" value="([^"]+)"`)

// run returns the final redirect to the client, or the last response.
func (b *browser) run(ctx context.Context, authURL string) (*url.URL, *http.Response, string, error) {
	resp, err := b.client.Get(authURL)
	if err != nil {
		return nil, nil, "", err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if loc, err := resp.Location(); err == nil && strings.HasPrefix(loc.String(), b.redirect) {
		return loc, resp, "", nil
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "wants to use Team Memory") {
		return nil, resp, string(body), fmt.Errorf("expected consent page, got %d: %s", resp.StatusCode, body)
	}
	form := url.Values{"provider": {"corp"}}
	for _, m := range hiddenField.FindAllStringSubmatch(string(body), -1) {
		form.Set(m[1], m[2])
	}
	if b.consent != nil {
		form = b.consent(string(body))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, resp.Request.URL.String(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = b.client.Do(req)
	if err != nil {
		return nil, nil, "", err
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	loc, err := resp.Location()
	if err != nil || !strings.HasPrefix(loc.String(), b.redirect) {
		return nil, resp, string(body), fmt.Errorf("login did not return to the client: %d %s", resp.StatusCode, body)
	}
	return loc, resp, string(body), nil
}

func (b *browser) fetcher() sdkauth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		loc, _, _, err := b.run(ctx, args.URL)
		if err != nil {
			return nil, err
		}
		q := loc.Query()
		if e := q.Get("error"); e != "" {
			return nil, fmt.Errorf("authorization error %s: %s", e, q.Get("error_description"))
		}
		return &sdkauth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
}

const clientRedirect = "http://127.0.0.1:47123/callback"

func (e *ssoEnv) connectOAuth(t *testing.T, cfg *sdkauth.AuthorizationCodeHandlerConfig) (*mcp.ClientSession, error) {
	t.Helper()
	h, err := sdkauth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "oauth-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: e.url + "/mcp", OAuthHandler: h, MaxRetries: -1}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func dcrConfig(b *browser) *sdkauth.AuthorizationCodeHandlerConfig {
	return &sdkauth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &sdkauth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "oauth-test", RedirectURIs: []string{clientRedirect}, GrantTypes: []string{"authorization_code", "refresh_token"},
			TokenEndpointAuthMethod: "none",
		}},
		RedirectURL:              clientRedirect,
		AuthorizationCodeFetcher: b.fetcher(),
		RequestRefreshToken:      true,
	}
}

// TestOAuthLoginEndToEnd drives the official go-sdk OAuth client through
// discovery, dynamic registration, consent, upstream login, code exchange,
// and an authenticated tool call. The upstream identity was linked to an
// existing principal first, so the call runs with that principal's roles.
func TestOAuthLoginEndToEnd(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	ctx := context.Background()

	// Unauthenticated requests get the RFC 9728 challenge.
	resp, err := http.Post(env.url+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ch := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != 401 ||
		!strings.Contains(ch, `resource_metadata="`+env.url+oauth.ProtectedResourcePath+`"`) || !strings.Contains(ch, `scope="memory"`) {
		t.Fatalf("challenge: %d %q", resp.StatusCode, ch)
	}

	alice := newPrincipal(t, env.tr.db.Admin, "Alice")
	if err := env.tr.admin.AddMember(ctx, uuid.Nil, "acme/platform/payments", alice.ID, memory.RoleWriter, false); err != nil {
		t.Fatal(err)
	}
	if err := auth.NewIdentities(env.tr.db.Admin).Link(ctx, alice.ID, auth.ExternalIdentity{Issuer: env.idp.srv.URL, Subject: "alice-sub"}); err != nil {
		t.Fatal(err)
	}
	env.idp.loginAs(map[string]any{"sub": "alice-sub", "email": "alice@example.com", "email_verified": true, "name": "Alice A."})

	b := newBrowser(t, clientRedirect)
	cs, err := env.connectOAuth(t, dcrConfig(b))
	if err != nil {
		t.Fatalf("connect with OAuth: %v", err)
	}
	out, isErr := call(t, cs, "memory_create", map[string]any{
		"scope": "acme/platform/payments", "kind": "decision", "title": "OAuth works",
		"content": "Created through the OAuth facade.",
	})
	if isErr {
		t.Fatalf("memory_create: %v", out)
	}
	var creator string
	if err := env.tr.db.Admin.QueryRow(ctx, "SELECT created_by::text FROM memories WHERE id = $1", out["id"]).Scan(&creator); err != nil || creator != alice.ID.String() {
		t.Fatalf("memory created by %s (%v), want linked principal %s", creator, err, alice.ID)
	}
	// Linking did not create a second principal for the same person.
	var n int
	_ = env.tr.db.Admin.QueryRow(ctx, "SELECT count(*) FROM principals WHERE display_name LIKE 'Alice%'").Scan(&n)
	if n != 1 {
		t.Fatalf("%d Alice principals", n)
	}
}

// TestOAuthProvisionedUserHasNoRoles: an unknown identity from an allowed
// domain gets a principal with no memberships, so it can sign in but sees
// nothing until an administrator grants a role.
func TestOAuthProvisionedUserHasNoRoles(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	env.idp.loginAs(map[string]any{"sub": "bob-sub", "email": "bob@example.com", "email_verified": true, "name": "Bob"})
	cs, err := env.connectOAuth(t, dcrConfig(newBrowser(t, clientRedirect)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	out, isErr := call(t, cs, "memory_search", map[string]any{"scope": "acme/platform/payments", "query": "anything"})
	if !isErr || out["error"].(map[string]any)["code"] != "not_found" {
		t.Fatalf("provisioned user without roles searched: %v", out)
	}
	var owner string
	_ = env.tr.db.Admin.QueryRow(context.Background(), "SELECT p.display_name FROM org_settings s JOIN principals p ON p.id = s.owner_principal_id").Scan(&owner)
	if owner != "Owner" {
		t.Fatalf("owner changed to %q", owner)
	}
}

// TestOAuthEmailNeverLinks: an upstream identity whose email equals an
// existing principal's name or another identity's email is not linked to
// it; it becomes its own principal.
func TestOAuthEmailNeverLinks(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	ctx := context.Background()
	ids := auth.NewIdentities(env.tr.db.Admin)
	carol := newPrincipal(t, env.tr.db.Admin, "carol@example.com")
	if err := ids.Link(ctx, carol.ID, auth.ExternalIdentity{Issuer: "https://other-idp.example.com", Subject: "carol", Email: "carol@example.com"}); err != nil {
		t.Fatal(err)
	}
	env.idp.loginAs(map[string]any{"sub": "impostor", "email": "carol@example.com", "email_verified": true})
	if _, err := env.connectOAuth(t, dcrConfig(newBrowser(t, clientRedirect))); err != nil {
		t.Fatal(err)
	}
	p, _, err := ids.Resolve(ctx, auth.ExternalIdentity{Issuer: env.idp.srv.URL, Subject: "impostor"}, false)
	if err != nil || p.ID == carol.ID {
		t.Fatalf("email matched an existing principal: %v %v", p.ID, err)
	}
	// Linking an identity already linked elsewhere is refused.
	if err := ids.Link(ctx, carol.ID, auth.ExternalIdentity{Issuer: env.idp.srv.URL, Subject: "impostor"}); !errors.Is(err, auth.ErrIdentityLinked) {
		t.Fatalf("relink: %v", err)
	}
	// Linking the same pair again is a no-op.
	if err := ids.Link(ctx, carol.ID, auth.ExternalIdentity{Issuer: "https://other-idp.example.com", Subject: "carol"}); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
}

// TestOAuthPolicyRejections covers the allow rules and the protocol
// guards on the authorization endpoint.
func TestOAuthPolicyRejections(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	cfg := func(b *browser) *sdkauth.AuthorizationCodeHandlerConfig { return dcrConfig(b) }

	for name, claims := range map[string]map[string]any{
		"other domain":   {"sub": "eve", "email": "eve@evil.test", "email_verified": true},
		"unverified":     {"sub": "mallory", "email": "mallory@example.com", "email_verified": false},
		"suffix domain":  {"sub": "trudy", "email": "trudy@notexample.com", "email_verified": true},
		"no email claim": {"sub": "nobody"},
	} {
		env.idp.loginAs(claims)
		_, err := env.connectOAuth(t, cfg(newBrowser(t, clientRedirect)))
		if err == nil || !strings.Contains(err.Error(), "access_denied") {
			t.Errorf("%s: sign-in allowed (%v)", name, err)
		}
	}
	var n int
	_ = env.tr.db.Admin.QueryRow(context.Background(), "SELECT count(*) FROM principal_identities").Scan(&n)
	if n != 0 {
		t.Fatalf("%d identities created by refused logins", n)
	}

	// Register a client by hand to probe the authorization endpoint.
	reg, _ := json.Marshal(map[string]any{"client_name": "probe", "redirect_uris": []string{clientRedirect}})
	resp, err := http.Post(env.url+"/oauth/register", "application/json", strings.NewReader(string(reg)))
	if err != nil {
		t.Fatal(err)
	}
	var client struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&client)
	_ = resp.Body.Close()
	if resp.StatusCode != 201 || client.ClientID == "" {
		t.Fatalf("register: %d", resp.StatusCode)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authorize := func(extra url.Values) *http.Response {
		q := url.Values{"client_id": {client.ClientID}, "redirect_uri": {clientRedirect}, "response_type": {"code"},
			"scope": {"memory"}, "state": {"s"}, "code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "code_challenge_method": {"S256"}}
		for k, v := range extra {
			q[k] = v
		}
		resp, err := noFollow.Get(env.url + "/oauth/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	if r := authorize(nil); r.StatusCode != 302 || !strings.HasPrefix(r.Header.Get("Location"), "/oauth/login") && !strings.Contains(r.Header.Get("Location"), "/oauth/login") {
		t.Fatalf("valid request: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := authorize(url.Values{"resource": {env.url + "/mcp"}}); r.StatusCode != 302 || !strings.Contains(r.Header.Get("Location"), "/oauth/login") {
		t.Fatalf("canonical resource refused: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := authorize(url.Values{"resource": {"https://other.example.com/mcp"}}); r.StatusCode != 400 {
		t.Fatalf("foreign resource: %d", r.StatusCode)
	}
	for name, extra := range map[string]url.Values{
		"no PKCE":    {"code_challenge": {""}, "code_challenge_method": {""}},
		"plain PKCE": {"code_challenge_method": {"plain"}},
	} {
		r := authorize(extra)
		loc := r.Header.Get("Location")
		if strings.Contains(loc, "/oauth/login") {
			t.Errorf("%s: reached login (%s)", name, loc)
		}
	}
	if r := authorize(url.Values{"redirect_uri": {"https://attacker.example.com/cb"}}); strings.Contains(r.Header.Get("Location"), "attacker") {
		t.Fatalf("unregistered redirect URI followed: %s", r.Header.Get("Location"))
	}

	// DCR refuses non-loopback http redirect URIs, and by default https
	// redirects to other hosts too (allow_remote_redirect_uris is off).
	for _, uri := range []string{"http://attacker.example.com/cb", "https://attacker.example.com/cb", "custom-scheme://cb"} {
		bad, _ := json.Marshal(map[string]any{"redirect_uris": []string{uri}})
		resp, _ = http.Post(env.url+"/oauth/register", "application/json", strings.NewReader(string(bad)))
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("redirect %s registered: %d", uri, resp.StatusCode)
		}
	}
}

// TestOAuthRejectsUpstreamAndForeignTokens: an upstream IdP token (even one
// whose audience is our resource), a malformed token and an unknown PAT are
// all 401 at /mcp, with the discovery challenge. Team Memory tokens for
// another resource, expired or wrongly issued ones are covered by
// TestAccessTokenValidation.
func TestOAuthRejectsUpstreamAndForeignTokens(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	now := time.Now()
	upstream := env.idp.sign(map[string]any{"iss": env.idp.srv.URL, "sub": "alice-sub", "aud": env.url + "/mcp",
		"exp": now.Add(time.Hour).Unix(), "scope": "memory"})
	for name, tok := range map[string]string{
		"upstream token": upstream,
		"garbage":        "eyJhbGciOiJub25lIn0.e30.",
		"bad PAT":        auth.TokenPrefix + "nope",
	} {
		req, _ := http.NewRequest(http.MethodPost, env.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata") {
			t.Errorf("%s: %d %q", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
}

// TestSoloModeWithSSOEnabledPATsStillWork: PATs keep working next to SSO
// (CI and service accounts).
func TestSSOModeAcceptsPATs(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	tok, err := auth.NewPATs(env.tr.db.Admin).Issue(context.Background(), env.tr.owner.ID, "ci", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m := &mcpEnv{url: env.url + "/mcp", token: tok.Secret, tr: env.tr}
	cs, err := m.connect(t, tok.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, isErr := call(t, cs, "memory_recent", map[string]any{"scope": "acme/platform/payments"}); isErr {
		t.Fatal("PAT call failed with SSO enabled")
	}
}

// TestCIMDRegistration signs in with a Client ID Metadata Document instead
// of dynamic registration.
func TestCIMDRegistration(t *testing.T) {
	doc := httptest.NewTLSServer(nil)
	t.Cleanup(doc.Close)
	clientID := doc.URL + "/oauth/client.json"
	doc.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/client.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": clientID, "client_name": "CIMD Test Client", "redirect_uris": []string{"http://127.0.0.1/callback"},
			"grant_types": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_method": "none",
		})
	})
	cimdClient = doc.Client()
	t.Cleanup(func() { cimdClient = nil })

	env := newSSOEnv(t, nil)
	env.waitReady(t)
	env.idp.loginAs(map[string]any{"sub": "dana", "email": "dana@example.com", "email_verified": true})
	// The registered redirect has no port; the client listens on one
	// (RFC 8252 section 7.3), as Codex does.
	b := newBrowser(t, clientRedirect)
	var page string
	b.consent = func(p string) url.Values {
		page = p
		form := url.Values{"provider": {"corp"}}
		for _, m := range hiddenField.FindAllStringSubmatch(p, -1) {
			form.Set(m[1], m[2])
		}
		return form
	}
	cs, err := env.connectOAuth(t, &sdkauth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &sdkauth.ClientIDMetadataDocumentConfig{URL: clientID},
		RedirectURL:                    clientRedirect,
		AuthorizationCodeFetcher:       b.fetcher(),
	})
	if err != nil {
		t.Fatalf("CIMD login: %v", err)
	}
	if !strings.Contains(page, "CIMD Test Client") || !strings.Contains(page, "127.0.0.1:47123") {
		t.Fatalf("consent page does not name the client and redirect host")
	}
	if out, isErr := call(t, cs, "memory_recent", map[string]any{"scope": "acme"}); !isErr {
		t.Fatalf("provisioned user without roles listed memories: %v", out)
	}
}

// TestConsentCSRF: the consent form must come from the same browser.
func TestConsentCSRF(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	env.idp.loginAs(map[string]any{"sub": "x", "email": "x@example.com", "email_verified": true})
	b := newBrowser(t, clientRedirect)
	b.consent = func(p string) url.Values {
		form := url.Values{"provider": {"corp"}}
		for _, m := range hiddenField.FindAllStringSubmatch(p, -1) {
			form.Set(m[1], m[2])
		}
		form.Set("csrf", "forged")
		return form
	}
	if _, err := env.connectOAuth(t, dcrConfig(b)); err == nil {
		t.Fatal("forged consent accepted")
	}
}

// TestOAuthGroupGate: with allowed_groups set, only members of a listed
// group may sign in. Groups gate sign-in; they never grant roles.
func TestOAuthGroupGate(t *testing.T) {
	env := newSSOEnv(t, func(cfg map[string]any, _ string) {
		p := cfg["providers"].([]map[string]any)[0]
		p["allowed_groups"] = []string{"tm-users"}
		delete(p, "allowed_email_domains")
	})
	env.waitReady(t)
	env.idp.loginAs(map[string]any{"sub": "outsider", "groups": []string{"other"}})
	if _, err := env.connectOAuth(t, dcrConfig(newBrowser(t, clientRedirect))); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("user outside allowed groups signed in: %v", err)
	}
	env.idp.loginAs(map[string]any{"sub": "member", "groups": []string{"other", "tm-users"}})
	cs, err := env.connectOAuth(t, dcrConfig(newBrowser(t, clientRedirect)))
	if err != nil {
		t.Fatalf("group member refused: %v", err)
	}
	if out, isErr := call(t, cs, "memory_recent", map[string]any{"scope": "acme"}); !isErr {
		t.Fatalf("group membership granted a role: %v", out)
	}
}
