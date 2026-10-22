package oauth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zitadel/oidc/v3/pkg/oidc"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// stateStore holds the authorization server's mutable state. pgState is
// the production implementation (shared by replicas, survives restarts);
// memState serves unit tests that run without a database. Codes, refresh
// tokens, login state values and browser bindings are passed in hashed. The
// upstream PKCE verifier and nonce are kept as is for at most loginTTL,
// because the callback must present them to the upstream provider.
type stateStore interface {
	putRequest(ctx context.Context, a *authRequest) error
	getRequest(ctx context.Context, id string) (*authRequest, error)
	completeRequest(ctx context.Context, id, subject, idp string, at time.Time) error
	deleteRequest(ctx context.Context, id string) error

	saveCode(ctx context.Context, codeHash, requestID string) error
	takeCode(ctx context.Context, codeHash string) (string, error)

	putRefresh(ctx context.Context, hash string, r *refreshToken) error
	getRefresh(ctx context.Context, hash string) (*refreshToken, error)
	// useRefresh marks the token used if it is unused and unexpired, and
	// returns it. ok is false when it was already used (or is unknown).
	useRefresh(ctx context.Context, hash string) (r *refreshToken, ok bool, err error)
	revokeFamily(ctx context.Context, family string) error

	putClient(ctx context.Context, c *client) error
	getClient(ctx context.Context, id string) (*client, error)

	putLogin(ctx context.Context, stateHash string, p *pendingLogin) error
	takeLogin(ctx context.Context, stateHash string) (*pendingLogin, error)

	sweep(ctx context.Context)
}

var errCapacity = errors.New("capacity reached")

// --- in memory ---------------------------------------------------------

type memState struct {
	now func() time.Time

	mu       sync.Mutex
	requests map[string]*authRequest
	codes    map[string]string
	refresh  map[string]*refreshToken
	clients  map[string]*client
	logins   map[string]*pendingLogin
}

func newMemState(now func() time.Time) *memState {
	return &memState{now: now, requests: map[string]*authRequest{}, codes: map[string]string{},
		refresh: map[string]*refreshToken{}, clients: map[string]*client{}, logins: map[string]*pendingLogin{}}
}

func (m *memState) sweep(context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
}

func (m *memState) sweepLocked() {
	now := m.now()
	for id, a := range m.requests {
		if now.Sub(a.created) > authRequestTTL {
			delete(m.requests, id)
		}
	}
	for c, id := range m.codes {
		if _, ok := m.requests[id]; !ok {
			delete(m.codes, c)
		}
	}
	for h, r := range m.refresh {
		if !now.Before(r.expires) {
			delete(m.refresh, h)
		}
	}
	for k, l := range m.logins {
		if now.After(l.expires) {
			delete(m.logins, k)
		}
	}
}

func (m *memState) putRequest(_ context.Context, a *authRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	if len(m.requests) >= maxPending {
		return errCapacity
	}
	cp := *a
	m.requests[a.id] = &cp
	return nil
}

func (m *memState) getRequest(_ context.Context, id string) (*authRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.requests[id]
	if !ok || m.now().Sub(a.created) > authRequestTTL {
		return nil, errNotFound
	}
	cp := *a
	return &cp, nil
}

func (m *memState) completeRequest(_ context.Context, id, subject, idp string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.requests[id]
	if !ok || a.done || m.now().Sub(a.created) > authRequestTTL {
		return errNotFound
	}
	a.subject, a.idp, a.authTime, a.done = subject, idp, at, true
	return nil
}

func (m *memState) deleteRequest(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.requests, id)
	for c, rid := range m.codes {
		if rid == id {
			delete(m.codes, c)
		}
	}
	return nil
}

func (m *memState) saveCode(_ context.Context, codeHash, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.requests[requestID]; !ok {
		return errNotFound
	}
	m.codes[codeHash] = requestID
	return nil
}

func (m *memState) takeCode(_ context.Context, codeHash string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.codes[codeHash]
	if !ok {
		return "", errNotFound
	}
	delete(m.codes, codeHash)
	return id, nil
}

func (m *memState) putRefresh(_ context.Context, hash string, r *refreshToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	m.refresh[hash] = &cp
	return nil
}

func (m *memState) getRefresh(_ context.Context, hash string) (*refreshToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refresh[hash]
	if !ok || !m.now().Before(r.expires) {
		return nil, errNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *memState) useRefresh(_ context.Context, hash string) (*refreshToken, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refresh[hash]
	if !ok || !r.usedAt.IsZero() || !m.now().Before(r.expires) {
		return nil, false, nil
	}
	r.usedAt = m.now()
	cp := *r
	return &cp, true, nil
}

func (m *memState) revokeFamily(_ context.Context, family string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, r := range m.refresh {
		if r.family == family {
			delete(m.refresh, h)
		}
	}
	return nil
}

func (m *memState) putClient(_ context.Context, c *client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.clients) >= maxDCRClients {
		return errCapacity
	}
	m.clients[c.id] = c
	return nil
}

func (m *memState) getClient(_ context.Context, id string) (*client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[id]
	if !ok {
		return nil, errNotFound
	}
	return c, nil
}

