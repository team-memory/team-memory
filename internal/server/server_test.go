package server

import (
	"net/http/httptest"
	"testing"
)

func TestClientAddr(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.42.0.0/16, 192.168.1.5")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, peer string
		xff        []string
		want       string
	}{
		{"no proxy", "203.0.113.7:5000", nil, "203.0.113.7"},
		{"untrusted peer ignores header", "203.0.113.7:5000", []string{"198.51.100.1"}, "203.0.113.7"},
		{"trusted proxy", "10.42.0.9:5000", []string{"198.51.100.1"}, "198.51.100.1"},
		{"spoofed left entries are ignored", "10.42.0.9:5000", []string{"1.1.1.1, 198.51.100.1"}, "198.51.100.1"},
		{"chain of trusted proxies", "10.42.0.9:5000", []string{"198.51.100.1, 192.168.1.5"}, "198.51.100.1"},
		{"several headers", "10.42.0.9:5000", []string{"1.1.1.1", "198.51.100.1"}, "198.51.100.1"},
		{"malformed falls back to the peer", "10.42.0.9:5000", []string{"not-an-ip"}, "10.42.0.9"},
		{"no header", "10.42.0.9:5000", nil, "10.42.0.9"},
		{"ipv6", "[2001:db8::1]:5000", []string{"198.51.100.1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		for _, h := range c.xff {
			r.Header.Add("X-Forwarded-For", h)
		}
		if got := ClientAddr(r, trusted); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := ClientAddr(httptest.NewRequest("GET", "/", nil), nil); got != "192.0.2.1" {
		t.Errorf("no trusted proxies: %q", got)
	}
	if _, err := ParseTrustedProxies("10.0.0.0/33"); err == nil {
		t.Error("bad CIDR accepted")
	}
}
