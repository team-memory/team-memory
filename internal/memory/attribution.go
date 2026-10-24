package memory

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"
)

// Values of Attribution.ClientSource.
const (
	ClientSourceMCPClientInfo = "mcp_client_info"
	ClientSourceUserAgent     = "user_agent"
)

// Attribution is a bounded snapshot supplied by transport/authentication adapters.
// Reported client names are descriptive, never authorization evidence.
type Attribution struct {
	Channel         string `json:"channel,omitempty"`
	AuthMethod      string `json:"auth_method,omitempty"`
	ClientName      string `json:"client_name,omitempty"`
	ClientVersion   string `json:"client_version,omitempty"`
	ClientSource    string `json:"client_source,omitempty"`
	ApplicationName string `json:"application_name,omitempty"`
	OAuthClientID   string `json:"oauth_client_id,omitempty"`
	CredentialID    string `json:"credential_id,omitempty"`
	CredentialLabel string `json:"credential_label,omitempty"`
	Tool            string `json:"tool,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
	ServerVersion   string `json:"server_version,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
}
type attributionKey struct{}

func WithAttribution(ctx context.Context, a Attribution) context.Context {
	for _, p := range []*string{&a.Channel, &a.AuthMethod, &a.ClientName, &a.ClientVersion, &a.ClientSource, &a.ApplicationName, &a.OAuthClientID, &a.CredentialID, &a.CredentialLabel, &a.Tool, &a.ProtocolVersion, &a.ServerVersion, &a.RequestID, &a.TraceID} {
		*p = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, strings.ToValidUTF8(*p, ""))
		rs := []rune(*p)
		limit := 200
		if p == &a.OAuthClientID {
			limit = 8192
		}
		if len(rs) > limit {
			*p = string(rs[:limit])
		}
	}
	return context.WithValue(ctx, attributionKey{}, a)
}
func AttributionFrom(ctx context.Context) Attribution {
	a, _ := ctx.Value(attributionKey{}).(Attribution)
	return a
}

// Public excludes credential references and operational correlation identifiers.
func (a Attribution) Public() Attribution {
	a.OAuthClientID = ""
	a.CredentialID = ""
	a.CredentialLabel = ""
	a.RequestID = ""
	a.TraceID = ""
	return a
}
func attributionJSON(ctx context.Context) []byte {
	b, _ := json.Marshal(AttributionFrom(ctx).Public())
	return b
}
func decodeAttribution(b []byte) Attribution {
	var a Attribution
	_ = json.Unmarshal(b, &a)
	return a.Public()
}
func (a Attribution) Summary() string {
	if a.ClientName != "" {
		s := a.ClientName
		if a.ClientVersion != "" {
			s += " · " + a.ClientVersion
		}
		return s + " (" + a.ClientNameLabel() + ")"
	}
	if a.ApplicationName != "" {
		return a.ApplicationName + " (OAuth application)"
	}
	if a.Channel != "" {
		return a.Channel + " · client not recorded"
	}
	return "Not recorded"
}

// ClientNameLabel says how ClientName was obtained.
func (a Attribution) ClientNameLabel() string {
	if a.ClientSource == ClientSourceUserAgent {
		return "HTTP user agent"
	}
	return "client-reported"
}

func withPublicAttribution(ctx context.Context, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range extra {
		out[k] = v
	}
	out["attribution"] = AttributionFrom(ctx).Public()
	return out
}
