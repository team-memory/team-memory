// Package oauth is Team Memory's OAuth 2.1 authorization server facade.
//
// MCP clients discover it through protected-resource metadata, register by
// Client ID Metadata Document or dynamic registration, and send users through
// a consent page to a configured upstream OpenID Connect provider. After the
// upstream login, the facade issues its own short-lived access tokens whose
// audience is the Team Memory MCP endpoint. Upstream tokens are never
// accepted by, or passed through, Team Memory.
//
// Stage 03 prototype: authorization requests, registered clients and refresh
// tokens are held in memory (see store.go). Stage 04 moves them to
// PostgreSQL before more than one replica may run.
package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ScopeMemory is the one resource scope: access to the MCP endpoint, with
// permissions decided by the principal's scope roles.
const ScopeMemory = "memory"

// Config is the SSO configuration, read from a JSON file (a Kubernetes
// ConfigMap). Secrets are never inline: every secret is a path to a file
// mounted from a Kubernetes Secret.
type Config struct {
	// Issuer is the public base URL of this Team Memory deployment, e.g.
	// https://memory.example.com. Defaults to TEAM_MEMORY_PUBLIC_URL.
	Issuer string `json:"issuer"`
	// SigningKeysDir holds one PEM private key per file (<kid>.pem). Every
	// key is published in the JWKS and accepted for verification; only the
	// active key signs.
	SigningKeysDir   string `json:"signing_keys_dir"`
	ActiveSigningKey string `json:"active_signing_key"`

	AccessTokenTTL  Duration `json:"access_token_ttl"`
	RefreshTokenTTL Duration `json:"refresh_token_ttl"`

	Registration Registration `json:"client_registration"`
	Providers    []Provider   `json:"providers"`

	// AllowInsecureHTTP permits an http:// issuer on a loopback host, for
	// local development only.
	AllowInsecureHTTP bool `json:"allow_insecure_http"`
}

// Registration selects how MCP clients obtain a client ID.
type Registration struct {
	ClientIDMetadataDocuments bool `json:"client_id_metadata_documents"`
	Dynamic                   bool `json:"dynamic"`
	// CIMDAllowedHosts restricts metadata document hosts. Empty allows any
	// public HTTPS host; the consent page always names the client.
	CIMDAllowedHosts []string `json:"cimd_allowed_hosts"`
	// AllowRemoteRedirectURIs accepts https redirect URIs on other hosts,
	// for browser-based MCP clients. Off by default: CLI clients use
	// loopback redirects, and a remote redirect lets any registered app
	// that tricks a user into consenting receive their code.
	AllowRemoteRedirectURIs bool `json:"allow_remote_redirect_uris"`
}

// Provider is one upstream OpenID Connect provider.
type Provider struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	Issuer           string   `json:"issuer"`
	ClientID         string   `json:"client_id"`
	ClientSecretFile string   `json:"client_secret_file"`
	Scopes           []string `json:"scopes"`
	// AllowedEmailDomains, when set, requires a verified email in one of
	// these domains. AllowedGroups, when set, requires membership in at
	// least one group named in GroupsClaim.
	AllowedEmailDomains []string `json:"allowed_email_domains"`
	AllowedGroups       []string `json:"allowed_groups"`
	GroupsClaim         string   `json:"groups_claim"`
	// AutoProvision creates a principal, without roles, on the first login
	// of an unknown identity. Default true.
	AutoProvision *bool `json:"auto_provision"`
	// SyncGroupRoles applies the operator-managed idp_group_mappings for
	// this issuer at every sign-in (team-memory idp-groups). Off by
	// default: roles are then managed only in the database.
	SyncGroupRoles bool `json:"sync_group_roles"`
}

func (p Provider) autoProvision() bool { return p.AutoProvision == nil || *p.AutoProvision }

// Duration is a time.Duration that unmarshals from strings like "15m".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`durations are strings such as "15m"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

const (
	defaultAccessTTL  = 15 * time.Minute
	maxAccessTTL      = time.Hour
	defaultRefreshTTL = 30 * 24 * time.Hour
	maxRefreshTTL     = 90 * 24 * time.Hour
)

