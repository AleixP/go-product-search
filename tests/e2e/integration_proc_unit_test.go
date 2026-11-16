package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/opensearch"
)

// These tests need no Docker: they run the test binary itself as the child
// process (the helper-process pattern) and check that the guarded launch ends
// the child when the parent goes away, however it goes away.

const (
	helperEnv = "E2E_HELPER_ROLE" // child, stubborn or parent
	pidPrefix = "child-pid="
)

var pidRE = regexp.MustCompile(pidPrefix + `(\d+)`)

func skipWithoutUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh and POSIX signals")
	}
}

// TestHelperChild is the child process of the tests below, not a test of its
// own: it only runs when helperEnv names a role.
func TestHelperChild(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "child":
		runHelperChild(false)
	case "stubborn":
		runHelperChild(true)
	case "exit3":
		runHelperChild(false)
		os.Exit(3)
	case "notify":
		runHelperNotify()
	}
}

// TestHelperParent starts a guarded child and then idles; the tests below kill
// it without any cleanup running.
func TestHelperParent(t *testing.T) {
	if os.Getenv(helperEnv) != "parent" {
		return
	}
	t.Setenv(helperEnv, "child")
	p := startGuarded(t, context.Background(), os.Args[0], "-test.run=^TestHelperChild$")
	fmt.Println(pidPrefix + strconv.Itoa(waitChildPID(t, p)))
	time.Sleep(time.Minute)
}

// startHelper starts a guarded helper child with the given role.
func startHelper(t *testing.T, ctx context.Context, role string) (*guardedProc, int) {
	t.Helper()
	t.Setenv(helperEnv, role)
	p := startGuarded(t, ctx, os.Args[0], "-test.run=^TestHelperChild$")
	return p, waitChildPID(t, p)
}

func waitChildPID(t *testing.T, p *guardedProc) int {
	t.Helper()
	deadline := time.After(listenWait)
	for {
		if m := pidRE.FindStringSubmatch(p.stderr.String()); m != nil {
			pid, _ := strconv.Atoi(m[1])
			return pid
		}
		select {
		case <-p.stderr.updated:
		case <-deadline:
			t.Fatalf("no child pid within %v\nstderr:\n%s", listenWait, p.stderr.String())
		}
	}
}

// waitGone fails unless pid disappears within d.
func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // do not leak it from a failing test
	t.Fatalf("process %d still alive %v later", pid, d)
}

func TestStartGuarded_StopEndsChildGracefully(t *testing.T) {
	skipWithoutUnix(t)
	old := stopGrace
	stopGrace = 3
	t.Cleanup(func() { stopGrace = old })
	p, pid := startHelper(t, context.Background(), "child")
	start := time.Now()
	if code := p.stop(t); code != 0 {
		t.Errorf("exit code after stop = %d, want 0 (the child handled SIGINT)", code)
	}
	// The leftover watcher must not hold the stderr pipe: Wait would then
	// block until its sleep ends.
	if d := time.Since(start); d >= 2*time.Second {
		t.Errorf("stop took %v, want well under the %ds grace", d, stopGrace)
	}
	waitGone(t, pid, time.Second)
}

func TestStartGuarded_ExitCodeIsTheChildsExitCode(t *testing.T) {
	skipWithoutUnix(t)
	p, _ := startHelper(t, context.Background(), "exit3")
	if code := p.stop(t); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestStartGuarded_ContextCancelEndsChild(t *testing.T) {
	skipWithoutUnix(t)
	ctx, cancel := context.WithCancel(context.Background())
	p, pid := startHelper(t, ctx, "child")
	cancel()
	p.waitExit(t, 10*time.Second)
	waitGone(t, pid, time.Second)
}

func TestStartGuarded_EscalatesWhenChildIgnoresSIGINT(t *testing.T) {
	skipWithoutUnix(t)
	old := stopGrace
	stopGrace = 1
	t.Cleanup(func() { stopGrace = old })
	p, pid := startHelper(t, context.Background(), "stubborn")
	start := time.Now()
	p.stop(t)
	waitGone(t, pid, time.Second)
	if d := time.Since(start); d < time.Second {
		t.Errorf("stubborn child gone after %v, before the %ds grace", d, stopGrace)
	}
}

func TestStartGuarded_ChildEndsWhenParentIsKilled(t *testing.T) {
	skipWithoutUnix(t)
	for _, sig := range []syscall.Signal{syscall.SIGKILL, syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperParent$")
			cmd.Env = append(os.Environ(), helperEnv+"=parent")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

			pid := 0
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if m := pidRE.FindStringSubmatch(sc.Text()); m != nil {
					pid, _ = strconv.Atoi(m[1])
					break
				}
			}
			if pid == 0 {
				t.Fatal("the parent did not report the child pid")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			waitGone(t, pid, 5*time.Second)
		})
	}
}

