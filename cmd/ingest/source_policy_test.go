package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// srcForeignCtx is a context that is not a *context.cancelCtx: deriving a
// cancelable child from it makes the context package start a goroutine that
// lives until the child is cancelled, so a cancel missing on a return path
// shows up as a leaked goroutine.
type srcForeignCtx struct{ done chan struct{} }

func (srcForeignCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (f srcForeignCtx) Done() <-chan struct{}     { return f.done }
func (srcForeignCtx) Err() error                  { return nil }
func (srcForeignCtx) Value(any) any               { return nil }

func TestOpenInput_ForeignParentContextNoGoroutineLeakOnAnyReturnPath(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/404" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "a\n")
	}))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := l.Addr().String()
	_ = l.Close()
	// Warm up the transport (its own goroutines), then take the baseline.
	if src, err := openInput(t.Context(), s.cl, s.url("/ok")); err == nil {
		_, _ = io.ReadAll(src)
		_ = src.Close()
	}
	_, _ = openInput(t.Context(), s.cl, s.url("/404"))
	s.tr.CloseIdleConnections()
	time.Sleep(300 * time.Millisecond)
	base := runtime.NumGoroutine()
	settled := func() bool {
		s.tr.CloseIdleConnections()
		return srcWaitUntil(t, 3*time.Second, func() bool { return runtime.NumGoroutine() <= base })
	}
	f := srcForeignCtx{done: make(chan struct{})}
	defer close(f.done)

	src, err := openInput(f, s.cl, s.url("/ok"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(src)
	_ = src.Close()
	if !settled() {
		t.Errorf("Close leaks goroutines with a foreign parent context: %d > %d", runtime.NumGoroutine(), base)
	}

	_, _ = openInput(f, s.cl, s.url("/404"))
	if !settled() {
		t.Errorf("a status failure leaks goroutines with a foreign parent context: %d > %d", runtime.NumGoroutine(), base)
	}

	_, _ = openInput(f, s.cl, "https://"+closedAddr+"/x")
	if !settled() {
		t.Errorf("a transport failure leaks goroutines with a foreign parent context: %d > %d", runtime.NumGoroutine(), base)
	}
}

// Close while a Read is blocked: the error is the closed connection, never the
// idle timeout (the cause must not be misattributed).
func TestSource_CloseWhileBlockedIsNotTheIdleTimeout(t *testing.T) {
	s := srcStartH(t, srcStall)
	src := srcOpenURL(t, s, "/stall")
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(src); done <- err }()
	time.Sleep(100 * time.Millisecond)

	_ = src.Close()

	err := <-done
	if err == nil || errors.Is(err, errIdleTimeout) {
		t.Errorf("err after Close = %v; want an error that is not the idle timeout", err)
	}
}

// userinfo with an empty user name (":secret@") is still userinfo.
func TestClassifyInput_UserinfoWithEmptyUserNameIsRejected(t *testing.T) {
	for _, in := range []string{"https://:" + srcSecret + "@example.com/x", "https://@example.com/x"} {
		_, err := classifyInput(in)
		if err == nil {
			t.Errorf("classifyInput(%q) accepted userinfo", in)
			continue
		}
		if strings.Contains(err.Error(), srcSecret) {
			t.Errorf("error leaks the secret: %v", err)
		}
	}
}

// A value that starts with https but has no "://" is a local path.
func TestClassifyInput_LocalPathStartingWithHTTPSIsLocal(t *testing.T) {
	u, err := classifyInput("https_products.tsv")
	if err != nil || u != nil {
		t.Errorf("classifyInput = %v, %v; want a local path", u, err)
	}
}

// A header-phase stall (no response headers) fails after the idle timeout, not
// after twice it. The upper bound leaves 200 ms of scheduling slack.
func TestWatchdog_HeaderStallFailsAtTheIdleTimeout(t *testing.T) {
	s := srcStartH(t, func(s *srcTestServer) http.Handler {
		return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-s.release:
			case <-r.Context().Done():
			}
		})
	})

	start := time.Now()
	_, err := openInputIdle(context.Background(), s.cl, s.url("/stall"), 300*time.Millisecond)
	el := time.Since(start)

	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("err = %v, want errIdleTimeout", err)
	}
	if el < 250*time.Millisecond || el > 500*time.Millisecond {
		t.Errorf("header stall failed after %v, want about the 300ms idle timeout", el)
	}
}

// The read-ahead is the 64 KiB bufio buffer: after one small read of a big
// local file exactly 64 KiB were pulled from the raw stream.
func TestSource_ReadAheadIs64KiB(t *testing.T) {
	s := srcOpenLocal(t, srcWriteFile(t, "big.bin", srcIncompressible(1<<20)))
	if _, err := io.ReadFull(s, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}

	if got := s.BytesRead(); got != 64<<10 {
		t.Errorf("BytesRead = %d, want 65536 (one bufio buffer)", got)
	}
}
