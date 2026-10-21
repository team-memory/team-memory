package memory

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	MaxTitleLen      = 200
	MaxContentLen    = 20000
	MaxRationaleLen  = 10000
	MaxSourceURILen  = 2048
	MaxChangeNoteLen = 1000
	MaxOverrides     = 10
	DefaultLimit     = 10
	MaxLimit         = 50
	MaxQueryLen      = 500
)

var (
	slugRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	levelRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
)

// ParseKind validates a memory kind.
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.TrimSpace(s))
	if !slices.Contains(Kinds, k) {
		return "", invalidf("kind must be one of fact, decision, convention, lesson, context")
	}
	return k, nil
}

// ParseKinds validates a kind filter; an empty filter means all kinds.
func ParseKinds(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		k, err := ParseKind(s)
		if err != nil {
			return nil, err
		}
		out = append(out, string(k))
	}
	return out, nil
}

func checkText(field, value string, min, max int) error {
	if !utf8.ValidString(value) {
		return invalidf("%s must be valid UTF-8", field)
	}
	if strings.ContainsRune(value, 0) {
		return invalidf("%s must not contain NUL bytes", field)
	}
	n := utf8.RuneCountInString(value)
	if n < min {
		return invalidf("%s is required", field)
	}
	if n > max {
		return invalidf("%s exceeds %d characters", field, max)
	}
	return nil
}

// ValidateSourceURI checks syntax and scheme only. It never fetches the URI,
// so a valid URI does not mean the source exists or supports the memory.
func ValidateSourceURI(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > MaxSourceURILen {
		return invalidf("source_uri exceeds %d characters", MaxSourceURILen)
	}
	u, err := url.Parse(s)
	if err != nil {
		return invalidf("source_uri is not a valid URI")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return invalidf("source_uri must use http or https")
	}
	if u.Host == "" {
		return invalidf("source_uri must be absolute and include a host")
	}
	if u.User != nil {
		return invalidf("source_uri must not contain credentials")
	}
	return nil
}

func validSlug(s string) error {
	if !slugRe.MatchString(s) {
		return invalidf("slug must be lowercase letters, digits and hyphens (max 63)")
	}
	return nil
}

func validLevel(s string) error {
	if !levelRe.MatchString(s) {
		return invalidf("level must be lowercase letters, digits, '_' or '-' (max 40)")
	}
	return nil
}

// ClampLimit applies the default and maximum result limits.
func ClampLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	return min(n, MaxLimit)
}
