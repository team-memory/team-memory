package secrets

import "testing"

// Test values are assembled at run time so this file does not itself trip
// secret scanners.
func join(parts ...string) string {
	s := ""
	for _, p := range parts {
		s += p
	}
	return s
}

func TestScanDetects(t *testing.T) {
	cases := map[string]string{
		"private_key":         join("-----BEGIN ", "RSA PRIVATE KEY-----\nMIIE..."),
		"aws_access_key_id":   "key id " + join("AKIA", "Q3EGRZ7J4K2XMPLT"),
		"github_token":        "use " + join("ghp_", "a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q7R8"),
		"slack_token":         join("xoxb-", "1234567890-abcdefghij"),
		"google_api_key":      join("AIza", "SyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q"),
		"stripe_secret_key":   join("sk_live_", "51Hc8aXbQ2wErTyUiOpAsDfGh"),
		"team_memory_token":   join("tmpat_", "Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eA"),
		"jwt":                 join("eyJhbGciOiJFUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", ".", "c2lnbmF0dXJlLXNpZ25hdHVyZS1zaWduYXR1cmU"),
		"url_credentials":     join("postgres://app:", "Qz8!vR2#mK9pL4xT", "@db.internal:5432/app"),
		"password_assignment": join("DB_PASSWORD=", "h7Gq2LmZ9xVb4Rt1Nc8K"),
	}
	for rule, text := range cases {
		got := Scan(map[string]string{"content": text})
		found := false
		for _, f := range got {
			if f.Rule == rule && f.Field == "content" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: not detected in %q (got %v)", rule, text, got)
		}
	}
}

func TestScanIgnoresOrdinaryText(t *testing.T) {
	for _, text := range []string{
		"We chose PostgreSQL for payments-api because we need ACID transactions.",
		"Set password: <your-password> in the deployment secret, never in code.",
		"password = changeme_in_production_please",
		"The api_key setting is read from TEAM_MEMORY_API_KEY_FILE.",
		"See https://github.com/acme/payments/pull/42 for the discussion.",
		"Token rotation runs every 90 days; tokens start with tmpat_.",
		"Use postgres://user@localhost/db for local development.",
	} {
		if got := Scan(map[string]string{"content": text}); len(got) > 0 {
			t.Errorf("false positive on %q: %v", text, got)
		}
	}
}