func (m *memState) putLogin(_ context.Context, stateHash string, p *pendingLogin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	if len(m.logins) >= maxPending {
		return errCapacity
	}
	m.logins[stateHash] = p
	return nil
}

func (m *memState) takeLogin(_ context.Context, stateHash string) (*pendingLogin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.logins[stateHash]
	delete(m.logins, stateHash)
	if !ok || m.now().After(p.expires) {
		return nil, errNotFound
	}
	return p, nil
}

// --- PostgreSQL --------------------------------------------------------

// pgState stores OAuth state in the oauth_* tables (migration 00004). Each
// operation is its own short transaction on the app pool.
type pgState struct {
	pool *pgxpool.Pool
	now  func() time.Time

	mu    sync.Mutex
	swept time.Time
}

func (p *pgState) tx(ctx context.Context, fn func(q *db.Queries) error) error {
	return postgres.WithTx(ctx, p.pool, uuid.Nil, func(tx postgres.Tx) error { return fn(tx.Q) })
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	return err
}

// sweep deletes expired state at most once a minute per process.
func (p *pgState) sweep(ctx context.Context) {
	p.mu.Lock()
	now := p.now()
	if now.Sub(p.swept) < time.Minute {
		p.mu.Unlock()
		return
	}
	p.swept = now
	p.mu.Unlock()
	_ = p.tx(ctx, func(q *db.Queries) error {
		if err := q.OAuthSweepAuthRequests(ctx, now.Add(-authRequestTTL)); err != nil {
			return err
		}
		if err := q.OAuthSweepRefreshTokens(ctx, now); err != nil {
			return err
		}
		return q.OAuthSweepPendingLogins(ctx, now)
	})
}

func (p *pgState) putRequest(ctx context.Context, a *authRequest) error {
	p.sweep(ctx)
	id, err := uuid.Parse(a.id)
	if err != nil {
		return err
	}
	return p.tx(ctx, func(q *db.Queries) error {
		n, err := q.OAuthCountAuthRequests(ctx, p.now().Add(-authRequestTTL))
		if err != nil {
			return err
		}
		if n >= maxPending {
			return errCapacity
		}
		return q.OAuthInsertAuthRequest(ctx, db.OAuthInsertAuthRequestParams{
			ID: id, ClientID: a.clientID, RedirectUri: a.redirectURI, Scopes: a.scopes, State: a.state, Nonce: a.nonce,
			ResponseType: string(a.responseType), ResponseMode: string(a.responseMode),
			CodeChallenge: a.codeChallenge.Challenge, CodeChallengeMethod: string(a.codeChallenge.Method),
			Resource: a.resource, CreatedAt: a.created,
		})
	})
}

func (p *pgState) getRequest(ctx context.Context, id string) (*authRequest, error) {
	rid, err := uuid.Parse(id)
	if err != nil {
		return nil, errNotFound
	}
	var out *authRequest
	err = p.tx(ctx, func(q *db.Queries) error {
		r, err := q.OAuthGetAuthRequest(ctx, db.OAuthGetAuthRequestParams{ID: rid, Since: p.now().Add(-authRequestTTL)})
		if err != nil {
			return notFound(err)
		}
		out = &authRequest{
			id: r.ID.String(), clientID: r.ClientID, redirectURI: r.RedirectUri, scopes: r.Scopes, state: r.State,
			nonce: r.Nonce, responseType: oidc.ResponseType(r.ResponseType), responseMode: oidc.ResponseMode(r.ResponseMode),
			codeChallenge: &oidc.CodeChallenge{Challenge: r.CodeChallenge, Method: oidc.CodeChallengeMethod(r.CodeChallengeMethod)},
			resource:      r.Resource, created: r.CreatedAt, done: r.Done,
		}
		if r.Subject != nil {
			out.subject = *r.Subject
		}
		if r.Idp != nil {
			out.idp = *r.Idp
		}
		if r.AuthTime != nil {
			out.authTime = *r.AuthTime
		}
		return nil
	})
	return out, err
}

func (p *pgState) completeRequest(ctx context.Context, id, subject, idp string, at time.Time) error {
	rid, err := uuid.Parse(id)
	if err != nil {
		return errNotFound
	}
	return p.tx(ctx, func(q *db.Queries) error {
		n, err := q.OAuthCompleteAuthRequest(ctx, db.OAuthCompleteAuthRequestParams{
			ID: rid, Subject: &subject, Idp: &idp, AuthTime: &at, Since: p.now().Add(-authRequestTTL)})
		if err == nil && n == 0 {
			return errNotFound
		}
		return err
	})
}

func (p *pgState) deleteRequest(ctx context.Context, id string) error {
	rid, err := uuid.Parse(id)
	if err != nil {
		return nil
	}
	return p.tx(ctx, func(q *db.Queries) error { return q.OAuthDeleteAuthRequest(ctx, rid) })
}

