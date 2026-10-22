package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type sample struct {
	d   time.Duration
	err bool
}

// Result is one scenario at one concurrency.
type Result struct {
	Scenario    string  `json:"scenario"`
	Concurrency int     `json:"concurrency"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	RPS         float64 `json:"rps"`
	P50, P95    float64 // milliseconds
	P99, Max    float64
	Note        string `json:"note,omitempty"`
}

type runner struct {
	base   string
	client *http.Client
	ds     Dataset
	// per-user state shared across scenarios
	mu      sync.Mutex
	seen    map[int][]string // memory ids each user has seen in search results
	portals map[int]*portalSession
}

type portalSession struct {
	client *http.Client
}

func runCmd(ctx context.Context, args []string) error {
	var base, users, caFile, scenarios, conc, jaeger, outJSON string
	var dur time.Duration
	flags("run", args, func(fs *flag.FlagSet) {
		fs.StringVar(&base, "base", "", "public URL, e.g. https://memory.example.com (required)")
		fs.StringVar(&users, "users", "users.json", "dataset written by seed")
		fs.StringVar(&caFile, "ca", "", "extra CA bundle for the server certificate")
		fs.StringVar(&scenarios, "scenarios", "search,write,portal_home,portal_scope,portal_search,portal_memory,portal_review", "comma-separated scenarios")
		fs.StringVar(&conc, "concurrency", "1,8,32", "comma-separated concurrency levels")
		fs.DurationVar(&dur, "duration", 60*time.Second, "duration per scenario and level")
		fs.StringVar(&jaeger, "jaeger", "", "Jaeger query URL; when set, search latency excluding query embedding is computed from traces")
		fs.StringVar(&outJSON, "out", "", "write results as JSON here")
	})
	if base == "" {
		return fmt.Errorf("--base is required")
	}
	ds, err := readDataset(users)
	if err != nil {
		return err
	}
	tr := &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 512, IdleConnTimeout: 90 * time.Second}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return err
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(pem)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	r := &runner{base: strings.TrimRight(base, "/"), client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
		ds: ds, seen: map[int][]string{}, portals: map[int]*portalSession{}}
	var levels []int
	for _, c := range strings.Split(conc, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(c))
		if err != nil || n < 1 {
			return fmt.Errorf("bad concurrency %q", c)
		}
		levels = append(levels, n)
	}
	// Sign portal users in before measuring: sign-in is limited per client
	// address (all load users share this machine's), and real users sign in
	// once, not on every page.
	if strings.Contains(scenarios, "portal_") {
		top := slices.Max(levels)
		for i := 0; i < min(top, len(ds.Users)); i++ {
			for {
				if _, err := r.session(ctx, i); err == nil {
					break
				} else if !strings.Contains(err.Error(), "429") {
					return fmt.Errorf("warm-up sign-in: %w", err)
				}
				time.Sleep(5 * time.Second)
			}
		}
		fmt.Printf("signed in %d portal users before measuring\n", min(top, len(ds.Users)))
	}
	var results []Result
	for _, sc := range strings.Split(scenarios, ",") {
		fn, ok := r.scenario(strings.TrimSpace(sc))
		if !ok {
			return fmt.Errorf("unknown scenario %q", sc)
		}
		for _, c := range levels {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			start := time.Now()
			res := r.measure(ctx, sc, c, dur, fn)
			if sc == "search" && jaeger != "" {
				if note, err := excludeEmbedding(ctx, jaeger, start, time.Now(), &res); err != nil {
					res.Note = "trace analysis failed: " + err.Error()
				} else {
					res.Note = note
				}
			}
			results = append(results, res)
			fmt.Printf("%-14s c=%-3d n=%-6d err=%-4d rps=%-7.1f p50=%-7.1f p95=%-7.1f p99=%-7.1f max=%-7.1f %s\n",
				res.Scenario, res.Concurrency, res.Requests, res.Errors, res.RPS, res.P50, res.P95, res.P99, res.Max, res.Note)
		}
	}
	if outJSON != "" {
		b, _ := json.MarshalIndent(results, "", "  ")
		return os.WriteFile(outJSON, b, 0o644)
	}
	return nil
}

// measure runs fn from c workers for d (closed loop: each worker sends its
// next request when the previous one returns). Worker i acts as user i.
func (r *runner) measure(ctx context.Context, name string, c int, d time.Duration, fn func(context.Context, int, *rand.Rand) error) Result {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var mu sync.Mutex
	var all []sample
	var wg sync.WaitGroup
	start := time.Now()
	var firstErr string
	for w := range c {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), uint64(c)))
			var local []sample
			for ctx.Err() == nil {
				t := time.Now()
				err := fn(ctx, w%len(r.ds.Users), rng)
				if ctx.Err() != nil && err != nil {
					break // cut off by the deadline, not a server error
				}
				local = append(local, sample{time.Since(t), err != nil})
				if err != nil {
					mu.Lock()
					if firstErr == "" {
						firstErr = err.Error()
					}
					mu.Unlock()
				}
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	el := time.Since(start)
	res := Result{Scenario: name, Concurrency: c, Requests: len(all), RPS: float64(len(all)) / el.Seconds()}
	var ok []float64
	for _, s := range all {
		if s.err {
			res.Errors++
			continue
		}
		ok = append(ok, float64(s.d.Microseconds())/1000)
	}
	res.P50, res.P95, res.P99, res.Max = pct(ok, 50), pct(ok, 95), pct(ok, 99), pct(ok, 100)
	if firstErr != "" {
		res.Note = "first error: " + truncate(firstErr, 120)
	}
	return res
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	slices.Sort(v)
	i := int(float64(len(v)-1) * p / 100)
	return v[i]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (r *runner) scenario(name string) (func(context.Context, int, *rand.Rand) error, bool) {
	switch name {
	case "search":
		return r.search, true
	case "write":
		return r.write, true
	case "portal_home":
		return r.portalGet(func(u User, _ *rand.Rand, _ int) string { return "/portal/" }), true
	case "portal_scope":
		return r.portalGet(func(u User, _ *rand.Rand, _ int) string { return "/portal/s/" + u.Scope }), true
	case "portal_search":
		return r.portalGet(func(u User, rng *rand.Rand, _ int) string {
			return "/portal/search?" + url.Values{"scope": {u.Scope}, "q": {words(rng, 2)}}.Encode()
		}), true
	case "portal_memory":
		return r.portalGet(func(u User, rng *rand.Rand, i int) string {
			r.mu.Lock()
			ids := r.seen[i]
			r.mu.Unlock()
			if len(ids) == 0 {
				return "/portal/s/" + u.Scope
			}
			return "/portal/m/" + ids[rng.IntN(len(ids))]
		}), true
	case "portal_review":
		return r.portalGet(func(u User, _ *rand.Rand, _ int) string {
			if !u.Maintain {
				return "/portal/s/" + u.Scope
			}
			return "/portal/review/" + u.Scope
		}), true
	}
	return nil, false
}

func (r *runner) mcp(ctx context.Context, u User, tool string, args map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+u.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d %s", tool, resp.StatusCode, truncate(string(raw), 100))
	}
	var rpc struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("%s: %s", tool, rpc.Error.Message)
	}
	if len(rpc.Result.Content) == 0 {
		return nil, fmt.Errorf("%s: empty result", tool)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(rpc.Result.Content[0].Text), &out)
	if rpc.Result.IsError {
		return out, fmt.Errorf("%s: tool error %v", tool, out["error"])
	}
	return out, nil
}

func (r *runner) search(ctx context.Context, i int, rng *rand.Rand) error {
	u := r.ds.Users[i]
	out, err := r.mcp(ctx, u, "memory_search", map[string]any{"scope": u.Scope, "query": words(rng, 2), "limit": 10})
	if err != nil {
		return err
	}
	if res, ok := out["results"].([]any); ok {
		r.mu.Lock()
		for _, x := range res {
			if m, ok := x.(map[string]any); ok && len(r.seen[i]) < 200 {
				r.seen[i] = append(r.seen[i], fmt.Sprint(m["id"]))
			}
		}
		r.mu.Unlock()
	}
	return nil
}

func (r *runner) write(ctx context.Context, i int, rng *rand.Rand) error {
	// Readers cannot write; use the nearest writer.
	for k := 0; k < len(r.ds.Users); k++ {
		u := r.ds.Users[(i+k)%len(r.ds.Users)]
		if u.Writer {
			_, err := r.mcp(ctx, u, "memory_create", map[string]any{"scope": u.Scope, "kind": kinds[rng.IntN(len(kinds))],
				"title": "Load " + words(rng, 4), "content": "We " + words(rng, 40) + ".", "rationale": "Because " + words(rng, 12) + "."})
			return err
		}
	}
	return fmt.Errorf("no writer in the dataset")
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// session signs user i in to the portal with its token (once).
func (r *runner) session(ctx context.Context, i int) (*portalSession, error) {
	r.mu.Lock()
	s := r.portals[i]
	r.mu.Unlock()
	if s != nil {
		return s, nil
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Transport: r.client.Transport, Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(r.base + "/portal/login")
	if err != nil {
		return nil, err
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := csrfRe.FindSubmatch(page)
	if m == nil {
		return nil, fmt.Errorf("portal login page has no CSRF token")
	}
	form := url.Values{"csrf": {string(m[1])}, "token": {r.ds.Users[i].Token}, "next": {"/portal/"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/portal/login/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", r.base)
	resp, err = c.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return nil, fmt.Errorf("portal sign-in: HTTP %d", resp.StatusCode)
	}
	s = &portalSession{client: c}
	r.mu.Lock()
	r.portals[i] = s
	r.mu.Unlock()
	return s, nil
}

func (r *runner) portalGet(path func(User, *rand.Rand, int) string) func(context.Context, int, *rand.Rand) error {
	return func(ctx context.Context, i int, rng *rand.Rand) error {
		s, err := r.session(ctx, i)
		if err != nil {
			return err
		}
		p := path(r.ds.Users[i], rng, i)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.base+p, nil)
		resp, err := s.client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: HTTP %d", strings.SplitN(p, "?", 2)[0], resp.StatusCode)
		}
		return nil
	}
}

// excludeEmbedding reads the search traces of the window from Jaeger and
// computes percentiles of (tool span - query embedding spans), the latency
// the engineering target is defined on.
func excludeEmbedding(ctx context.Context, jaeger string, from, to time.Time, res *Result) (string, error) {
	q := url.Values{"service": {"team-memory-serve"}, "operation": {"mcp.tool memory_search"},
		"start": {strconv.FormatInt(from.UnixMicro(), 10)}, "end": {strconv.FormatInt(to.UnixMicro(), 10)}, "limit": {"100000"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(jaeger, "/")+"/api/traces?"+q.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data []struct {
			Spans []struct {
				OperationName string `json:"operationName"`
				Duration      int64  `json:"duration"`
			} `json:"spans"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	var net, embed []float64
	for _, t := range body.Data {
		var tool, emb int64 = -1, 0
		for _, s := range t.Spans {
			switch {
			case s.OperationName == "mcp.tool memory_search":
				tool = s.Duration
			case strings.HasPrefix(s.OperationName, "POST ") && !strings.HasSuffix(s.OperationName, "/mcp"):
				emb += s.Duration
			}
		}
		if tool >= 0 {
			net = append(net, float64(tool-emb)/1000)
			embed = append(embed, float64(emb)/1000)
		}
	}
	if len(net) == 0 {
		return "", fmt.Errorf("no traces in the window")
	}
	return fmt.Sprintf("excl. query embedding (%d traces): p50=%.1f p95=%.1f p99=%.1f; query embedding p50=%.1f p95=%.1f",
		len(net), pct(net, 50), pct(net, 95), pct(net, 99), pct(embed, 50), pct(embed, 95)), nil
}
