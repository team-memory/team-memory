package portal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/postgres"
)

// Server-side sessions. The cookie holds a random 256-bit id; the database
// holds only its SHA-256 hash, so reading the table never yields a usable
// cookie. Logging in always issues a new id (an id planted by an attacker is
// never adopted) and revokes any session the browser presented. A session
// ends at the idle timeout (since last use) or the absolute timeout (since
// login), or when it is revoked at logout.

const (
	methodToken = "token"
	methodSSO   = "sso"
	// touchEvery bounds write amplification: last_seen_at moves at most this
	// often, so the idle timeout has this much slack.
	touchEvery = time.Minute
)

// ErrNoSession means the cookie names no live session.
var ErrNoSession = errors.New("no session")

// Session is a live browser session.
type Session struct {
	Hash      string
	Principal uuid.UUID
	CSRF      string
	Method    string
	Created   time.Time
}

type sessions struct {
	pool     *pgxpool.Pool
	idle     time.Duration
	absolute time.Duration
	now      func() time.Time
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashID(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// create starts a session and returns the cookie value.
func (s *sessions) create(ctx context.Context, principal uuid.UUID, method string) (cookie string, sess Session, err error) {
	cookie = randomToken(32)
	sess = Session{Hash: hashID(cookie), Principal: principal, CSRF: randomToken(24), Method: method, Created: s.now()}
	err = postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO portal_sessions (id_hash, principal_id, csrf_token, method, created_at, last_seen_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $5, $6)`, sess.Hash, principal, sess.CSRF, method, sess.Created, sess.Created.Add(s.absolute))
		return err
	})
	return cookie, sess, err
}

// lookup returns the live session for a cookie value.
func (s *sessions) lookup(ctx context.Context, cookie string) (Session, error) {
	if len(cookie) < 20 || len(cookie) > 128 {
		return Session{}, ErrNoSession
	}
	return s.byHash(ctx, hashID(cookie), true)
}

func (s *sessions) byHash(ctx context.Context, hash string, touch bool) (Session, error) {
	var (
		out  Session
		last time.Time
		exp  time.Time
	)
	now := s.now()
	err := postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		err := tx.QueryRow(ctx, `SELECT principal_id, csrf_token, method, created_at, last_seen_at, expires_at
			FROM portal_sessions WHERE id_hash = $1 AND revoked_at IS NULL`, hash).
			Scan(&out.Principal, &out.CSRF, &out.Method, &out.Created, &last, &exp)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		if !now.Before(exp) || now.Sub(last) >= s.idle {
			return ErrNoSession
		}
		if touch && now.Sub(last) >= touchEvery {
			_, err = tx.Exec(ctx, `UPDATE portal_sessions SET last_seen_at = $2 WHERE id_hash = $1`, hash, now)
		}
		return err
	})
	out.Hash = hash
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// revoke ends a session server-side.
func (s *sessions) revoke(ctx context.Context, hash string) error {
	return postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE portal_sessions SET revoked_at = now() WHERE id_hash = $1 AND revoked_at IS NULL`, hash)
		return err
	})
}

// sweep deletes sessions that can no longer be used.
func (s *sessions) sweep(ctx context.Context) {
	_ = postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM portal_sessions WHERE expires_at < $1 OR revoked_at < $1 OR last_seen_at < $2`,
			s.now().Add(-time.Hour), s.now().Add(-s.idle-time.Hour))
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM portal_login_states WHERE expires_at < $1`, s.now())
		}
		return err
	})
}

// --- upstream login states ------------------------------------------------

const loginStateTTL = 10 * time.Minute

type loginState struct {
	Provider    string
	Purpose     string // login | link
	Nonce       string
	Verifier    string
	BrowserHash string
	SessionHash string
	// Started is when the login was started (set by takeLogin).
	Started time.Time
}

func (s *sessions) putLogin(ctx context.Context, state string, l loginState) error {
	return postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM portal_login_states WHERE expires_at > $1`, s.now()).Scan(&n); err != nil {
			return err
		}
		if n >= 10000 {
			return errors.New("too many sign-ins in progress")
		}
		var sh *string
		if l.SessionHash != "" {
			sh = &l.SessionHash
		}
		_, err := tx.Exec(ctx, `INSERT INTO portal_login_states (state_hash, provider, purpose, nonce, verifier, browser_hash, session_hash, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, hashID(state), l.Provider, l.Purpose, l.Nonce, l.Verifier, l.BrowserHash, sh, s.now().Add(loginStateTTL))
		return err
	})
}

// takeLogin consumes a state: a replayed callback finds nothing.
func (s *sessions) takeLogin(ctx context.Context, state string) (loginState, error) {
	var (
		l   loginState
		exp time.Time
		sh  *string
	)
	err := postgres.WithTx(ctx, s.pool, uuid.Nil, func(tx postgres.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM portal_login_states WHERE state_hash = $1
			RETURNING provider, purpose, nonce, verifier, browser_hash, session_hash, expires_at`, hashID(state)).
			Scan(&l.Provider, &l.Purpose, &l.Nonce, &l.Verifier, &l.BrowserHash, &sh, &exp)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		return err
	})
	if err != nil {
		return loginState{}, err
	}
	if !s.now().Before(exp) {
		return loginState{}, ErrNoSession
	}
	if sh != nil {
		l.SessionHash = *sh
	}
	l.Started = exp.Add(-loginStateTTL)
	return l, nil
}
