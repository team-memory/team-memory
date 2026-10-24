package memory

import (
	"context"
	"strings"
	"testing"
)

func TestAttributionBoundsAndRedaction(t *testing.T) {
	ctx := WithAttribution(context.Background(), Attribution{ClientName: strings.Repeat("界", 300) + "\n", CredentialID: "secret-reference", CredentialLabel: "private label", OAuthClientID: "app-id", RequestID: "request", TraceID: "trace"})
	a := AttributionFrom(ctx)
	if len([]rune(a.ClientName)) != 200 || strings.Contains(a.ClientName, "\n") {
		t.Fatal("unbounded client")
	}
	public := string(attributionJSON(ctx))
	for _, s := range []string{"secret-reference", "private label", "app-id", "request", "trace"} {
		if strings.Contains(public, s) {
			t.Fatalf("public attribution leaked %s", s)
		}
	}
	if a.CredentialID != "secret-reference" {
		t.Fatal("audit attribution lost")
	}
	if (Attribution{}).Summary() != "Not recorded" {
		t.Fatal("legacy attribution inferred")
	}
}
