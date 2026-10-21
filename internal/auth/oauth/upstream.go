package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/team-memory/team-memory/internal/auth"
)

// upstream is one configured OpenID Connect provider. Team Memory is a
// confidential client of it, registered in advance by an administrator;
// MCP clients never talk to it directly.
type upstream struct {
	cfg         Provider
	redirectURL string
	httpClient  *http.Client

	mu       sync.Mutex
	provider *gooidc.Provider
	err      error
}

func newUpstream(cfg Provider, redirectURL string, hc *http.Client) *upstream {
	return &upstream{cfg: cfg, redirectURL: redirectURL, httpClient: hc, err: errors.New("discovery has not run yet")}
}

// discover fetches the provider's OpenID configuration. go-oidc checks that
// the document's issuer equals the configured issuer.
func (u *upstream) discover(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(gooidc.ClientContext(ctx, u.httpClient), 10*time.Second)
	defer cancel()
	p, err := gooidc.NewProvider(ctx, u.cfg.Issuer)
	if err == nil {
		var claims struct {
			Methods []string `json:"code_challenge_methods_supported"`
		}
		if cerr := p.Claims(&claims); cerr == nil && !slices.Contains(claims.Methods, "S256") {
			err = errors.New("provider does not advertise PKCE S256")
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.err = fmt.Errorf("provider %q discovery: %w", u.cfg.ID, err)
		return u.err
	}
	u.provider, u.err = p, nil
	return nil
}

func (u *upstream) status() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

// clientSecret reads the secret on every use, so a rotated Kubernetes
// Secret takes effect without a restart once the kubelet updates the file.
func (u *upstream) clientSecret() (string, error) {
	b, err := os.ReadFile(u.cfg.ClientSecretFile)
	if err != nil {
		return "", fmt.Errorf("provider %q client secret: %w", u.cfg.ID, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("provider %q client secret file %s is empty", u.cfg.ID, u.cfg.ClientSecretFile)
	}
	return s, nil
}

func (u *upstream) oauth2Config() (*oauth2.Config, *gooidc.Provider, error) {
	u.mu.Lock()
	p, perr := u.provider, u.err
	u.mu.Unlock()
	if perr != nil {
		return nil, nil, perr
	}
	secret, err := u.clientSecret()
	if err != nil {
		return nil, nil, err
	}
	return &oauth2.Config{
		ClientID: u.cfg.ClientID, ClientSecret: secret, Endpoint: p.Endpoint(),
		RedirectURL: u.redirectURL, Scopes: u.cfg.Scopes,
	}, p, nil
}

// authCodeURL starts the upstream login with state, nonce and PKCE.
func (u *upstream) authCodeURL(state, nonce, verifier string) (string, error) {
	oc, _, err := u.oauth2Config()
	if err != nil {
		return "", err
	}
	return oc.AuthCodeURL(state, gooidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// upstreamClaims are the ID token claims Team Memory reads.
type upstreamClaims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"`
	Name          string `json:"name"`
	Nonce         string `json:"nonce"`
}

// exchange redeems the upstream code and verifies the ID token: signature,
// issuer, audience (our client ID), expiry and nonce. The upstream access
// token is discarded; it is never stored or forwarded.
func (u *upstream) exchange(ctx context.Context, code, verifier, nonce string) (auth.ExternalIdentity, error) {
	oc, p, err := u.oauth2Config()
	if err != nil {
		return auth.ExternalIdentity{}, err
	}
	ctx = gooidc.ClientContext(ctx, u.httpClient)
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return auth.ExternalIdentity{}, fmt.Errorf("code exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return auth.ExternalIdentity{}, errors.New("provider returned no id_token")
	}
	idt, err := p.Verifier(&gooidc.Config{ClientID: u.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return auth.ExternalIdentity{}, fmt.Errorf("id_token: %w", err)
	}
	var c upstreamClaims
	if err := idt.Claims(&c); err != nil {
		return auth.ExternalIdentity{}, fmt.Errorf("id_token claims: %w", err)
	}
	if c.Nonce != nonce {
		return auth.ExternalIdentity{}, errors.New("id_token nonce mismatch")
	}
	if err := u.checkPolicy(idt, c); err != nil {
		return auth.ExternalIdentity{}, err
	}
	return auth.ExternalIdentity{Issuer: idt.Issuer, Subject: idt.Subject, Email: c.Email, Name: c.Name}, nil
}

// errPolicy marks a login refused by the provider's allow rules.
var errPolicy = errors.New("sign-in not allowed")

func (u *upstream) checkPolicy(idt *gooidc.IDToken, c upstreamClaims) error {
	if len(u.cfg.AllowedEmailDomains) > 0 {
		if !truthy(c.EmailVerified) {
			return fmt.Errorf("%w: email is not verified", errPolicy)
		}
		at := strings.LastIndexByte(c.Email, '@')
		if at < 1 || !slices.Contains(u.cfg.AllowedEmailDomains, strings.ToLower(c.Email[at+1:])) {
			return fmt.Errorf("%w: email domain is not allowed", errPolicy)
		}
	}
	if len(u.cfg.AllowedGroups) > 0 {
		var all map[string]any
		if err := idt.Claims(&all); err != nil {
			return err
		}
		groups, _ := all[u.cfg.GroupsClaim].([]any)
		for _, g := range groups {
			if s, ok := g.(string); ok && slices.Contains(u.cfg.AllowedGroups, s) {
				return nil
			}
		}
		return fmt.Errorf("%w: not in an allowed group", errPolicy)
	}
	return nil
}

// truthy accepts the boolean and the string forms of email_verified; some
// providers send "true".
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true"
	}
	return false
}
