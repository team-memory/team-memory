package portal

import (
	"html"
	"html/template"
	"net/url"
	"strings"
	"unicode"
)

// Memory content is untrusted data written by agents. The renderer never
// passes input through: every character is HTML-escaped, and only the
// elements this file emits can appear in the output (p, h3-h5, ul, ol, li,
// blockquote, pre, code, em, strong, hr, br, a). There is no raw HTML, no
// attribute from input other than a vetted href, and images are never
// loaded: they render as a visible placeholder. Links are limited to an
// allowlist of schemes and always carry rel="noopener noreferrer nofollow".

type renderer struct {
	schemes map[string]bool
}

func newRenderer(extraSchemes []string) *renderer {
	r := &renderer{schemes: map[string]bool{"https": true, "http": true, "mailto": true}}
	for _, s := range extraSchemes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && s != "javascript" && s != "data" && s != "vbscript" && s != "file" {
			r.schemes[s] = true
		}
	}
	return r
}

// safeURL returns a normalized URL if its scheme is allowed.
func (r *renderer) safeURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", false
	}
	for _, c := range raw {
		if unicode.IsControl(c) || unicode.IsSpace(c) {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || !r.schemes[strings.ToLower(u.Scheme)] {
		return "", false
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		if u.Host == "" || u.User != nil {
			return "", false
		}
	}
	return u.String(), true
}

// Link renders a source URI: an anchor if its scheme is allowed, else text.
func (r *renderer) Link(raw string) template.HTML {
	if raw == "" {
		return ""
	}
	if u, ok := r.safeURL(raw); ok {
		return template.HTML(`<a href="` + html.EscapeString(u) + `" rel="noopener noreferrer nofollow">` + html.EscapeString(raw) + `</a>`) //nolint:gosec // escaped, scheme allowlisted
	}
	return template.HTML(html.EscapeString(raw)) //nolint:gosec // escaped
}

// Markdown renders a small, safe subset of Markdown.
func (r *renderer) Markdown(src string) template.HTML {
	if len(src) > 200_000 {
		src = src[:200_000]
	}
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(src, "\n")
	var b strings.Builder
	var para []string
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + r.inline(strings.Join(para, "\n")) + "</p>\n")
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trim := strings.TrimSpace(line)
		switch {
		case trim == "":
			flush()
		case strings.HasPrefix(trim, "```"):
			flush()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case strings.HasPrefix(trim, "#"):
			flush()
			n := 0
			for n < len(trim) && trim[n] == '#' {
				n++
			}
			if n <= 6 && n < len(trim) && trim[n] == ' ' {
				level := min(max(n+2, 3), 5) // page headings are h1/h2
				b.WriteString("<h" + string(rune('0'+level)) + ">" + r.inline(strings.TrimSpace(trim[n:])) + "</h" + string(rune('0'+level)) + ">\n")
			} else {
				para = append(para, line)
			}
		case trim == "---" || trim == "***":
			flush()
			b.WriteString("<hr>\n")
		case strings.HasPrefix(trim, "> "):
			flush()
			var q []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				q = append(q, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
			}
			i--
			b.WriteString("<blockquote><p>" + r.inline(strings.Join(q, "\n")) + "</p></blockquote>\n")
		case isBullet(trim) || isOrdered(trim):
			flush()
			tag := "ul"
			if isOrdered(trim) {
				tag = "ol"
			}
			b.WriteString("<" + tag + ">\n")
			for ; i < len(lines); i++ {
				t := strings.TrimSpace(lines[i])
				if (tag == "ul" && !isBullet(t)) || (tag == "ol" && !isOrdered(t)) {
					break
				}
				b.WriteString("<li>" + r.inline(listText(t)) + "</li>\n")
			}
			i--
			b.WriteString("</" + tag + ">\n")
		default:
			para = append(para, line)
		}
	}
	flush()
	return template.HTML(b.String()) //nolint:gosec // every byte is escaped or emitted by this function
}

func isBullet(t string) bool {
	return len(t) > 2 && (t[0] == '-' || t[0] == '*' || t[0] == '+') && t[1] == ' '
}

func isOrdered(t string) bool {
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i > 0 && i < 10 && i+1 < len(t) && t[i] == '.' && t[i+1] == ' '
}

func listText(t string) string {
	if isBullet(t) {
		return strings.TrimSpace(t[2:])
	}
	i := strings.Index(t, ". ")
	return strings.TrimSpace(t[i+2:])
}

// inline renders code spans, links, images (as placeholders), strong and
// emphasis. Text is escaped as it is copied, never afterwards.
func (r *renderer) inline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteString("<code>" + html.EscapeString(s[i+1:i+1+j]) + "</code>")
				i += j + 2
				continue
			}
		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			if text, _, n, ok := parseLink(s[i+1:]); ok {
				b.WriteString("[image: " + html.EscapeString(text) + "]")
				i += 1 + n
				continue
			}
		case c == '[':
			if text, target, n, ok := parseLink(s[i:]); ok {
				if u, safe := r.safeURL(target); safe {
					b.WriteString(`<a href="` + html.EscapeString(u) + `" rel="noopener noreferrer nofollow">` + r.inline(text) + `</a>`)
				} else {
					b.WriteString(html.EscapeString(text) + " (" + html.EscapeString(target) + ")")
				}
				i += n
				continue
			}
		case c == '*' && strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				b.WriteString("<strong>" + r.inline(s[i+2:i+2+j]) + "</strong>")
				i += j + 4
				continue
			}
		case c == '*' || (c == '_' && (i == 0 || !isWordByte(s[i-1]))):
			if j := strings.IndexByte(s[i+1:], c); j > 0 && s[i+1] != ' ' {
				b.WriteString("<em>" + r.inline(s[i+1:i+1+j]) + "</em>")
				i += j + 2
				continue
			}
		case c == '\n':
			b.WriteString("<br>")
			i++
			continue
		}
		// Copy one rune, escaped.
		j := i + 1
		for j < len(s) && s[j]&0xC0 == 0x80 {
			j++
		}
		b.WriteString(html.EscapeString(s[i:j]))
		i = j
	}
	return b.String()
}

// parseLink parses "[text](target)" at the start of s and returns the
// consumed length.
func parseLink(s string) (text, target string, n int, ok bool) {
	depth := 0
	end := -1
	for i := 0; i < len(s) && i < 500; i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 || end+1 >= len(s) || s[end+1] != '(' {
		return "", "", 0, false
	}
	close := strings.IndexByte(s[end+2:], ')')
	if close < 0 || close > 2048 {
		return "", "", 0, false
	}
	return s[1:end], strings.TrimSpace(s[end+2 : end+2+close]), end + 2 + close + 1, true
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}
