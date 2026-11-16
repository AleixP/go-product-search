package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests use the test binary as a fake "ingest binary" that runs for a
// long time, to check that runIngestGuarded ends it when its own process dies
// and when ingestBound is exceeded. The helper roles below run only when
// ingestRoleEnv names them.

const (
	ingestRoleEnv = "E2E_INGEST_ROLE" // parent, bound or child
	pidFileEnv    = "E2E_INGEST_PIDFILE"
)

// TestHelperIngestChild is the stubborn fake ingest: it writes its pid to the
// file, ignores SIGINT (only the escalation to SIGKILL ends it) and idles.
func TestHelperIngestChild(t *testing.T) {
	if os.Getenv(ingestRoleEnv) != "child" {
		return
	}
	signal.Ignore(os.Interrupt)
	if err := os.WriteFile(os.Getenv(pidFileEnv), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write pid file:", err)
		os.Exit(2)
	}
	time.Sleep(time.Minute) // bounded, so a failing test cannot leak it forever
}

// TestHelperIngestParent calls runIngestGuarded on the stubborn child. Role
// "parent" blocks there until the test kills it; role "bound" shortens
// ingestBound, so the call fails by itself.
func TestHelperIngestParent(t *testing.T) {
	role := os.Getenv(ingestRoleEnv)
	if role != "parent" && role != "bound" {
		return
	}
	stopGrace = 1
	if role == "bound" {
		ingestBound = time.Second
	}
	t.Setenv(ingestRoleEnv, "child")
	runIngestGuarded(t, context.Background(), os.Args[0], "-test.run=^TestHelperIngestChild$")
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(listenWait)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no child pid in %s within %v", path, listenWait)
	return 0
}

func startIngestParent(t *testing.T, role string) (*exec.Cmd, string) {
	t.Helper()
	pidFile := t.TempDir() + "/pid"
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperIngestParent$")
	cmd.Env = append(os.Environ(), ingestRoleEnv+"="+role, pidFileEnv+"="+pidFile)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, pidFile
}

func TestRunIngestGuarded_ChildEndsWhenTheTestProcessDies(t *testing.T) {
	skipWithoutUnix(t)
	for _, sig := range []syscall.Signal{syscall.SIGKILL, syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd, pidFile := startIngestParent(t, "parent")
			pid := readPIDFile(t, pidFile)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			// The child ignores SIGINT: it takes the 1 s grace, then SIGKILL.
			waitGone(t, pid, 6*time.Second)
		})
	}
}

func TestRunIngestGuarded_FailsAndKillsTheChildPastIngestBound(t *testing.T) {
	skipWithoutUnix(t)
	cmd, pidFile := startIngestParent(t, "bound")
	pid := readPIDFile(t, pidFile)
	err := cmd.Wait()
	if err == nil {
		t.Fatal("the helper passed, want runIngestGuarded to fail past ingestBound")
	}
	if out := cmd.Stdout.(*strings.Builder).String(); !strings.Contains(out, "did not finish within") {
		t.Errorf("helper output lacks the bound message:\n%s", out)
	}
	waitGone(t, pid, 6*time.Second)
}
