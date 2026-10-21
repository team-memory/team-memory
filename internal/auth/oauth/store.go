package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// Lifetimes of the short-lived login state.
const (
	authRequestTTL = 10 * time.Minute
	maxDCRClients  = 10000
	maxPending     = 10000
)

var errNotFound = notFoundError{}

type notFoundError struct{}

func (notFoundError) Error() string { return "not found" }
func (notFoundError) IsNotFound()   {}

// authRequest is one authorization request from an MCP client, from
// /oauth/authorize until its code is redeemed.
type authRequest struct {
	id            string
	clientID      string
	redirectURI   string
	scopes        []string
	state         string
	nonce         string
	responseType  oidc.ResponseType
	responseMode  oidc.ResponseMode
	codeChallenge *oidc.CodeChallenge
	resource      string
	created       time.Time

	// Set when the upstream login completes.
	subject  string
	idp      string
	authTime time.Time
	done     bool
}

func (a *authRequest) GetID() string                         { return a.id }
func (a *authRequest) GetACR() string                        { return "" }
func (a *authRequest) GetAMR() []string                      { return nil }
func (a *authRequest) GetAudience() []string                 { return []string{a.resource} }
func (a *authRequest) GetAuthTime() time.Time                { return a.authTime }
func (a *authRequest) GetClientID() string                   { return a.clientID }
func (a *authRequest) GetCodeChallenge() *oidc.CodeChallenge { return a.codeChallenge }
func (a *authRequest) GetNonce() string                      { return a.nonce }
func (a *authRequest) GetRedirectURI() string                { return a.redirectURI }
func (a *authRequest) GetResponseType() oidc.ResponseType    { return a.responseType }
func (a *authRequest) GetResponseMode() oidc.ResponseMode    { return a.responseMode }
func (a *authRequest) GetScopes() []string                   { return a.scopes }
func (a *authRequest) GetState() string                      { return a.state }
func (a *authRequest) GetSubject() string                    { return a.subject }
func (a *authRequest) Done() bool                            { return a.done }

// refreshToken is stored by hash. Each use rotates it; presenting a rotated
// token again revokes its whole family (OAuth 2.1 section 4.3.1).
type refreshToken struct {
	family   string
	clientID string
	subject  string
	idp      string
	scopes   []string
	resource string
	authTime time.Time
	expires  time.Time
	used     bool
}

func (r *refreshToken) GetAMR() []string            { return nil }
func (r *refreshToken) GetAudience() []string       { return []string{r.resource} }
func (r *refreshToken) GetAuthTime() time.Time      { return r.authTime }
func (r *refreshToken) GetClientID() string         { return r.clientID }
func (r *refreshToken) GetScopes() []string         { return r.scopes }
func (r *refreshToken) GetSubject() string          { return r.subject }
func (r *refreshToken) SetCurrentScopes(s []string) { r.scopes = s }

// store implements op.Storage in memory.
type store struct {
	cfg     *Config
	keys    *Keyring
	clients *clientRegistry
	now     func() time.Time

	mu       sync.Mutex
	requests map[string]*authRequest
	codes    map[string]string // code -> request id
	refresh  map[string]*refreshToken
}

func newStore(cfg *Config, keys *Keyring, clients *clientRegistry) *store {
	return &store{
		cfg: cfg, keys: keys, clients: clients, now: time.Now,
		requests: map[string]*authRequest{}, codes: map[string]string{}, refresh: map[string]*refreshToken{},
	}
}

type ctxKey int

const resourceKey ctxKey = iota

func withResource(ctx context.Context, resource string) context.Context {
	return context.WithValue(ctx, resourceKey, resource)
}

