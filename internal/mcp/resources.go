package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/team-memory/team-memory/internal/memory"
)

// Resource views. A scope path may contain slashes, so each view has its own
// template instead of one ambiguous {+path}/{view} template.
var views = []struct {
	name, title, description string
	kinds                    []string
}{
	{"overview", "Scope overview", "Visible scopes and recent memories for a scope, including inherited ones.", nil},
	{"decisions", "Decisions", "Active decisions visible from a scope.", []string{"decision"}},
	{"conventions", "Conventions", "Active conventions visible from a scope.", []string{"convention"}},
	{"recent", "Recent memories", "Recently changed memories visible from a scope.", nil},
}

func registerResources(s *mcp.Server, svc *memory.Service) {
	for _, v := range views {
		h := resourceHandler(svc, v.name, v.kinds)
		for _, prefix := range []string{"memory://scopes/", "memory://projects/"} {
			s.AddResourceTemplate(&mcp.ResourceTemplate{
				Name:        strings.TrimSuffix(strings.TrimPrefix(prefix, "memory://"), "/") + "-" + v.name,
				Title:       v.title,
				Description: v.description,
				URITemplate: prefix + "{+scope}/" + v.name,
				MIMEType:    "application/json",
			}, h)
		}
	}
}

// parseResourceURI returns the scope path from memory://scopes/<path>/<view>
// (or the memory://projects/ alias).
func parseResourceURI(uri, view string) (string, bool) {
	for _, prefix := range []string{"memory://scopes/", "memory://projects/"} {
		if rest, ok := strings.CutPrefix(uri, prefix); ok {
			path, ok := strings.CutSuffix(rest, "/"+view)
			return path, ok && path != ""
		}
	}
	return "", false
}

func resourceHandler(svc *memory.Service, view string, kinds []string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		scope, ok := parseResourceURI(uri, view)
		if !ok {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		p, err := principalFrom(req.Extra)
		if err != nil {
			return nil, err
		}
		body := map[string]any{"scope": scope, "view": view, "notice": UntrustedNotice,
			"note": "inherited=true marks memories owned by ancestor scopes"}
		if view == "overview" {
			vs, err := svc.VisibleScopes(ctx, p, scope)
			if err != nil {
				return nil, mcp.ResourceNotFoundError(uri)
			}
			body["visible_scopes"] = vs
		}
		res, err := svc.Recent(ctx, p, scope, kinds, memory.MaxLimit)
		if err != nil {
			// Hide existence of scopes the caller cannot read.
			return nil, mcp.ResourceNotFoundError(uri)
		}
		body["memories"] = res
		b, err := json.MarshalIndent(body, "", "  ")
		if err != nil {
			return nil, err
		}
		out := &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(b),
			Meta: mcp.Meta{"team-memory/untrusted-content": true}}}}
		return out, nil
	}
}