var providerID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// LoadConfig reads and statically validates the configuration file. It does
// not read secrets or contact providers; see New and Server.Ready.
func LoadConfig(path, publicURL string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sso config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("sso config %s: %w", path, err)
	}
	if c.Issuer == "" {
		c.Issuer = publicURL
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("sso config %s: %w", path, err)
	}
	return &c, nil
}

// Validate checks the configuration and fills defaults.
func (c *Config) Validate() error {
	c.Issuer = strings.TrimRight(c.Issuer, "/")
	u, err := url.Parse(c.Issuer)
	// A path would move every well-known URL (RFC 8414 path insertion); the
	// prototype serves Team Memory at the root of its host only.
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("issuer must be an origin URL such as https://memory.example.com, without path, query, fragment or credentials (set issuer or TEAM_MEMORY_PUBLIC_URL)")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && c.AllowInsecureHTTP && isLoopbackHost(u.Hostname()):
	default:
		return errors.New("issuer must use https (http is allowed only on a loopback host with allow_insecure_http)")
	}
	if c.SigningKeysDir == "" {
		return errors.New("signing_keys_dir is required")
	}
	if c.AccessTokenTTL == 0 {
		c.AccessTokenTTL = Duration(defaultAccessTTL)
	}
	if c.RefreshTokenTTL == 0 {
		c.RefreshTokenTTL = Duration(defaultRefreshTTL)
	}
	if time.Duration(c.AccessTokenTTL) < time.Minute || time.Duration(c.AccessTokenTTL) > maxAccessTTL {
		return fmt.Errorf("access_token_ttl must be between 1m and %s", maxAccessTTL)
	}
	if time.Duration(c.RefreshTokenTTL) < time.Duration(c.AccessTokenTTL) || time.Duration(c.RefreshTokenTTL) > maxRefreshTTL {
		return fmt.Errorf("refresh_token_ttl must be between access_token_ttl and %s", maxRefreshTTL)
	}
	if !c.Registration.ClientIDMetadataDocuments && !c.Registration.Dynamic {
		return errors.New("client_registration must enable client_id_metadata_documents, dynamic, or both")
	}
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	seen := map[string]bool{}
	for i := range c.Providers {
		p := &c.Providers[i]
		if !providerID.MatchString(p.ID) {
			return fmt.Errorf("provider %d: id must match %s", i, providerID)
		}
		if seen[p.ID] {
			return fmt.Errorf("provider %q: duplicate id", p.ID)
		}
		seen[p.ID] = true
		iu, err := url.Parse(p.Issuer)
		if err != nil || iu.Host == "" || (iu.Scheme != "https" && (iu.Scheme != "http" || !c.AllowInsecureHTTP || !isLoopbackHost(iu.Hostname()))) {
			return fmt.Errorf("provider %q: issuer must be an https URL", p.ID)
		}
		if p.ClientID == "" || p.ClientSecretFile == "" {
			return fmt.Errorf("provider %q: client_id and client_secret_file are required", p.ID)
		}
		if p.DisplayName == "" {
			p.DisplayName = p.ID
		}
		if len(p.Scopes) == 0 {
			p.Scopes = []string{"openid", "email", "profile"}
		}
		if !slices.Contains(p.Scopes, "openid") {
			return fmt.Errorf("provider %q: scopes must include openid", p.ID)
		}
		if len(p.AllowedGroups) > 0 && p.GroupsClaim == "" {
			p.GroupsClaim = "groups"
		}
		for j, d := range p.AllowedEmailDomains {
			d = strings.ToLower(strings.TrimSpace(d))
			if d == "" || strings.ContainsAny(d, "@ /") {
				return fmt.Errorf("provider %q: invalid email domain %q", p.ID, p.AllowedEmailDomains[j])
			}
			p.AllowedEmailDomains[j] = d
		}
	}
	return nil
}

// Resource is the canonical URI of the MCP endpoint, the audience of every
// access token this server issues (RFC 8707).
func (c *Config) Resource() string { return c.Issuer + "/mcp" }

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