func (s *store) CreateAuthRequest(ctx context.Context, req *oidc.AuthRequest, _ string) (op.AuthRequest, error) {
	// OAuth 2.1 and MCP require PKCE with S256 for every client.
	if req.CodeChallenge == "" || req.CodeChallengeMethod != oidc.CodeChallengeMethodS256 {
		return nil, oidc.ErrInvalidRequest().WithDescription("PKCE with code_challenge_method=S256 is required")
	}
	// The memory scope is the only resource scope and the consent page
	// describes it; a client that asks only for openid or offline_access
	// gets it too rather than a token the MCP endpoint would refuse.
	if !slices.Contains(req.Scopes, ScopeMemory) {
		req.Scopes = append(req.Scopes, ScopeMemory)
	}
	resource, _ := ctx.Value(resourceKey).(string)
	if resource == "" {
		// Our only resource; a client that omits the parameter gets it.
		resource = s.cfg.Resource()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if len(s.requests) >= maxPending {
		return nil, oidc.ErrServerError().WithDescription("too many pending sign-ins")
	}
	a := &authRequest{
		id: uuid.NewString(), clientID: req.ClientID, redirectURI: req.RedirectURI, scopes: req.Scopes,
		state: req.State, nonce: req.Nonce, responseType: req.ResponseType, responseMode: req.ResponseMode,
		codeChallenge: &oidc.CodeChallenge{Challenge: req.CodeChallenge, Method: req.CodeChallengeMethod},
		resource:      resource, created: s.now(),
	}
	s.requests[a.id] = a
	return a, nil
}

func (s *store) sweepLocked() {
	cut := s.now().Add(-authRequestTTL)
	for id, a := range s.requests {
		if a.created.Before(cut) {
			delete(s.requests, id)
		}
	}
	for c, id := range s.codes {
		if _, ok := s.requests[id]; !ok {
			delete(s.codes, c)
		}
	}
	now := s.now()
	for h, r := range s.refresh {
		if !now.Before(r.expires) {
			delete(s.refresh, h)
		}
	}
}

// request returns a copy, so callers never read fields that complete
// writes concurrently.
func (s *store) request(id string) (*authRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.requests[id]
	if !ok || s.now().Sub(a.created) > authRequestTTL {
		return nil, errNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *store) AuthRequestByID(_ context.Context, id string) (op.AuthRequest, error) {
	return s.request(id)
}

// AuthRequestByCode consumes the code: op looks a code up once per token
// request, so two concurrent redemptions of the same code cannot both
// succeed. A failed redemption (wrong PKCE verifier) also burns the code.
func (s *store) AuthRequestByCode(_ context.Context, code string) (op.AuthRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.codes[code]
	if !ok {
		return nil, errNotFound
	}
	delete(s.codes, code)
	a, ok := s.requests[id]
	if !ok || s.now().Sub(a.created) > authRequestTTL {
		return nil, errNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *store) SaveAuthCode(_ context.Context, id, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.requests[id]; !ok {
		return errNotFound
	}
	s.codes[code] = id
	return nil
}

// DeleteAuthRequest is called when a code is redeemed, so a code is single
// use.
func (s *store) DeleteAuthRequest(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.requests, id)
	for c, rid := range s.codes {
		if rid == id {
			delete(s.codes, c)
		}
	}
	return nil
}

// complete records the upstream login result on a pending request.
func (s *store) complete(id, subject, idp string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.requests[id]
	if !ok || a.done || s.now().Sub(a.created) > authRequestTTL {
		return errNotFound
	}
	a.subject, a.idp, a.authTime, a.done = subject, idp, at, true
	return nil
}

func (s *store) CreateAccessToken(context.Context, op.TokenRequest) (string, time.Time, error) {
	return uuid.NewString(), s.now().Add(time.Duration(s.cfg.AccessTokenTTL)), nil
}

func (s *store) CreateAccessAndRefreshTokens(_ context.Context, req op.TokenRequest, current string) (string, string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next refreshToken
	switch r := req.(type) {
	case *authRequest:
		next = refreshToken{family: uuid.NewString(), clientID: r.clientID, subject: r.subject, idp: r.idp,
			scopes: r.scopes, resource: r.resource, authTime: r.authTime}
	case *refreshToken:
		old, ok := s.refresh[hashToken(current)]
		if !ok {
			return "", "", time.Time{}, op.ErrInvalidRefreshToken
		}
		if old.used {
			// Two requests looked the token up before either rotated it:
			// this is reuse too, so the family ends here as well.
			s.revokeFamilyLocked(old.family)
			return "", "", time.Time{}, op.ErrInvalidRefreshToken
		}
		old.used = true
		next = *old
		next.used = false
		next.scopes = r.scopes
	default:
		return "", "", time.Time{}, errors.New("unsupported token request")
	}
	next.expires = s.now().Add(time.Duration(s.cfg.RefreshTokenTTL))
	if req, ok := req.(*refreshToken); ok {
		// Rotation keeps the original lifetime: refresh never extends a
		// session beyond refresh_token_ttl from the login.
		next.expires = req.expires
	}
	secret := "tmrt_" + randomString(32)
	s.refresh[hashToken(secret)] = &next
	return uuid.NewString(), secret, s.now().Add(time.Duration(s.cfg.AccessTokenTTL)), nil
}

func (s *store) TokenRequestByRefreshToken(_ context.Context, token string) (op.RefreshTokenRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refresh[hashToken(token)]
	if !ok || !s.now().Before(r.expires) {
		return nil, op.ErrInvalidRefreshToken
	}
	if r.used {
		// Reuse of a rotated token: assume theft and end the family.
		s.revokeFamilyLocked(r.family)
		return nil, op.ErrInvalidRefreshToken
	}
	cp := *r
	return &cp, nil
}

func (s *store) revokeFamilyLocked(family string) {
	for h, r := range s.refresh {
		if r.family == family {
			delete(s.refresh, h)
		}
	}
}

// TerminateSession backs op's end_session endpoint, which Team Memory does
// not advertise. It is refused: op accepts an expired id_token_hint there,
// and clients end their sign-in by revoking the refresh token instead.
func (s *store) TerminateSession(context.Context, string, string) error {
	return oidc.ErrRequestNotSupported().WithDescription("end_session is not supported; revoke the refresh token")
}

func (s *store) RevokeToken(_ context.Context, token, _, clientID string) *oidc.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.refresh[hashToken(token)]; ok && r.clientID == clientID {
		s.revokeFamilyLocked(r.family)
	}
	// Access tokens are short-lived JWTs and cannot be revoked
	// individually; revoking succeeds silently (RFC 7009 section 2.2).
	return nil
}

// GetRefreshTokenInfo is called by op's revocation endpoint, which then
// passes the returned token ID to RevokeToken. The ID is the token itself,
// because RevokeToken looks tokens up by hash; returning the family ID
// would make every refresh-token revocation a silent no-op.
func (s *store) GetRefreshTokenInfo(_ context.Context, clientID, token string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refresh[hashToken(token)]
	if !ok || r.clientID != clientID {
		return "", "", op.ErrInvalidRefreshToken
	}
	return r.subject, token, nil
}

func (s *store) SigningKey(context.Context) (op.SigningKey, error) { return s.keys.Active(), nil }
func (s *store) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return s.keys.Algorithms(), nil
}
func (s *store) KeySet(context.Context) ([]op.Key, error) { return s.keys.Public(), nil }