// runHelperChild prints its pid and exits on SIGINT or SIGTERM, or ignores
// SIGINT when stubborn (only SIGKILL ends it).
func runHelperChild(stubborn bool) {
	fmt.Fprintln(os.Stderr, pidPrefix+strconv.Itoa(os.Getpid()))
	if stubborn {
		signal.Ignore(os.Interrupt)
		select {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

// runHelperNotify reports "ready" once notifyInterrupt is installed, "cancelled"
// when its context is done, and then blocks: only a signal that reaches Go's
// default handling can end it.
func runHelperNotify() {
	ctx, stop := notifyInterrupt(context.Background())
	defer stop()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("cancelled")
	select {}
}

// TestNotifyInterrupt_SecondSignalKillsTheProcess runs notifyInterrupt in a
// child process: the first signal cancels the context and leaves the child
// alive, the second one ends it by that signal (the handler is gone).
func TestNotifyInterrupt_SecondSignalKillsTheProcess(t *testing.T) {
	skipWithoutUnix(t)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperChild$")
			cmd.Env = append(os.Environ(), helperEnv+"=notify")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lines := make(chan string, 8)
			scanned := make(chan struct{})
			go func() {
				defer close(scanned)
				defer close(lines)
				sc := bufio.NewScanner(out)
				for sc.Scan() {
					lines <- sc.Text()
				}
			}()
			done := make(chan struct{}) // closed when the child has been reaped
			go func() {
				<-scanned // Wait closes the pipe: read it to EOF first
				_ = cmd.Wait()
				close(done)
			}()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Errorf("helper %d not reaped after Kill", cmd.Process.Pid)
				}
			})
			wantLine := func(want string) {
				t.Helper()
				select {
				case got, ok := <-lines:
					if !ok || got != want {
						t.Fatalf("helper line = %q (open %v), want %q", got, ok, want)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("helper did not report %q within 5s", want)
				}
			}

			wantLine("ready")
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			wantLine("cancelled")
			time.Sleep(300 * time.Millisecond)
			select {
			case <-done:
				t.Fatalf("the helper ended after the first %v, want it alive", sig)
			default:
			}

			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("the helper is still alive 5s after the second %v", sig)
			}
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != sig {
				t.Errorf("helper status = %v, want killed by %v", cmd.ProcessState, sig)
			}
		})
	}
}

func TestNotifyInterrupt_SignalCancelsTheContextAndKeepsTheProcessAlive(t *testing.T) {
	skipWithoutUnix(t)
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := notifyInterrupt(context.Background())
			defer stop()
			if err := syscall.Kill(os.Getpid(), sig.(syscall.Signal)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the context was not cancelled by the signal")
			}
		})
	}
}

// TestIntegration_SkipsUnderShort runs the test binary itself with -test.short
// and checks that TestIntegration reports the skip message. That assertion is
// what catches a dropped skip. INTEGRATION_HELPER=skip-check makes
// startOpenSearch fail before it touches Docker, so a dropped skip fails this
// check without starting a container. (DOCKER_HOST alone would not do: the
// library falls back to the default socket.)
func TestIntegration_SkipsUnderShort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.short", "-test.run=^TestIntegration$", "-test.v")
	cmd.Env = append(os.Environ(), "INTEGRATION_HELPER=skip-check", "CI=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), shortSkipMessage) {
		t.Errorf("output lacks %q:\n%s", shortSkipMessage, out)
	}
}

func TestCheckContainerGone_NothingToCheck(t *testing.T) {
	checkContainerGone(t, nil)
	checkContainerGone(t, &opensearch.OpenSearchContainer{})
}
