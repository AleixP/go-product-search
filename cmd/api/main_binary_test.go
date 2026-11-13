package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// buildAPI builds the real binary outside the repo.
func buildAPI(t *testing.T) string {
	t.Helper()
	bin := t.TempDir() + "/api"
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// startBinary starts the binary against a stub OpenSearch and waits for its
// listening line.
func startBinary(t *testing.T, bin string) (cmd *exec.Cmd, addr string) {
	t.Helper()
	stub := newStubOpenSearch(t)
	cmd = exec.Command(bin, "--addr", "127.0.0.1:0", "--opensearch", stub.URL)
	var out syncBuf
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// No Wait here: tests wait in their own goroutine.
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	for i := 0; i < 400 && addr == ""; i++ {
		if m := listeningRe.FindStringSubmatch(out.String()); m != nil {
			addr = m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		_ = cmd.Process.Kill()
		t.Fatalf("no listening line: %s", out.String())
	}
	return cmd, addr
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode() // -1 when killed by a signal
	}
	return -2
}

// boundedCtx returns a context that kills the child after 10 s, so a
// regression that keeps the binary serving fails the test instead of hanging
// the package and leaving a listener on the default port.
func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestBinary_ExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildAPI(t)
	if got := exitCodeOf(exec.CommandContext(boundedCtx(t), bin, "--bogus").Run()); got != 2 {
		t.Errorf("--bogus exit = %d, want 2", got)
	}
	if got := exitCodeOf(exec.CommandContext(boundedCtx(t), bin, "-h").Run()); got != 0 {
		t.Errorf("-h exit = %d, want 0", got)
	}
	if got := exitCodeOf(exec.CommandContext(boundedCtx(t), bin, "--opensearch", "ftp://x").Run()); got != 1 {
		t.Errorf("bad address exit = %d, want 1", got)
	}
}

// One SIGINT or SIGTERM starts a clean shutdown that exits 0.
func TestBinary_FirstSignalShutsDownCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildAPI(t)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd, _ := startBinary(t, bin)
			_ = cmd.Process.Signal(sig)
			waitErr := make(chan error, 1)
			go func() { waitErr <- cmd.Wait() }()
			select {
			case err := <-waitErr:
				if got := exitCodeOf(err); got != 0 {
					t.Errorf("exit = %d (%v), want 0", got, err)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("no exit after the signal")
			}
		})
	}
}

// The first signal starts a graceful shutdown that waits for a stuck
// connection; a second one kills the process at once.
func TestBinary_SecondSignalKillsAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	cmd, addr := startBinary(t, buildAPI(t))
	// A request whose header never ends keeps the graceful shutdown waiting.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("GET /search HTTP/1.1\r\nHost: x\r\n"))
	time.Sleep(100 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGINT)
	time.Sleep(200 * time.Millisecond)
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		t.Fatalf("process exited (%v) after one signal while a connection was open", err)
	case <-time.After(300 * time.Millisecond):
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case err := <-waitErr:
		if err == nil {
			t.Error("process exited 0 after a second signal, want it killed")
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("second SIGINT did not kill the process")
	}
}

func newStubOpenSearch(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}
