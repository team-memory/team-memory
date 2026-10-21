package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// client is a public MCP client, registered dynamically or described by a
// Client ID Metadata Document.
type client struct {
	id           string
	name         string
	clientURI    string
	redirectURIs []string
	source       string // "cimd" or "dcr"
	loginURL     string
}

func (c *client) GetID() string                    { return c.id }
func (c *client) RedirectURIs() []string           { return c.redirectURIs }
func (c *client) PostLogoutRedirectURIs() []string { return nil }
func (c *client) AuthMethod() oidc.AuthMethod      { return oidc.AuthMethodNone }
func (c *client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (c *client) LoginURL(id string) string {
	return c.loginURL + "?authRequestID=" + url.QueryEscape(id)
}
func (c *client) AccessTokenType() op.AccessTokenType  { return op.AccessTokenTypeJWT }
func (c *client) IDTokenLifetime() time.Duration       { return 5 * time.Minute }
func (c *client) DevMode() bool                        { return false }
func (c *client) IDTokenUserinfoClaimsAssertion() bool { return false }
func (c *client) ClockSkew() time.Duration             { return 0 }
func (c *client) IsScopeAllowed(scope string) bool     { return scope == ScopeMemory }
func (c *client) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken}
}

// ApplicationType is native for every MCP client. op then accepts an https
// redirect URI by exact match and a loopback one with any port (RFC 8252
// section 7.3), which CLI clients need.
func (c *client) ApplicationType() op.ApplicationType { return op.ApplicationTypeNative }

func (c *client) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return func(s []string) []string { return s }
}

func (c *client) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return func(s []string) []string { return s }
}

// clientRegistry resolves client IDs: an https URL is a Client ID Metadata
// Document, anything else a dynamically registered client.
type clientRegistry struct {
	cfg      Registration
	loginURL string
	fetch    *http.Client
	now      func() time.Time

	mu    sync.Mutex
	dcr   map[string]*client
	cache map[string]cachedClient
}

type cachedClient struct {
	c       *client
	expires time.Time
}

func newClientRegistry(cfg Registration, loginURL string) *clientRegistry {
	return &clientRegistry{
		cfg: cfg, loginURL: loginURL, fetch: publicOnlyHTTPClient(), now: time.Now,
		dcr: map[string]*client{}, cache: map[string]cachedClient{},
	}
}