func (p *pgState) saveCode(ctx context.Context, codeHash, requestID string) error {
	rid, err := uuid.Parse(requestID)
	if err != nil {
		return errNotFound
	}
	return p.tx(ctx, func(q *db.Queries) error {
		return notFound(q.OAuthInsertCode(ctx, db.OAuthInsertCodeParams{CodeHash: codeHash, RequestID: rid}))
	})
}

func (p *pgState) takeCode(ctx context.Context, codeHash string) (string, error) {
	var out string
	err := p.tx(ctx, func(q *db.Queries) error {
		id, err := q.OAuthTakeCode(ctx, codeHash)
		out = id.String()
		return notFound(err)
	})
	return out, err
}

func toRefresh(r db.OauthRefreshToken) *refreshToken {
	out := &refreshToken{family: r.Family.String(), clientID: r.ClientID, subject: r.Subject, idp: r.Idp,
		scopes: r.Scopes, resource: r.Resource, authTime: r.AuthTime, expires: r.ExpiresAt}
	if r.UsedAt != nil {
		out.usedAt = *r.UsedAt
	}
	return out
}

func (p *pgState) putRefresh(ctx context.Context, hash string, r *refreshToken) error {
	fam, err := uuid.Parse(r.family)
	if err != nil {
		return err
	}
	return p.tx(ctx, func(q *db.Queries) error {
		return q.OAuthInsertRefreshToken(ctx, db.OAuthInsertRefreshTokenParams{
			TokenHash: hash, Family: fam, ClientID: r.clientID, Subject: r.subject, Idp: r.idp, Scopes: r.scopes,
			Resource: r.resource, AuthTime: r.authTime, ExpiresAt: r.expires,
		})
	})
}

func (p *pgState) getRefresh(ctx context.Context, hash string) (*refreshToken, error) {
	var out *refreshToken
	err := p.tx(ctx, func(q *db.Queries) error {
		r, err := q.OAuthGetRefreshToken(ctx, hash)
		if err != nil {
			return notFound(err)
		}
		if !p.now().Before(r.ExpiresAt) {
			return errNotFound
		}
		out = toRefresh(r)
		return nil
	})
	return out, err
}

func (p *pgState) useRefresh(ctx context.Context, hash string) (*refreshToken, bool, error) {
	var out *refreshToken
	err := p.tx(ctx, func(q *db.Queries) error {
		now := p.now()
		r, err := q.OAuthUseRefreshToken(ctx, db.OAuthUseRefreshTokenParams{TokenHash: hash, Now: &now})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = toRefresh(r)
		return nil
	})
	return out, out != nil, err
}

func (p *pgState) revokeFamily(ctx context.Context, family string) error {
	fam, err := uuid.Parse(family)
	if err != nil {
		return nil
	}
	return p.tx(ctx, func(q *db.Queries) error { return q.OAuthRevokeRefreshFamily(ctx, fam) })
}

func (p *pgState) putClient(ctx context.Context, c *client) error {
	return p.tx(ctx, func(q *db.Queries) error {
		n, err := q.OAuthCountClients(ctx)
		if err != nil {
			return err
		}
		if n >= maxDCRClients {
			return errCapacity
		}
		return q.OAuthInsertClient(ctx, db.OAuthInsertClientParams{ID: c.id, Name: c.name, ClientUri: c.clientURI, RedirectUris: c.redirectURIs})
	})
}

func (p *pgState) getClient(ctx context.Context, id string) (*client, error) {
	var out *client
	err := p.tx(ctx, func(q *db.Queries) error {
		c, err := q.OAuthGetClient(ctx, id)
		if err != nil {
			return notFound(err)
		}
		out = &client{id: c.ID, name: c.Name, clientURI: c.ClientUri, redirectURIs: c.RedirectUris, source: "dcr"}
		return nil
	})
	return out, err
}

func (p *pgState) putLogin(ctx context.Context, stateHash string, l *pendingLogin) error {
	rid, err := uuid.Parse(l.authRequestID)
	if err != nil {
		return err
	}
	return p.tx(ctx, func(q *db.Queries) error {
		n, err := q.OAuthCountPendingLogins(ctx, p.now())
		if err != nil {
			return err
		}
		if n >= maxPending {
			return errCapacity
		}
		return q.OAuthInsertPendingLogin(ctx, db.OAuthInsertPendingLoginParams{
			StateHash: stateHash, AuthRequestID: rid, Provider: l.provider, Nonce: l.nonce, Verifier: l.verifier,
			BrowserHash: l.browser, ExpiresAt: l.expires,
		})
	})
}

func (p *pgState) takeLogin(ctx context.Context, stateHash string) (*pendingLogin, error) {
	var out *pendingLogin
	err := p.tx(ctx, func(q *db.Queries) error {
		l, err := q.OAuthTakePendingLogin(ctx, stateHash)
		if err != nil {
			return notFound(err)
		}
		if p.now().After(l.ExpiresAt) {
			return errNotFound
		}
		out = &pendingLogin{authRequestID: l.AuthRequestID.String(), provider: l.Provider, nonce: l.Nonce,
			verifier: l.Verifier, browser: l.BrowserHash, expires: l.ExpiresAt}
		return nil
	})
	return out, err
}
