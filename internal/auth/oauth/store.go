package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
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
	usedAt   time.Time
}

// refreshGrace is how long after rotation a repeated presentation of the
// old token is treated as a client race rather than theft: the repeat is
// refused, but the family survives, so the request that won keeps the
// session. Later reuse revokes the family.
const refreshGrace = 10 * time.Second

func (r *refreshToken) GetAMR() []string            { return nil }
func (r *refreshToken) GetAudience() []string       { return []string{r.resource} }
func (r *refreshToken) GetAuthTime() time.Time      { return r.authTime }
func (r *refreshToken) GetClientID() string         { return r.clientID }
func (r *refreshToken) GetScopes() []string         { return r.scopes }
func (r *refreshToken) GetSubject() string          { return r.subject }
func (r *refreshToken) SetCurrentScopes(s []string) { r.scopes = s }

// store implements op.Storage on a stateStore.
type store struct {
	cfg     *Config
	keys    *Keyring
	clients *clientRegistry
	state   stateStore
	now     func() time.Time
}

func newStore(cfg *Config, keys *Keyring, clients *clientRegistry, state stateStore) *store {
	return &store{cfg: cfg, keys: keys, clients: clients, state: state, now: time.Now}
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
	a := &authRequest{
		id: uuid.NewString(), clientID: req.ClientID, redirectURI: req.RedirectURI, scopes: req.Scopes,
		state: req.State, nonce: req.Nonce, responseType: req.ResponseType, responseMode: req.ResponseMode,
		codeChallenge: &oidc.CodeChallenge{Challenge: req.CodeChallenge, Method: req.CodeChallengeMethod},
		resource:      resource, created: s.now().UTC().Truncate(time.Microsecond),
	}
	if err := s.state.putRequest(ctx, a); err != nil {
		if errors.Is(err, errCapacity) {
			return nil, oidc.ErrServerError().WithDescription("too many pending sign-ins")
		}
		return nil, err
	}
	return a, nil
}

// request returns a pending authorization request.
func (s *store) request(ctx context.Context, id string) (*authRequest, error) {
	return s.state.getRequest(ctx, id)
}

func (s *store) AuthRequestByID(ctx context.Context, id string) (op.AuthRequest, error) {
	return s.request(ctx, id)
}

// AuthRequestByCode consumes the code: op looks a code up once per token
// request, so two concurrent redemptions of the same code cannot both
// succeed. A failed redemption (wrong PKCE verifier) also burns the code.
func (s *store) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	id, err := s.state.takeCode(ctx, hashToken(code))
	if err != nil {
		return nil, err
	}
	return s.request(ctx, id)
}

func (s *store) SaveAuthCode(ctx context.Context, id, code string) error {
	return s.state.saveCode(ctx, hashToken(code), id)
}

// DeleteAuthRequest is called when a code is redeemed, so a code is single
// use.
func (s *store) DeleteAuthRequest(ctx context.Context, id string) error {
	return s.state.deleteRequest(ctx, id)
}

// complete records the upstream login result on a pending request.
func (s *store) complete(ctx context.Context, id, subject, idp string, at time.Time) error {
	return s.state.completeRequest(ctx, id, subject, idp, at)
}

func (s *store) CreateAccessToken(context.Context, op.TokenRequest) (string, time.Time, error) {
	return uuid.NewString(), s.now().Add(time.Duration(s.cfg.AccessTokenTTL)), nil
}

func (s *store) CreateAccessAndRefreshTokens(ctx context.Context, req op.TokenRequest, current string) (string, string, time.Time, error) {
	var next refreshToken
	switch r := req.(type) {
	case *authRequest:
		next = refreshToken{family: uuid.NewString(), clientID: r.clientID, subject: r.subject, idp: r.idp,
			scopes: r.scopes, resource: r.resource, authTime: r.authTime,
			expires: s.now().Add(time.Duration(s.cfg.RefreshTokenTTL))}
	case *refreshToken:
		old, ok, err := s.state.useRefresh(ctx, hashToken(current))
		if err != nil {
			return "", "", time.Time{}, err
		}
		if !ok {
			// Another request rotated the token after op looked it up.
			if err := s.reused(ctx, current); err != nil {
				return "", "", time.Time{}, err
			}
			return "", "", time.Time{}, op.ErrInvalidRefreshToken
		}
		next = *old
		next.usedAt = time.Time{}
		next.scopes = r.scopes
		// Rotation keeps the original lifetime: refresh never extends a
		// session beyond refresh_token_ttl from the login.
	default:
		return "", "", time.Time{}, errors.New("unsupported token request")
	}
	secret := "tmrt_" + randomString(32)
	if err := s.state.putRefresh(ctx, hashToken(secret), &next); err != nil {
		return "", "", time.Time{}, err
	}
	return uuid.NewString(), secret, s.now().Add(time.Duration(s.cfg.AccessTokenTTL)), nil
}

// reused handles a rotated token presented again: inside refreshGrace it is
// a client race and only this request fails; later it is treated as theft
// and the whole family is revoked (OAuth 2.1 section 4.3.1).
func (s *store) reused(ctx context.Context, token string) error {
	r, err := s.state.getRefresh(ctx, hashToken(token))
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !r.usedAt.IsZero() && s.now().Sub(r.usedAt) <= refreshGrace {
		return nil
	}
	return s.state.revokeFamily(ctx, r.family)
}

func (s *store) TokenRequestByRefreshToken(ctx context.Context, token string) (op.RefreshTokenRequest, error) {
	r, err := s.state.getRefresh(ctx, hashToken(token))
	if errors.Is(err, errNotFound) {
		return nil, op.ErrInvalidRefreshToken
	}
	if err != nil {
		return nil, err
	}
	if !r.usedAt.IsZero() {
		if err := s.reused(ctx, token); err != nil {
			return nil, err
		}
		return nil, op.ErrInvalidRefreshToken
	}
	return r, nil
}

// TerminateSession backs op's end_session endpoint, which Team Memory does
// not advertise. It is refused: op accepts an expired id_token_hint there,
// and clients end their sign-in by revoking the refresh token instead.
func (s *store) TerminateSession(context.Context, string, string) error {
	return oidc.ErrRequestNotSupported().WithDescription("end_session is not supported; revoke the refresh token")
}

func (s *store) RevokeToken(ctx context.Context, token, _, clientID string) *oidc.Error {
	if r, err := s.state.getRefresh(ctx, hashToken(token)); err == nil && r.clientID == clientID {
		if err := s.state.revokeFamily(ctx, r.family); err != nil {
			return oidc.ErrServerError()
		}
	}
	// Access tokens are short-lived JWTs and cannot be revoked
	// individually; revoking succeeds silently (RFC 7009 section 2.2).
	return nil
}

// GetRefreshTokenInfo is called by op's revocation endpoint, which then
// passes the returned token ID to RevokeToken. The ID is the token itself,
// because RevokeToken looks tokens up by hash; returning the family ID
// would make every refresh-token revocation a silent no-op.
func (s *store) GetRefreshTokenInfo(ctx context.Context, clientID, token string) (string, string, error) {
	r, err := s.state.getRefresh(ctx, hashToken(token))
	if err != nil || r.clientID != clientID {
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
	// token_use distinguishes access tokens from ID tokens, which share the
	// issuer, signing keys and (in op) the audience. The verifier requires it.
	claims := map[string]any{"token_use": tokenUseAccess, "scope": strings.Join(slices.DeleteFunc(slices.Clone(req.GetScopes()), func(sc string) bool {
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
