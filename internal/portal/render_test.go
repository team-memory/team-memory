package portal

import (
	"strings"
	"testing"
)

func TestMarkdownNeutralizesHostileInput(t *testing.T) {
	r := newRenderer([]string{"jira", "javascript"})
	cases := []struct {
		in       string
		contains []string
		absent   []string
	}{
		{`<script>alert(1)</script>`, []string{"&lt;script&gt;"}, []string{"<script"}},
		{`<img src=x onerror=alert(1)>`, []string{"&lt;img"}, []string{"<img"}},
		{`[x](javascript:alert(1))`, []string{"x (javascript:alert(1)"}, []string{"href"}},
		{`[x](JaVaScRiPt:alert(1))`, nil, []string{"href"}},
		{`[x](data:text/html;base64,AAAA)`, nil, []string{"href"}},
		{`[x]( javascript:alert(1))`, nil, []string{"href"}},
		{"[x](java\tscript:alert(1))", nil, []string{"href"}},
		{`[x](/relative/path)`, nil, []string{"href"}},
		{`[x](//evil.example/x)`, nil, []string{"href"}},
		{`[x](https://user:pw@example.com/)`, nil, []string{"href"}},
		{`[x](https://example.com/a?b="><script>)`, nil, []string{"<script"}},
		{`[ok](https://example.com/ok)`, []string{`href="https://example.com/ok"`, `rel="noopener noreferrer nofollow"`}, nil},
		{`[mail](mailto:a@example.com)`, []string{`href="mailto:a@example.com"`}, nil},
		{`[t](jira:PROJ-1)`, []string{`href="jira:PROJ-1"`}, nil},
		{`![pic](https://evil.example/track.png)`, []string{"[image: pic]"}, []string{"<img", "track.png"}},
		{"```\n<b>x</b>\n```", []string{"<pre><code>&lt;b&gt;x&lt;/b&gt;"}, []string{"<b>"}},
		{"`<i>`", []string{"<code>&lt;i&gt;</code>"}, []string{"<i>"}},
		{"snake_case_name and **bold** and *em*", []string{"snake_case_name", "<strong>bold</strong>", "<em>em</em>"}, nil},
		{"# Title\n- a\n- b\n\n1. x\n2. y\n\n> quote", []string{"<h3>Title</h3>", "<ul>", "<ol>", "<blockquote>"}, nil},
		{`"><svg onload=alert(1)>`, []string{"&#34;&gt;&lt;svg"}, []string{"<svg"}},
	}
	for _, c := range cases {
		got := string(r.Markdown(c.in))
		for _, s := range c.contains {
			if !strings.Contains(got, s) {
				t.Errorf("%q: output %q lacks %q", c.in, got, s)
			}
		}
		for _, s := range c.absent {
			if strings.Contains(got, s) {
				t.Errorf("%q: output %q contains %q", c.in, got, s)
			}
		}
	}
}

func TestSchemeAllowlistNeverAllowsScriptSchemes(t *testing.T) {
	r := newRenderer([]string{"javascript", "data", "vbscript", "file"})
	for _, u := range []string{"javascript:alert(1)", "data:text/html,x", "vbscript:x", "file:///etc/passwd"} {
		if _, ok := r.safeURL(u); ok {
			t.Errorf("%s allowed", u)
		}
	}
}

func TestLineDiff(t *testing.T) {
	d, big := lineDiff("a\nb\nc", "a\nB\nc\nd")
	if big {
		t.Fatal("too large")
	}
	var got []string
	for _, l := range d {
		got = append(got, l.Op+l.Text)
	}
	if strings.Join(got, "|") != " a|-b|+B| c|+d" {
		t.Errorf("diff = %v", got)
	}
}

func TestCombinedPayload(t *testing.T) {
	r := newRenderer(nil)
	got := string(r.Markdown(`<img src=x onerror=alert(1)><script>alert(2)</script> [click](javascript:alert(3)) [d](data:text/html;base64,AAAA) ![pic](https://evil.example/track.png) [ok](https://example.com/ok) <a href="javascript:alert(4)">x</a> ` + "`<b>`"))
	t.Log(got)
	if strings.Contains(got, "track.png") && !strings.Contains(got, "[image: pic]") {
		t.Error("image")
	}
}
