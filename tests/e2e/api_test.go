package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const apiPkg = "github.com/AleixP/go-product-search/cmd/api"

// Every wait in this file is bounded, so a regression fails by assertion
// instead of hanging the package.
const (
	listenWait   = 10 * time.Second // the stub ping is local, so startup is fast
	sigintWait   = 10 * time.Second // graceful shutdown bound
	unreachWait  = 11 * time.Second // 10 s startup ping plus slack
	quickExit    = 10 * time.Second // usage errors exit at once
	clientBound  = 10 * time.Second
	processBound = 2 * time.Minute // upper bound for any child process
)

var listenRE = regexp.MustCompile(`msg=listening addr=(\S+)`)

const wantSearchJSON = `{"total":42,"page":1,"size":2,"items":[` +
	`{"id":"0046872021545","name":"Smoked Mussels In Cottonseed Oil","brands":["Family"],"categories":["en:undefined"],"nutriscore":"a"},` +
	`{"id":"9990000000002","name":"Green Tea <b>&","brands":[],"categories":["en:beverages","en:teas"],"nutriscore":""}],` +
	`"facets":{"brands":[],"categories":[],"nutriscore":[{"value":"a","count":0},{"value":"b","count":0},{"value":"c","count":0},{"value":"d","count":0},{"value":"e","count":0}]}}` + "\n"

const unavailableJSON = `{"error":{"code":"search_unavailable","message":"search is temporarily unavailable"}}` + "\n"

// analyzeCall is one request the stub saw on the _analyze endpoint.
type analyzeCall struct {
	method string
	body   string
}

// stubSearch is a stand-in for the OpenSearch calls the API makes: the startup
// ping, the search and the _analyze normalization. Any other request fails the
// test.
type stubSearch struct {
	srv         *httptest.Server
	missing     atomic.Bool // when set, the search answers 404 index_not_found_exception
	noAggs      atomic.Bool // when set, the search answers without aggregations
	analyzeFail atomic.Bool // when set, _analyze answers 400 and quotes the request text

	mu       sync.Mutex
	lastBody string
	method   string
	path     string
	searches int
	analyzes []analyzeCall

	gets        int    // GET /products/_doc/{id} requests
	getURI      string // raw RequestURI of the last one
	aliasChecks int    // HEAD /_alias/products requests
}

func newStubSearch(t *testing.T) *stubSearch {
	t.Helper()
	s := &stubSearch{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/"+alias+"/_search":
			s.mu.Lock()
			s.lastBody, s.method, s.path = string(body), r.Method, r.URL.Path
			s.searches++
			s.mu.Unlock()
			if s.missing.Load() {
				writeJSON(w, http.StatusNotFound, `{"error":{"root_cause":[{"type":"index_not_found_exception","reason":"stub-secret-text"}],"type":"index_not_found_exception","reason":"stub-secret-text"},"status":404}`)
				return
			}
			writeJSON(w, http.StatusOK, searchBody(string(body), s.noAggs.Load()))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/"+alias+"/_doc/"):
			s.getDoc(w, r)
		case r.Method == http.MethodHead && r.RequestURI == "/_alias/"+alias:
			s.aliasExists(w)
		case r.URL.Path == "/"+alias+"/_analyze":
			s.mu.Lock()
			s.analyzes = append(s.analyzes, analyzeCall{r.Method, string(body)})
			s.mu.Unlock()
			s.analyze(w, body)
		case r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/_nodes/_local/stats/") || r.URL.Path == "/_cluster/health"):
			// opensearch-go polls node stats every 5 s or more, and cluster
			// health on multi-node pools; both are background noise, not API
			// behavior, so they must not fail a long-lived API process.
			writeJSON(w, http.StatusOK, `{"nodes":{}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// analyze answers _analyze with one token per text, in order. The fold is only
// strings.ToLower: the real folded_keyword normalizer also folds accents, but
// the stub needs a deterministic canonicalization, not the engine's rule.
func (s *stubSearch) analyze(w http.ResponseWriter, body []byte) {
	var req struct {
		Normalizer string   `json:"normalizer"`
		Text       []string `json:"text"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Normalizer != "folded_keyword" {
		writeJSON(w, http.StatusBadRequest, `{"error":{"type":"illegal_argument_exception","reason":"stub: bad analyze request"},"status":400}`)
		return
	}
	if s.analyzeFail.Load() {
		// Like the real engine, the reason quotes the request text.
		reason, _ := json.Marshal("cannot normalize " + strings.Join(req.Text, ","))
		writeJSON(w, http.StatusBadRequest, `{"error":{"root_cause":[{"type":"illegal_argument_exception","reason":`+string(reason)+`}],"type":"illegal_argument_exception","reason":`+string(reason)+`},"status":400}`)
		return
	}
	tokens := make([]string, len(req.Text))
	for i, text := range req.Text {
		tk, _ := json.Marshal(strings.ToLower(text))
		tokens[i] = fmt.Sprintf(`{"token":%s,"start_offset":0,"end_offset":%d,"type":"word","position":%d}`, tk, len(text), i)
	}
	writeJSON(w, http.StatusOK, `{"tokens":[`+strings.Join(tokens, ",")+`]}`)
}

func (s *stubSearch) last() (body, method, path string, searches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBody, s.method, s.path, s.searches
}

// analyzed returns a copy of the _analyze requests seen so far.
func (s *stubSearch) analyzed() []analyzeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]analyzeCall(nil), s.analyzes...)
}

