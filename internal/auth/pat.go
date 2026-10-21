// Package auth turns credentials into memory.Principal values. Personal
// access tokens are the first authenticator; upstream OIDC and the OAuth
// login flow are added in stages 03-04 behind the same interface.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// ErrInvalidCredential covers unknown, malformed, expired and revoked
// credentials alike, so callers cannot tell them apart.
var ErrInvalidCredential = errors.New("invalid credential")

// Authenticator verifies a bearer credential.
type Authenticator interface {
	Authenticate(ctx context.Context, credential string) (memory.Principal, time.Time, error)
}

const (
	// TokenPrefix marks Team Memory personal access tokens so secret scanners
	// can recognise them.
	TokenPrefix = "tmpat_"
	tokenBytes  = 32
	// DefaultTokenTTL is used when no expiry is requested.
	DefaultTokenTTL = 90 * 24 * time.Hour
	// MaxTokenTTL caps token lifetime.
	MaxTokenTTL = 366 * 24 * time.Hour
)

// PATs issues and verifies personal access tokens. Tokens are 256-bit random
// values shown once; only their SHA-256 hash is stored.
type PATs struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewPATs(pool *pgxpool.Pool) *PATs { return &PATs{pool: pool, now: time.Now} }

// WithClock returns a copy using now as the clock (for tests).
func (a *PATs) WithClock(now func() time.Time) *PATs { return &PATs{pool: a.pool, now: now} }

// IssuedToken is returned once at creation; Secret is never stored.
type IssuedToken struct {
	ID        uuid.UUID
	Secret    string
	ExpiresAt time.Time
}

func hashToken(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// Issue creates a token for a principal.
func (a *PATs) Issue(ctx context.Context, principal uuid.UUID, name string, ttl time.Duration) (IssuedToken, error) {
	var out IssuedToken
	err := postgres.WithTx(ctx, a.pool, uuid.Nil, func(tx postgres.Tx) error {
		var err error
		out, err = IssueInTx(ctx, tx.Q, principal, name, ttl, a.now())
		return err
	})
	return out, err
}

// IssueInTx creates a token inside an existing transaction.
func IssueInTx(ctx context.Context, q *db.Queries, principal uuid.UUID, name string, ttl time.Duration, now time.Time) (IssuedToken, error) {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	if ttl > MaxTokenTTL {
		return IssuedToken{}, fmt.Errorf("token lifetime exceeds %s", MaxTokenTTL)
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return IssuedToken{}, err
	}
	secret := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	row, err := q.InsertPersonalAccessToken(ctx, db.InsertPersonalAccessTokenParams{
		PrincipalID: principal, Name: name, TokenPrefix: secret[:len(TokenPrefix)+4],
		TokenHash: hashToken(secret), ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		return IssuedToken{}, err
	}
	return IssuedToken{ID: row.ID, Secret: secret, ExpiresAt: row.ExpiresAt}, nil
}

// Revoke disables a token immediately.
func (a *PATs) Revoke(ctx context.Context, id uuid.UUID) error {
	return postgres.WithTx(ctx, a.pool, uuid.Nil, func(tx postgres.Tx) error {
		n, err := tx.Q.RevokePersonalAccessToken(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("token %s not found or already revoked", id)
		}
		return nil
	})
}

// Authenticate verifies a token and returns its principal and expiry.
func (a *PATs) Authenticate(ctx context.Context, credential string) (memory.Principal, time.Time, error) {
	if !strings.HasPrefix(credential, TokenPrefix) || len(credential) > 200 {
		return memory.Principal{}, time.Time{}, ErrInvalidCredential
	}
	var (
		p   memory.Principal
		exp time.Time
	)
	err := postgres.WithTx(ctx, a.pool, uuid.Nil, func(tx postgres.Tx) error {
		row, err := tx.Q.LookupPersonalAccessToken(ctx, hashToken(credential))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCredential
		}
		if err != nil {
			return err
		}
		if row.RevokedAt != nil || row.DisabledAt != nil || !a.now().Before(row.ExpiresAt) {
			return ErrInvalidCredential
		}
		if err := tx.Q.TouchPersonalAccessToken(ctx, row.ID); err != nil {
			return err
		}
		p = memory.Principal{ID: row.PrincipalID, Kind: row.Kind, DisplayName: row.DisplayName, AuthMethod: "pat"}
		exp = row.ExpiresAt
		return nil
	})
	return p, exp, err
}
