// Package secrets detects common credential formats in text that is about
// to be stored as shared memory. It is a tripwire for accidents, not a
// guarantee: it cannot recognise every secret, and a determined writer can
// evade it. See docs/security/threat-model.md.
package secrets

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Finding names a detected credential type and the field it was found in.
// It never includes the matched text, so findings are safe to log, audit
// and return to clients.
type Finding struct {
	Field string `json:"field"`
	Rule  string `json:"rule"`
}

type rule struct {
	name string
	re   *regexp.Regexp
	// entropy, when > 0, is the minimum Shannon entropy (bits per
	// character) of capture group 1, to skip placeholders like "changeme".
	entropy float64
}

// Rules are deliberately specific: false positives block legitimate
// memories, so generic "long random string" detection is limited to
// assignments that name a secret.
var rules = []rule{
	{name: "private_key", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED |PGP )?PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "aws_access_key_id", re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`)},
	{name: "aws_secret_access_key", re: regexp.MustCompile(`(?i)aws.{0,20}secret.{0,20}[:=]\s*["']?([A-Za-z0-9/+=]{40})\b`), entropy: 4.0},
	{name: "github_token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{60,}\b`)},
	{name: "gitlab_token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{name: "slack_token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`)},
	{name: "slack_webhook", re: regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Za-z0-9_]+/B[A-Za-z0-9_]+/[A-Za-z0-9_]+`)},
	{name: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{name: "stripe_secret_key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{20,}\b`)},
	{name: "openai_api_key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{32,}\b`)},
	{name: "anthropic_api_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{32,}\b`)},
	{name: "npm_token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{name: "team_memory_token", re: regexp.MustCompile(`\btm(?:pat|rt)_[A-Za-z0-9_-]{30,}\b`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{20,}\b`)},
	{name: "url_credentials", re: regexp.MustCompile(`\b[a-z][a-z0-9+.-]{1,20}://[^\s/:@]{1,64}:([^\s/@]{6,})@[^\s/]+`), entropy: 2.5},
	{name: "password_assignment", re: regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret)\s*[:=]\s*["']?([^\s"'<>]{12,})`), entropy: 3.5},
}

// Scan checks named fields and returns one finding per (field, rule),
// sorted for stable output.
func Scan(fields map[string]string) []Finding {
	var out []Finding
	for field, text := range fields {
		if text == "" {
			continue
		}
		for _, r := range rules {
			if match(r, text) {
				out = append(out, Finding{Field: field, Rule: r.name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

func match(r rule, text string) bool {
	if r.entropy <= 0 {
		return r.re.MatchString(text)
	}
	for _, m := range r.re.FindAllStringSubmatch(text, -1) {
		v := m[0]
		if len(m) > 1 && m[1] != "" {
			v = m[1]
		}
		if !placeholder(v) && entropy(v) >= r.entropy {
			return true
		}
	}
	return false
}

// placeholder recognises obvious examples so documentation about
// configuration ("password: <your-password>") is not rejected.
func placeholder(v string) bool {
	l := strings.ToLower(v)
	for _, p := range []string{"example", "placeholder", "changeme", "your", "xxxx", "****", "redacted", "${", "{{", "env("} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]float64{}
	n := 0.0
	for _, c := range s {
		counts[c]++
		n++
	}
	h := 0.0
	for _, c := range counts {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}