// buildAPI compiles the real command into dir and returns the binary path.
func buildAPI(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "api")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, apiPkg).CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", apiPkg, err, out)
	}
	return bin
}

// stderrBuf collects the child's stderr and wakes waiters on every write.
type stderrBuf struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	updated chan struct{}
}

func (b *stderrBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.updated <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (b *stderrBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// apiProc is a running API binary. All waits on it have a timer.
type apiProc struct {
	cmd    *exec.Cmd
	stderr *stderrBuf
	exited chan int // receives the exit code once, then is closed
}

// startAPI launches the binary under a bounded context. The cleanup kills the
// child (a no-op when it already exited) and waits for it with a bound.
func startAPI(t *testing.T, bin string, args ...string) *apiProc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), processBound)
	p := &apiProc{
		cmd:    exec.CommandContext(ctx, bin, args...),
		stderr: &stderrBuf{updated: make(chan struct{}, 1)},
		exited: make(chan int, 1),
	}
	p.cmd.Stderr = p.stderr
	p.cmd.WaitDelay = 2 * time.Second
	if err := p.cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", bin, err)
	}
	go func() {
		err := p.cmd.Wait()
		code := 0
		if err != nil {
			code = -1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		p.exited <- code
		close(p.exited)
	}()
	t.Cleanup(func() {
		cancel() // kills the child when it is still running
		select {
		case <-p.exited:
		case <-time.After(10 * time.Second):
			t.Errorf("child process %d did not exit after kill", p.cmd.Process.Pid)
		}
	})
	return p
}

// waitExit returns the exit code, failing the test when the child is still
// running after d.
func (p *apiProc) waitExit(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case code := <-p.exited:
		return code
	case <-time.After(d):
		t.Fatalf("process did not exit within %v\nstderr:\n%s", d, p.stderr.String())
		return 0
	}
}

// waitAddr waits for the "listening" log line and returns the address.
func (p *apiProc) waitAddr(t *testing.T) string {
	t.Helper()
	timer := time.NewTimer(listenWait)
	defer timer.Stop()
	for {
		if m := listenRE.FindStringSubmatch(p.stderr.String()); m != nil {
			return m[1]
		}
		select {
		case <-p.stderr.updated:
		case <-p.exited: // exited before listening; the next check reports it
			t.Fatalf("process exited before the listening line\nstderr:\n%s", p.stderr.String())
		case <-timer.C:
			t.Fatalf("no listening line within %v\nstderr:\n%s", listenWait, p.stderr.String())
		}
	}
}

// closedPort returns a loopback port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

type reply struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, c *http.Client, method, url string) reply {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, url, err)
	}
	return reply{resp.StatusCode, resp.Header, string(b)}
}

func wantJSONReply(t *testing.T, got reply, status int, body string) {
	t.Helper()
	if got.status != status {
		t.Errorf("status = %d, want %d\nbody: %s", got.status, status, got.body)
	}
	if got.body != body {
		t.Errorf("body = %q, want %q", got.body, body)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if v := got.header.Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", v)
	}
}

