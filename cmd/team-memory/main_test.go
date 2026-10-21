package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONRPCMethod(t *testing.T) {
	big := strings.Repeat("x", 70<<10)
	cases := []struct {
		name, body, want string
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_get"}}`, "tools/call"},
		{"method after params", `{"jsonrpc":"2.0","params":{"a":[1,{"b":"c"}]},"method":"tools/list"}`, "tools/list"},
		{"method before a truncated value", (`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"content":"` + big + `"}}`)[:prefixCaptureLimit], "tools/call"},
		{"method after a truncated value", (`{"params":{"content":"` + big + `"},"method":"tools/call"}`)[:prefixCaptureLimit], ""},
		{"response has no method", `{"jsonrpc":"2.0","id":1,"result":{}}`, ""},
		{"keys match exactly", `{"METHOD":"tools/call"}`, ""},
		{"non-string method", `{"method":42}`, ""},
		{"overlong method", `{"method":"` + strings.Repeat("m", 65) + `"}`, ""},
		{"not json", `hello`, ""},
		{"empty", ``, ""},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jsonRPCMethod([]byte(c.body)); got != c.want {
				t.Fatalf("method = %q, want %q", got, c.want)
			}
		})
	}
}

// loggedMethod runs one request through logRequests and returns the logged
// mcp_method.
func loggedMethod(t *testing.T, r *http.Request, next http.HandlerFunc) string {
	t.Helper()
	var out bytes.Buffer
	logRequests(slog.New(slog.NewJSONHandler(&out, nil)), next).ServeHTTP(httptest.NewRecorder(), r)
	var entry struct {
		MCPMethod string `json:"mcp_method"`
	}
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("decode log line %q: %v", out.String(), err)
	}
	return entry.MCPMethod
}

func readAllHandler(t *testing.T, want string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if string(got) != want {
			t.Errorf("handler read %d bytes, want %d", len(got), len(want))
		}
	}
}

func TestLogRequestsMethod(t *testing.T) {
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_get"}}`

	t.Run("body method logged after the handler reads it", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(call))
		if got := loggedMethod(t, r, readAllHandler(t, call)); got != "tools/call" {
			t.Fatalf("mcp_method = %q", got)
		}
	})

	t.Run("large body passes through and still logs the method", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"content":"` + strings.Repeat("x", 200<<10) + `"}}`
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		if got := loggedMethod(t, r, readAllHandler(t, body)); got != "tools/call" {
			t.Fatalf("mcp_method = %q", got)
		}
	})

	t.Run("unread body is not read ahead and the request body is untouched", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(call))
		original := r.Body
		got := loggedMethod(t, r, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		if got != "" {
			t.Fatalf("mcp_method = %q, want empty for an unread body", got)
		}
		if r.Body != original {
			t.Fatal("outer request body was replaced")
		}
		if rest, _ := io.ReadAll(r.Body); string(rest) != call {
			t.Fatal("body was consumed before the handler read it")
		}
	})

	t.Run("header takes precedence and the body is not wrapped", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(call))
		r.Header.Set("Mcp-Method", "tools/list")
		original := r.Body
		got := loggedMethod(t, r, func(_ http.ResponseWriter, inner *http.Request) {
			if inner.Body != original {
				t.Error("body wrapped despite Mcp-Method header")
			}
		})
		if got != "tools/list" {
			t.Fatalf("mcp_method = %q", got)
		}
	})

	t.Run("GET is not wrapped", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		original := r.Body
		loggedMethod(t, r, func(_ http.ResponseWriter, inner *http.Request) {
			if inner.Body != original {
				t.Error("GET body wrapped")
			}
		})
	})

	t.Run("NoBody keeps its identity", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Body = http.NoBody
		got := loggedMethod(t, r, func(_ http.ResponseWriter, inner *http.Request) {
			if inner.Body != http.NoBody {
				t.Error("NoBody replaced")
			}
		})
		if got != "" {
			t.Fatalf("mcp_method = %q", got)
		}
	})

	t.Run("nil body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Body = nil
		loggedMethod(t, r, func(_ http.ResponseWriter, inner *http.Request) {
			if inner.Body != nil {
				t.Error("nil body replaced")
			}
		})
	})

	t.Run("read error reaches the handler", func(t *testing.T) {
		errBroken := errors.New("broken")
		body := io.MultiReader(strings.NewReader(`{"method":"tools/call",`), errReader{errBroken})
		r := httptest.NewRequest(http.MethodPost, "/mcp", body)
		got := loggedMethod(t, r, func(_ http.ResponseWriter, inner *http.Request) {
			if _, err := io.ReadAll(inner.Body); !errors.Is(err, errBroken) {
				t.Errorf("read error = %v, want %v", err, errBroken)
			}
		})
		if got != "tools/call" {
			t.Fatalf("mcp_method = %q", got)
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
