package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// syncBuf is a bytes.Buffer safe for the concurrent use of run and the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeSearcher answers every search with one fixed product, after fn if set.
type fakeSearcher struct {
	fn func(ctx context.Context) error
}

func (f fakeSearcher) Search(ctx context.Context, _ catalog.Query) (catalog.Result, error) {
	if f.fn != nil {
		if err := f.fn(ctx); err != nil {
			return catalog.Result{}, err
		}
	}
	return catalog.Result{Total: 1, Products: []catalog.Product{{ID: "1", Name: "Tea"}}}, nil
}

func factoryOf(s catalog.Searcher) func(context.Context, string) (catalog.Searcher, error) {
	return func(context.Context, string) (catalog.Searcher, error) { return s, nil }
}

var listeningRe = regexp.MustCompile(`msg=listening addr=(\S+)`)

// startRun runs the server in the background and waits for its listening line.
func startRun(t *testing.T, args []string, s catalog.Searcher) (addr string, cancel context.CancelFunc, done <-chan int, out *syncBuf) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out = &syncBuf{}
	ch := make(chan int, 1)
	go func() { ch <- run(ctx, args, out, factoryOf(s)) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := listeningRe.FindStringSubmatch(out.String()); m != nil {
			return m[1], cancel, ch, out
		}
		select {
		case code := <-ch:
			t.Fatalf("run returned %d before listening: %s", code, out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("no listening line: %s", out.String())
	return
}

func waitCode(t *testing.T, done <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case c := <-done:
		return c
	case <-time.After(within):
		t.Fatalf("run did not return within %v", within)
		return -1
	}
}

func TestShutdownTimeoutDefault(t *testing.T) {
	if shutdownTimeout != 10*time.Second {
		t.Errorf("shutdownTimeout = %v, want 10s", shutdownTimeout)
	}
}

func TestNewServer_Settings(t *testing.T) {
	var logBuf syncBuf
	log := newLogger(&logBuf)
	h := http.NewServeMux()
	srv := newServer(h, log)
	if srv.Handler != h {
		t.Error("Handler not set")
	}
	checks := []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 15 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if srv.MaxHeaderBytes != 16384 {
		t.Errorf("MaxHeaderBytes = %d, want 16384", srv.MaxHeaderBytes)
	}
	if srv.ErrorLog == nil {
		t.Fatal("ErrorLog not set")
	}
	srv.ErrorLog.Print("boom")
	if got := logBuf.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "boom") {
		t.Errorf("ErrorLog output = %q, want a WARN line with boom", got)
	}
}

func TestRun_FlagErrors(t *testing.T) {
	called := false
	f := func(context.Context, string) (catalog.Searcher, error) { called = true; return nil, errors.New("x") }
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{"unknown flag", []string{"--bogus"}, 2, "Usage: api [flags]"},
		{"positional", []string{"extra"}, 2, "api: unexpected argument"},
		{"positional shows usage", []string{"extra"}, 2, "Usage: api [flags]"},
		{"help", []string{"-h"}, 0, "Usage: api [flags]"},
		{"help long", []string{"--help"}, 0, "-opensearch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out syncBuf
			if code := runBounded(tt.args, &out, f); code != tt.wantCode {
				t.Errorf("run(%v) = %d, want %d", tt.args, code, tt.wantCode)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("stderr = %q, want it to contain %q", out.String(), tt.wantOut)
			}
		})
	}
	if called {
		t.Error("the factory was called on a usage error")
	}
}

func TestRun_UsageShowsFlagDefaults(t *testing.T) {
	var out syncBuf
	runBounded([]string{"-h"}, &out, nil)
	for _, want := range []string{"127.0.0.1:8080", "http://localhost:9200", "-addr", "-opensearch"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q: %s", want, out.String())
		}
	}
}

func TestRun_FactoryErrorExitsOneAndGetsFlagValue(t *testing.T) {
	var got string
	f := func(_ context.Context, addr string) (catalog.Searcher, error) {
		got = addr
		return nil, errors.New("ping failed")
	}
	var out syncBuf
	code := runBounded([]string{"--opensearch", "http://os.example:9201"}, &out, f)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if got != "http://os.example:9201" {
		t.Errorf("factory got %q", got)
	}
	if out.String() != "api: ping failed\n" {
		t.Errorf("stderr = %q, want exactly one api: line", out.String())
	}
}

func TestRun_DefaultOpenSearchAddress(t *testing.T) {
	var got string
	f := func(_ context.Context, addr string) (catalog.Searcher, error) {
		got = addr
		return nil, errors.New("stop")
	}
	runBounded(nil, &syncBuf{}, f)
	if got != "http://localhost:9200" {
		t.Errorf("default --opensearch = %q", got)
	}
}

// A typed nil *T in the interface would pass a nil check and panic later:
// the factory must hand back a true nil interface on error.
func TestOpenSearcher_ErrorIsNilInterface(t *testing.T) {
	s, err := openSearcher(context.Background(), "ftp://nope")
	if err == nil {
		t.Fatal("want an error for a non-http address")
	}
	if s != nil {
		t.Errorf("searcher = %#v, want a nil interface", s)
	}
}

// httpClient bounds every request a test makes so a stuck server fails the test.
var httpClient = &http.Client{Timeout: 5 * time.Second}

// runBounded runs run in a goroutine with a 3 s context and gives up after 5 s,
// returning -1. A regression that keeps serving, or a run that ignores its
// context, then fails the test instead of hanging the package.
func runBounded(args []string, stderr io.Writer, newSearcher func(context.Context, string) (catalog.Searcher, error)) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, args, stderr, newSearcher) }()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		return -1 // run ignored its context: the test fails here instead of hanging
	}
}