func (s *store) GetClientByClientID(ctx context.Context, id string) (op.Client, error) {
	c, err := s.clients.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// AuthorizeClientIDSecret is never satisfied: every MCP client is public
// and proves possession through PKCE.
func (s *store) AuthorizeClientIDSecret(context.Context, string, string) error {
	return oidc.ErrInvalidClient().WithDescription("client secrets are not supported; use PKCE")
}

func (s *store) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil
}

func (s *store) SetUserinfoFromRequest(_ context.Context, ui *oidc.UserInfo, req op.IDTokenRequest, _ []string) error {
	ui.Subject = req.GetSubject()
	return nil
}

func (s *store) SetUserinfoFromToken(_ context.Context, ui *oidc.UserInfo, _, subject, _ string) error {
	ui.Subject = subject
	return nil
}

func (s *store) SetIntrospectionFromToken(context.Context, *oidc.IntrospectionResponse, string, string, string) error {
	return errors.New("introspection is not supported")
}

func (s *store) GetPrivateClaimsFromScopes(context.Context, string, string, []string) (map[string]any, error) {
	return nil, nil
}

// GetPrivateClaimsFromRequest adds the granted scopes and the upstream
// issuer to access tokens. The idp claim is informational: authorization
// uses only the subject (the principal id).
func (s *store) GetPrivateClaimsFromRequest(_ context.Context, req op.TokenRequest, _ []string) (map[string]any, error) {
	claims := map[string]any{"scope": strings.Join(slices.DeleteFunc(slices.Clone(req.GetScopes()), func(sc string) bool {
		return sc == oidc.ScopeOpenID || sc == oidc.ScopeOfflineAccess
	}), " ")}
	switch r := req.(type) {
	case *authRequest:
		claims["idp"] = r.idp
	case *refreshToken:
		claims["idp"] = r.idp
	}
	return claims, nil
}

func (s *store) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, errors.New("private_key_jwt is not supported")
}

func (s *store) ValidateJWTProfileScopes(context.Context, string, []string) ([]string, error) {
	return nil, errors.New("JWT profile grants are not supported")
}

func (s *store) Health(context.Context) error { return nil }

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
