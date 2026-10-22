package oauth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/team-memory/team-memory/internal/auth"
)

// The web portal signs people in (and links SSO) through the same upstream
// providers and the same registered callback URL as MCP clients, so an
// administrator registers one redirect URI per provider. States that start
// with PortalStatePrefix belong to the portal and are handed to its
// callback; everything else is an MCP authorization.

// PortalStatePrefix marks upstream state values created by the portal.
const PortalStatePrefix = "p."

// PortalCallback handles an upstream redirect whose state carries
// PortalStatePrefix.
type PortalCallback interface {
	UpstreamCallback(w http.ResponseWriter, r *http.Request, provider string)
}

// SetPortal registers the portal's callback handler.
func (s *Server) SetPortal(h PortalCallback) { s.portal = h }

// ProviderInfo is what a sign-in page shows.
type ProviderInfo struct{ ID, DisplayName string }

// Providers lists the configured upstream providers.
func (s *Server) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		out = append(out, ProviderInfo{ID: p.ID, DisplayName: p.DisplayName})
	}
	return out
}

// ErrUnknownProvider is returned for a provider id that is not configured.
var ErrUnknownProvider = errors.New("unknown identity provider")

// PortalAuthURL starts an upstream login. forceLogin (prompt=login) is used
// for Connect SSO, which must prove control of the upstream identity now.
func (s *Server) PortalAuthURL(provider, state, nonce, verifier string, forceLogin bool) (string, error) {
	up, ok := s.upstreams[provider]
	if !ok {
		return "", ErrUnknownProvider
	}
	return up.authCodeURL(state, nonce, verifier, forceLogin)
}

// PortalExchange redeems the upstream code and applies the provider's
// allow rules. It does not map the identity to a principal.
func (s *Server) PortalExchange(ctx context.Context, provider, code, verifier, nonce string) (auth.ExternalIdentity, error) {
	up, ok := s.upstreams[provider]
	if !ok {
		return auth.ExternalIdentity{}, ErrUnknownProvider
	}
	return up.exchange(ctx, code, verifier, nonce)
}

// PortalResolve maps an upstream identity to an existing or newly
// provisioned principal, with the same rules as an MCP sign-in (never by
// email), and applies group-to-role sync when the provider enables it.
func (s *Server) PortalResolve(ctx context.Context, provider string, id auth.ExternalIdentity) (auth.ExternalIdentityResult, error) {
	up, ok := s.upstreams[provider]
	if !ok {
		return auth.ExternalIdentityResult{}, ErrUnknownProvider
	}
	p, created, err := s.identities.Resolve(ctx, id, up.cfg.autoProvision())
	if err != nil {
		return auth.ExternalIdentityResult{}, err
	}
	if up.cfg.SyncGroupRoles {
		if err := s.identities.SyncGroupRoles(ctx, p.ID, id); err != nil {
			return auth.ExternalIdentityResult{}, err
		}
	}
	return auth.ExternalIdentityResult{Principal: p, Created: created}, nil
}

// IsPortalState reports whether an upstream state value belongs to the portal.
func IsPortalState(state string) bool { return strings.HasPrefix(state, PortalStatePrefix) }

// PolicyError reports whether err is a refusal by the provider's allow rules.
func PolicyError(err error) bool { return errors.Is(err, errPolicy) }

// SignInReason maps a sign-in failure to a fixed audit code.
func SignInReason(err error) string { return signInReason(err) }

// Origins lists the upstream providers' origins, for Content-Security-Policy
// form-action (browsers apply it to redirects after a form post).
func (s *Server) Origins() []string { return append([]string(nil), s.upstreamOrigins...) }