func TestAPI_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the api binary; skipped under -short")
	}
	bin := buildAPI(t, t.TempDir())
	stub := newStubSearch(t)
	client := &http.Client{Timeout: clientBound, Transport: &http.Transport{DisableKeepAlives: true}}

	proc := startAPI(t, bin, "--addr", "127.0.0.1:0", "--opensearch", stub.srv.URL)
	base := "http://" + proc.waitAddr(t)

	t.Run("search returns the exact JSON and asks for an exact total", func(t *testing.T) {
		got := do(t, client, http.MethodGet, base+"/search?q=tea&size=2")
		wantJSONReply(t, got, http.StatusOK, wantSearchJSON)
		body, method, path, _ := stub.last()
		if method != http.MethodPost || path != "/products/_search" {
			t.Errorf("stub saw %s %s, want POST /products/_search", method, path)
		}
		for _, want := range []string{`"track_total_hits":true`, `"from":0`, `"size":2`, `"query":"tea"`} {
			if !strings.Contains(body, want) {
				t.Errorf("stub body %s lacks %s", body, want)
			}
		}
	})

	t.Run("page offset reaches OpenSearch as from and is echoed back", func(t *testing.T) {
		got := do(t, client, http.MethodGet, base+"/search?q=tea&size=10&page=3")
		if want := `{"total":42,"page":3,"size":10,"items":[`; !strings.HasPrefix(got.body, want) {
			t.Errorf("body = %q, want prefix %q", got.body, want)
		}
		body, _, _, _ := stub.last()
		for _, want := range []string{`"from":20`, `"size":10`} {
			if !strings.Contains(body, want) {
				t.Errorf("stub body %s lacks %s", body, want)
			}
		}
	})

	t.Run("the last page of the window is accepted", func(t *testing.T) {
		got := do(t, client, http.MethodGet, base+"/search?page=500")
		if got.status != http.StatusOK {
			t.Fatalf("status = %d, want 200\nbody: %s", got.status, got.body)
		}
		if body, _, _, _ := stub.last(); !strings.Contains(body, `"from":9980`) {
			t.Errorf("stub body %s lacks \"from\":9980", body)
		}
	})

	t.Run("invalid parameters are 400 and never reach OpenSearch", func(t *testing.T) {
		_, _, _, before := stub.last()
		tests := []struct{ name, url, body string }{
			{"size zero", "/search?size=0", `{"error":{"code":"invalid_parameter","message":"size must be an integer between 1 and 100"}}` + "\n"},
			{"size above the maximum", "/search?size=101", `{"error":{"code":"invalid_parameter","message":"size must be an integer between 1 and 100"}}` + "\n"},
			{"window with size one", "/search?size=1&page=10001", `{"error":{"code":"page_out_of_range","message":"page must be at most 10000 when size is 1"}}` + "\n"},
			{"page beyond window", "/search?page=501", `{"error":{"code":"page_out_of_range","message":"page must be at most 500 when size is 20"}}` + "\n"},
		}
		for _, tt := range tests {
			wantJSONReply(t, do(t, client, http.MethodGet, base+tt.url), http.StatusBadRequest, tt.body)
		}
		if _, _, _, after := stub.last(); after != before {
			t.Errorf("stub saw %d searches for invalid requests, want 0", after-before)
		}
	})

	t.Run("a missing index is a fixed 503 without engine text", func(t *testing.T) {
		stub.missing.Store(true)
		t.Cleanup(func() { stub.missing.Store(false) })
		got := do(t, client, http.MethodGet, base+"/search?q=tea")
		wantJSONReply(t, got, http.StatusServiceUnavailable, unavailableJSON)
		for _, leak := range []string{"stub-secret-text", "index_not_found"} {
			if strings.Contains(got.body, leak) {
				t.Errorf("body leaks %q: %s", leak, got.body)
			}
		}
	})

	t.Run("the API recovers when the index is back", func(t *testing.T) {
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/search?q=tea&size=2"), http.StatusOK, wantSearchJSON)
	})

	t.Run("unknown path is a plain 404", func(t *testing.T) {
		got := do(t, client, http.MethodGet, base+"/nope")
		if got.status != http.StatusNotFound || got.body != "404 page not found\n" {
			t.Errorf("got %d %q, want 404 and the mux's plain body", got.status, got.body)
		}
		if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
	})

	t.Run("POST /search is 405 with Allow", func(t *testing.T) {
		got := do(t, client, http.MethodPost, base+"/search?q=tea")
		if got.status != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", got.status)
		}
		if allow := got.header.Get("Allow"); !strings.Contains(allow, "GET") {
			t.Errorf("Allow = %q, want it to list GET", allow)
		}
		if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
	})

	t.Run("HEAD /search is 200 without a body", func(t *testing.T) {
		got := do(t, client, http.MethodHead, base+"/search?q=tea&size=2")
		if got.status != http.StatusOK || got.body != "" {
			t.Errorf("got %d %q, want 200 and no body", got.status, got.body)
		}
	})

	filterSubtests(t, stub, client, base, proc)
	suggestSubtests(t, stub, client, base, proc)
	productSubtests(t, stub, client, base, proc)

	t.Run("SIGINT shuts down gracefully with exit 0", func(t *testing.T) {
		if err := proc.cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		if code := proc.waitExit(t, sigintWait); code != 0 {
			t.Errorf("exit code = %d, want 0\nstderr:\n%s", code, proc.stderr.String())
		}
	})

	t.Run("unreachable OpenSearch exits 1 and hides the path", func(t *testing.T) {
		p := startAPI(t, bin, "--addr", "127.0.0.1:0", "--opensearch", "http://"+closedPort(t)+"/s3cr3t")
		if code := p.waitExit(t, unreachWait); code != 1 {
			t.Errorf("exit code = %d, want 1\nstderr:\n%s", code, p.stderr.String())
		}
		got := p.stderr.String()
		if strings.Contains(got, "s3cr3t") {
			t.Errorf("stderr leaks the URL path:\n%s", got)
		}
		if !strings.Contains(got, "api: ") {
			t.Errorf("stderr = %q, want an api: error line", got)
		}
		if strings.Contains(got, "msg=listening") {
			t.Errorf("stderr says listening although startup failed:\n%s", got)
		}
	})

	t.Run("unknown flag exits 2", func(t *testing.T) {
		p := startAPI(t, bin, "--bogus")
		if code := p.waitExit(t, quickExit); code != 2 {
			t.Errorf("exit code = %d, want 2\nstderr:\n%s", code, p.stderr.String())
		}
		if !strings.Contains(p.stderr.String(), "bogus") {
			t.Errorf("stderr = %q, want it to name the flag", p.stderr.String())
		}
	})

	t.Run("binary is built outside the repository", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join("..", "..", "api")); err == nil {
			t.Error("a binary named api exists in the repo root")
		}
	})
}
