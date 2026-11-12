package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// srcWaitUntil polls cond until it holds or the deadline passes.
func srcWaitUntil(t testing.TB, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// srcStall sends the headers and a few bytes, then blocks until the test ends.
func srcStall(s *srcTestServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "abc\n")
		w.(http.Flusher).Flush()
		select {
		case <-s.release:
		case <-r.Context().Done():
		}
	})
}

// --- production client ---

func TestNewHTTPClient_ProductionSettings(t *testing.T) {
	c := newHTTPClient()

	// Client.Timeout covers connection time, redirects AND reading the body (net/http
	// docs): it would cut a long download, so it must stay zero.
	if c.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (it would cap the whole body read)", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Error("CheckRedirect is not set")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}
	if !tr.DisableCompression {
		t.Error("DisableCompression is false")
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 10s", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 30s", tr.ResponseHeaderTimeout)
	}
	if tr.DialContext == nil {
		t.Error("DialContext is not set (the dial timeout would not apply)")
	}
	if httpDialer.Timeout != 10*time.Second {
		t.Errorf("dial timeout = %v, want 10s", httpDialer.Timeout)
	}
	if tr.Proxy == nil {
		t.Error("the transport ignores the proxy environment")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 is false: a custom DialContext would turn HTTP/2 off")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil: the minimum TLS version is not pinned")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
}

func TestSourceConstants(t *testing.T) {
	if maxRedirects != 5 {
		t.Errorf("maxRedirects = %d, want 5", maxRedirects)
	}
	if idleTimeout != 60*time.Second {
		t.Errorf("idleTimeout = %v, want 60s", idleTimeout)
	}
	want := "go-product-search-ingest/1 (+https://github.com/AleixP/go-product-search)"
	if userAgent != want {
		t.Errorf("userAgent = %q, want %q", userAgent, want)
	}
}

func TestCheckRedirect_Policy(t *testing.T) {
	c := newHTTPClient()
	via := func(n int) []*http.Request {
		v := make([]*http.Request, n)
		for i := range v {
			v[i] = &http.Request{}
		}
		return v
	}
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	tests := []struct {
		name    string
		target  string
		hops    int // len(via): redirects followed so far, including this one
		wantErr bool
	}{
		{"first https redirect", "https://example.com/a", 1, false},
		{"fifth https redirect", "https://example.com/a", 5, false},
		{"sixth https redirect", "https://example.com/a", 6, true},
		{"seventh https redirect", "https://example.com/a", 7, true},
		{"http target", "http://example.com/a?token=" + srcSecret, 1, true},
		{"http target uppercase", "HTTP://example.com/a", 1, true},
		{"https uppercase is fine", "HTTPS://example.com/a", 1, false},
		{"ftp target", "ftp://example.com/a", 1, true},
		{"userinfo target", "https://u:" + srcSecret + "@example.com/a", 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.CheckRedirect(req(tt.target), via(tt.hops))

			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckRedirect err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && (strings.Contains(err.Error(), srcSecret) || strings.Contains(err.Error(), "token")) {
				t.Errorf("error leaks the query or userinfo: %q", err)
			}
		})
	}
}

// --- idle-read watchdog ---

func TestWatchdog_StalledBodyFailsPromptly(t *testing.T) {
	s := srcStartH(t, srcStall)
	src, err := openInputIdle(t.Context(), s.cl, s.url("/stall?token="+srcSecret), 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })

	start := time.Now()
	got, err := io.ReadAll(src)

	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("err = %v, want errIdleTimeout", err)
	}
	if string(got) != "abc\n" {
		t.Errorf("data before the stall = %q", got)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("stalled read took %v to fail", d)
	}
	srcNoLeak(t, err)
}

func TestWatchdog_StalledBeforeHeadersFailsPromptly(t *testing.T) {
	s := srcStartH(t, func(s *srcTestServer) http.Handler {
		return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-s.release:
			case <-r.Context().Done():
			}
		})
	})

	start := time.Now()
	_, err := openInputIdle(t.Context(), s.cl, s.url("/stall?token="+srcSecret), 200*time.Millisecond)

	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("err = %v, want errIdleTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("open took %v to fail", d)
	}
	srcNoLeak(t, err)
}

// Bytes keep coming, each gap clearly under the idle timeout but over half of
// it, the total over twice the timeout: the timer is re-armed by every Read.
func TestWatchdog_SlowButSteadyBodyDoesNotFail(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 4 {
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			time.Sleep(250 * time.Millisecond)
		}
	}))
	src, err := openInputIdle(t.Context(), s.cl, s.url("/slow"), 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })

	start := time.Now()
	got, err := io.ReadAll(src)

	if err != nil || string(got) != strings.Repeat("x", 4) {
		t.Errorf("ReadAll = %q, %v", got, err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("the body arrived faster than the idle timeout: the test proves nothing")
	}
}

// A gap longer than the idle timeout, though shorter than twice it, fails.
func TestWatchdog_GapLongerThanIdleFails(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ab") // two bytes: enough for the gzip peek
		w.(http.Flusher).Flush()
		select {
		case <-time.After(700 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "b")
	}))
	src, err := openInputIdle(t.Context(), s.cl, s.url("/gap"), 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })

	_, err = io.ReadAll(src)

	if !errors.Is(err, errIdleTimeout) {
		t.Errorf("err = %v, want errIdleTimeout", err)
	}
}