func TestRun_AddressInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	var out syncBuf
	code := runBounded([]string{"--addr", l.Addr().String()}, &out, factoryOf(fakeSearcher{}))
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.HasPrefix(out.String(), "api: ") || strings.Contains(out.String(), "listening") {
		t.Errorf("stderr = %q", out.String())
	}
}

func TestRun_InvalidAddrIsExitOne(t *testing.T) {
	var out syncBuf
	code := runBounded([]string{"--addr", "not-an-address"}, &out, factoryOf(fakeSearcher{}))
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}

func TestRun_ServesSearchAndStopsOnCancel(t *testing.T) {
	addr, cancel, done, out := startRun(t, []string{"--addr", "127.0.0.1:0"}, fakeSearcher{})
	if !strings.Contains(out.String(), "level=INFO") {
		t.Errorf("listening line is not INFO: %s", out.String())
	}
	resp, err := httpClient.Get("http://" + addr + "/search?q=tea")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"total":1`) {
		t.Errorf("GET /search = %d %s", resp.StatusCode, body)
	}
	cancel()
	if code := waitCode(t, done, time.Second); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Contains(out.String(), "level=ERROR") || strings.Contains(out.String(), "level=WARN") {
		t.Errorf("clean shutdown logged problems: %s", out.String())
	}
}

func TestRun_HandlerLogsToStderr(t *testing.T) {
	s := fakeSearcher{fn: func(context.Context) error { return errors.New("backend down") }}
	addr, cancel, done, out := startRun(t, []string{"--addr", "127.0.0.1:0"}, s)
	resp, err := httpClient.Get("http://" + addr + "/search")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	cancel()
	waitCode(t, done, time.Second)
	if !strings.Contains(out.String(), "msg=\"search failed\"") || !strings.Contains(out.String(), "backend down") {
		t.Errorf("handler error not on stderr: %s", out.String())
	}
}

func TestRun_GracefulShutdownFinishesInFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s := fakeSearcher{fn: func(context.Context) error {
		close(entered)
		<-release // ignores ctx on purpose
		return nil
	}}
	addr, cancel, done, _ := startRun(t, []string{"--addr", "127.0.0.1:0"}, s)
	type result struct {
		status int
		err    error
	}
	res := make(chan result, 1)
	client := &http.Client{Timeout: 10 * time.Second}
	go func() {
		resp, err := client.Get("http://" + addr + "/search")
		if err != nil {
			res <- result{err: err}
			return
		}
		_ = resp.Body.Close()
		res <- result{status: resp.StatusCode}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the searcher")
	}
	cancel()
	select {
	case c := <-done:
		t.Fatalf("run returned %d while a request was in flight", c)
	case <-time.After(200 * time.Millisecond):
	}
	release <- struct{}{} // lets the handler finish; the deferred close frees it on any early failure
	select {
	case r := <-res:
		if r.err != nil || r.status != 200 {
			t.Errorf("in-flight response = %+v, want 200", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no response for the in-flight request")
	}
	if code := waitCode(t, done, 2*time.Second); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
}

func TestRun_ShutdownTimeoutExitsOne(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 100 * time.Millisecond
	defer func() { shutdownTimeout = old }()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s := fakeSearcher{fn: func(context.Context) error {
		close(entered)
		<-release
		return nil
	}}
	addr, cancel, done, out := startRun(t, []string{"--addr", "127.0.0.1:0"}, s)
	client := &http.Client{Timeout: 10 * time.Second}
	go func() {
		if resp, err := client.Get("http://" + addr + "/search"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the searcher")
	}
	cancel()
	if code := waitCode(t, done, time.Second); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "level=ERROR") || !strings.Contains(out.String(), "shutdown") {
		t.Errorf("shutdown failure not logged: %s", out.String())
	}
}

// failingListener makes Serve return a non-ErrServerClosed error.
type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept broke") }

func TestRun_ServeErrorExitsOne(t *testing.T) {
	old := listen
	listen = func(network, addr string) (net.Listener, error) {
		l, err := old(network, addr)
		return failingListener{l}, err
	}
	defer func() { listen = old }()
	var out syncBuf
	done := make(chan int, 1)
	go func() {
		done <- runBounded([]string{"--addr", "127.0.0.1:0"}, &out, factoryOf(fakeSearcher{}))
	}()
	if code := waitCode(t, done, 2*time.Second); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "level=ERROR") || !strings.Contains(out.String(), "accept broke") {
		t.Errorf("serve error not logged: %s", out.String())
	}
}

func TestRun_StartupErrorsDoNotLeakPathOrUserinfo(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := closed.Addr().String()
	_ = closed.Close()
	for _, addr := range []string{
		"http://" + port + "/s3cr3t?token=t0ken",
		"http://user:p4ss@" + port + "/s3cr3t",
		"ftp://" + port + "/s3cr3t",
	} {
		var out syncBuf
		start := time.Now()
		code := runBounded([]string{"--opensearch", addr}, &out, openSearcher)
		if code != 1 {
			t.Errorf("%s: code = %d, want 1", addr, code)
		}
		for _, secret := range []string{"s3cr3t", "p4ss", "t0ken"} {
			if strings.Contains(out.String(), secret) {
				t.Errorf("%s: stderr leaks %q: %s", addr, secret, out.String())
			}
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: took %v", addr, d)
		}
	}
}