func (r *clientRegistry) get(ctx context.Context, id string) (*client, error) {
	if strings.HasPrefix(id, "https://") {
		if !r.cfg.ClientIDMetadataDocuments {
			return nil, errors.New("client ID metadata documents are disabled")
		}
		return r.metadataDocument(ctx, id)
	}
	if !r.cfg.Dynamic {
		return nil, errNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.dcr[id]
	if !ok {
		return nil, errNotFound
	}
	return c, nil
}

// clientMetadata is the subset of RFC 7591 client metadata Team Memory
// reads, shared by registration requests and metadata documents.
type clientMetadata struct {
	ClientID                string   `json:"client_id,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

// validate checks metadata. Unless remote redirects are allowed, https
// redirect URIs on non-loopback hosts are dropped, and metadata left with
// none is refused.
func (m *clientMetadata) validate(allowRemote bool) error {
	if len(m.RedirectURIs) == 0 || len(m.RedirectURIs) > 10 {
		return errors.New("redirect_uris must list 1 to 10 URIs")
	}
	var kept []string
	for _, u := range m.RedirectURIs {
		if err := validRedirectURI(u); err != nil {
			return err
		}
		if pu, _ := url.Parse(u); allowRemote || isLoopbackHost(pu.Hostname()) {
			kept = append(kept, u)
		}
	}
	if len(kept) == 0 {
		return errors.New("only loopback redirect URIs (http://127.0.0.1, http://localhost) are accepted by this server")
	}
	m.RedirectURIs = kept
	for _, g := range m.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return fmt.Errorf("grant type %q is not supported", g)
		}
	}
	for _, rt := range m.ResponseTypes {
		if rt != "code" {
			return fmt.Errorf("response type %q is not supported", rt)
		}
	}
	if len([]rune(m.ClientName)) > 200 {
		return errors.New("client_name is too long")
	}
	if m.ClientURI != "" {
		if u, err := url.Parse(m.ClientURI); err != nil || u.Scheme != "https" {
			m.ClientURI = ""
		}
	}
	return nil
}

// validRedirectURI accepts https URIs and http loopback URIs, without
// fragments (OAuth 2.1 section 2.3, MCP communication security).
func validRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil || u.Host == "" {
		return fmt.Errorf("invalid redirect URI %q", raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return fmt.Errorf("redirect URI %q must use https or a loopback http address", raw)
	}
	return nil
}

// register handles RFC 7591 dynamic client registration. Every client is
// registered as public (token_endpoint_auth_method "none"), whatever the
// request asks for; RFC 7591 section 3.2.1 lets the server replace values.
func (r *clientRegistry) register(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var m clientMetadata
	dec := json.NewDecoder(io.LimitReader(req.Body, 16<<10))
	if err := dec.Decode(&m); err != nil {
		registrationError(w, "invalid_client_metadata", "request body must be client metadata JSON")
		return
	}
	if err := m.validate(r.cfg.AllowRemoteRedirectURIs); err != nil {
		registrationError(w, "invalid_redirect_uri", err.Error())
		return
	}
	c := &client{
		id: "tmc_" + randomString(18), name: m.ClientName, clientURI: m.ClientURI,
		redirectURIs: m.RedirectURIs, source: "dcr", loginURL: r.loginURL,
	}
	r.mu.Lock()
	if len(r.dcr) >= maxDCRClients {
		r.mu.Unlock()
		registrationError(w, "invalid_client_metadata", "registration limit reached")
		return
	}
	r.dcr[c.id] = c
	r.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id":                  c.id,
		"client_id_issued_at":        r.now().Unix(),
		"client_name":                c.name,
		"redirect_uris":              c.redirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func registrationError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

const (
	cimdMaxBytes   = 64 << 10
	cimdMinTTL     = time.Minute
	cimdDefaultTTL = time.Hour
	cimdMaxTTL     = 24 * time.Hour
)

// metadataDocument fetches and validates a Client ID Metadata Document
// (draft-ietf-oauth-client-id-metadata-document-00).
func (r *clientRegistry) metadataDocument(ctx context.Context, id string) (*client, error) {
	r.mu.Lock()
	if c, ok := r.cache[id]; ok && r.now().Before(c.expires) {
		r.mu.Unlock()
		return c.c, nil
	}
	r.mu.Unlock()

	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" || u.Path == "/" ||
		u.Fragment != "" || u.User != nil || strings.Contains(u.Path, "/./") || strings.Contains(u.Path, "/../") {
		return nil, errors.New("client_id is not a valid metadata document URL")
	}
	if len(r.cfg.CIMDAllowedHosts) > 0 && !slices.Contains(r.cfg.CIMDAllowedHosts, strings.ToLower(u.Hostname())) {
		return nil, fmt.Errorf("client metadata host %q is not allowed", u.Hostname())
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.fetch.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch client metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch client metadata: HTTP %d", resp.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "application/json" {
		return nil, errors.New("client metadata is not application/json")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > cimdMaxBytes {
		return nil, errors.New("client metadata document is too large")
	}
	var m clientMetadata
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, errors.New("client metadata document is not valid JSON")
	}
	if m.ClientID != id {
		return nil, errors.New("client metadata client_id does not match its URL")
	}
	if m.ClientName == "" {
		return nil, errors.New("client metadata has no client_name")
	}
	if m.TokenEndpointAuthMethod != "" && m.TokenEndpointAuthMethod != "none" {
		return nil, fmt.Errorf("token_endpoint_auth_method %q is not supported", m.TokenEndpointAuthMethod)
	}
	if err := m.validate(r.cfg.AllowRemoteRedirectURIs); err != nil {
		return nil, err
	}
	c := &client{id: id, name: m.ClientName, clientURI: m.ClientURI, redirectURIs: m.RedirectURIs, source: "cimd", loginURL: r.loginURL}
	r.mu.Lock()
	if len(r.cache) >= maxDCRClients {
		r.cache = map[string]cachedClient{}
	}
	r.cache[id] = cachedClient{c: c, expires: r.now().Add(cacheTTL(resp.Header.Get("Cache-Control")))}
	r.mu.Unlock()
	return c, nil
}

func cacheTTL(cc string) time.Duration {
	for _, d := range strings.Split(cc, ",") {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "no-store" || d == "no-cache" {
			return cimdMinTTL
		}
		if v, ok := strings.CutPrefix(d, "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				return min(max(time.Duration(n)*time.Second, cimdMinTTL), cimdMaxTTL)
			}
		}
	}
	return cimdDefaultTTL
}

// publicOnlyHTTPClient refuses to connect to loopback, private, link-local
// and other non-public addresses, after DNS resolution, so a metadata URL
// cannot reach internal services (SSRF). It follows no redirects.
func publicOnlyHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !publicIP(net.ParseIP(host)) {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        10,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// nonPublic lists global-unicast ranges that are still not public. 100.64/10
// is RFC 6598 carrier-grade NAT space, also used by overlay networks such as
// Tailscale. The IPv6 translation prefixes (NAT64, 6to4, Teredo) embed an
// IPv4 address, so in an IPv6-only cluster with NAT64 an AAAA record such
// as 64:ff9b::a00:1 reaches 10.0.0.1.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