// The watchdog measures time spent WAITING for bytes. A consumer that is busy
// (for example waiting on a bulk request) and does not read for longer than the
// idle timeout must not trigger it.
func TestWatchdog_SlowConsumerDoesNotTrigger(t *testing.T) {
	s := srcStart(t, srcServe(srcIncompressible(1<<20)))
	src, err := openInputIdle(t.Context(), s.cl, s.url("/big"), 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	if _, err := io.ReadFull(src, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond) // pause between reads, longer than the idle timeout
	n, err := io.Copy(io.Discard, src)

	if err != nil || n != 1<<20-1000 {
		t.Errorf("Copy = %d, %v; want the rest of the body and no error", n, err)
	}
}

func TestWatchdog_CloseStopsTheTimer(t *testing.T) {
	s := srcStart(t, srcServe([]byte("abc\n")))
	src := srcOpenURL(t, s, "/x")
	src.idle.timer.Reset(time.Hour) // simulate a pending watchdog

	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	if src.idle.timer.Stop() {
		t.Error("Close left the watchdog timer pending")
	}
}

// --- Close and context ---

func TestSource_CloseClosesBodyOnceAndTheConnection(t *testing.T) {
	s := srcStart(t, srcServe(srcIncompressible(1<<20)))
	tt := s.track()
	src, err := openInput(t.Context(), s.cl, s.url("/big"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(src, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}

	e1, e2 := src.Close(), src.Close()

	if e1 != nil || e2 != nil {
		t.Errorf("Close errors: %v, %v", e1, e2)
	}
	tt.mu.Lock()
	n, closes := len(tt.bodies), int32(0)
	if n == 1 {
		closes = tt.bodies[0].closes.Load()
	}
	tt.mu.Unlock()
	if n != 1 || closes != 1 {
		t.Errorf("bodies = %d, body Close calls = %d; want 1 and 1", n, closes)
	}
	if !srcWaitUntil(t, 3*time.Second, func() bool { return s.closed.Load() >= 1 }) {
		t.Error("the server never saw the connection close: the download was not stopped")
	}
}

func TestSource_CloseUnblocksARead(t *testing.T) {
	s := srcStartH(t, srcStall)
	src := srcOpenURL(t, s, "/stall")
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(src); done <- err }()
	time.Sleep(100 * time.Millisecond)

	_ = src.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Error("read ended without an error after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read still blocked 3s after Close")
	}
}

func TestSource_ContextCancelMidReadReturnsCanceledPromptly(t *testing.T) {
	s := srcStartH(t, srcStall)
	ctx, cancel := context.WithCancel(t.Context())
	src, err := openInput(ctx, s.cl, s.url("/stall?token="+srcSecret))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	_, err = io.ReadAll(src)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, errIdleTimeout) {
		t.Error("cancellation reported as idle timeout")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cancel took %v to surface", d)
	}
	srcNoLeak(t, err)
}

// --- goroutine leaks ---

func TestSource_NoGoroutineLeakAfterClose(t *testing.T) {
	srcLeakCheck(t, runtime.NumGoroutine()) // registered first: runs after the server cleanup
	z := srcGzip(t, srcRepeated(2000))
	var hits atomic.Int32
	s := srcStartH(t, func(s *srcTestServer) http.Handler {
		mux := http.NewServeMux()
		mux.Handle("/gz", srcServe(z))
		mux.Handle("/plain", srcServe([]byte("a\nb\n")))
		mux.Handle("/r/", srcRedirectChain(9, &hits))
		mux.HandleFunc("/404", http.NotFound)
		mux.Handle("/stall", srcStall(s))
		mux.HandleFunc("/cut", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "50")
			_, _ = io.WriteString(w, "abc")
		})
		return mux
	})
	read := func(path string, idle time.Duration) {
		src, err := openInputIdle(t.Context(), s.cl, s.url(path), idle)
		if err != nil {
			return
		}
		_, _ = io.ReadAll(src)
		_ = src.Close()
	}
	for _, p := range []string{"/gz", "/plain", "/404", "/r/0", "/cut"} {
		read(p, time.Minute)
	}
	read("/stall", 100*time.Millisecond)

	// opened and closed without reading, and cancelled mid-read
	if src, err := openInput(t.Context(), s.cl, s.url("/gz")); err == nil {
		_ = src.Close()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if src, err := openInput(ctx, s.cl, s.url("/stall")); err == nil {
		time.AfterFunc(50*time.Millisecond, cancel)
		_, _ = io.ReadAll(src)
		_ = src.Close()
	}
}

// srcLeakCheck registers a cleanup that waits for the goroutine count to return
// to the baseline. Call it BEFORE starting servers: cleanups run last-in
// first-out, so it runs after the server and client are shut down.
func srcLeakCheck(t *testing.T, before int) {
	t.Helper()
	t.Cleanup(func() {
		if !srcWaitUntil(t, 5*time.Second, func() bool { return runtime.NumGoroutine() <= before }) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Errorf("goroutines: %d before, %d after Close and server shutdown\n%s", before, runtime.NumGoroutine(), buf)
		}
	})
}
