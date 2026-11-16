package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/opensearch"
)

// stopGrace is how many seconds a child gets to leave after SIGINT before the
// wrapper sends SIGKILL. A variable only so a test can shorten it.
var stopGrace = 5

// guardedScript runs "$@" (after the grace seconds in $1) and ends it when
// its own stdin reaches EOF. The test process holds the write end of that
// pipe, so the kernel closes it when the test process dies for any reason,
// including a `-test.timeout` panic or SIGKILL, where no t.Cleanup runs.
//
// The command line of the child stays "<bin> <args>". The watcher runs in a
// detached subshell (fd 3 is the saved stdin) so that killing the wrapper does
// not stop it. Both stdout and stderr of the watcher go to /dev/null: it can
// outlive the wrapper and must not hold the test's stderr pipe open.
//
// Signals: SIGINT on EOF is the graceful stop, SIGKILL follows after the
// grace. The exit code of the wrapper is the exit code of the child.
const guardedScript = `g=$1; shift
exec 3<&0 0</dev/null
"$@" 3<&- &
p=$!
( cat <&3; kill -INT $p; sleep $g; kill -KILL $p ) >/dev/null 2>&1 &
w=$!
wait $p; rc=$?
kill $w 2>/dev/null
exit $rc`

// guardedProc is a child started through guardedScript.
type guardedProc struct {
	*apiProc
	stdin  io.WriteCloser
	cancel context.CancelFunc
}

// startGuarded starts bin through guardedScript, on a pipe the test process
// keeps open. Cancelling ctx, closing the pipe (stop) or the death of the test
// process all end the child with SIGINT first. A non-Go child started from a
// non-interactive sh inherits SIGINT as ignored; the api is Go and re-enables
// it through signal.Notify. Its stderr is collected like
// startAPI does, so waitAddr works on the result.
func startGuarded(t *testing.T, ctx context.Context, bin string, args ...string) *guardedProc {
	t.Helper()
	grace := time.Duration(stopGrace) * time.Second
	ctx, cancel := context.WithTimeout(ctx, processBound)
	argv := append([]string{"-c", guardedScript, "sh", strconv.Itoa(stopGrace), bin}, args...)
	cmd := exec.CommandContext(ctx, "sh", argv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdin pipe: %v", err)
	}
	p := &guardedProc{
		apiProc: &apiProc{
			cmd:    cmd,
			stderr: &stderrBuf{updated: make(chan struct{}, 1)},
			exited: make(chan int, 1),
		},
		stdin:  stdin,
		cancel: cancel,
	}
	cmd.Stderr = p.stderr
	// Cancelling the context asks for the graceful stop; WaitDelay is the
	// last resort that kills the wrapper itself.
	cmd.Cancel = func() error { _ = stdin.Close(); return nil }
	cmd.WaitDelay = grace + 5*time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", bin, err)
	}
	go func() {
		_ = cmd.Wait()
		p.exited <- cmd.ProcessState.ExitCode()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(grace + 10*time.Second):
			t.Errorf("child process %d did not exit after the stop", cmd.Process.Pid)
		}
		cancel()
	})
	return p
}

// stop ends the child gracefully (EOF on its stdin, so SIGINT) and returns
// its exit code, failing the test when it is still running after the grace.
func (p *guardedProc) stop(t *testing.T) int {
	t.Helper()
	_ = p.stdin.Close()
	return p.waitExit(t, time.Duration(stopGrace)*time.Second+10*time.Second)
}

// notifyInterrupt returns a context cancelled by the first SIGINT or SIGTERM.
// Without a handler Go kills the test binary on SIGINT at once, before any
// t.Cleanup runs. After the first signal the handler is removed, so a second
// one kills the process as usual.
func notifyInterrupt(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	// The stop function of AfterFunc is dropped on purpose: stop runs once,
	// when ctx is done, and there is nothing to cancel before that.
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

const (
	goneWait = 10 * time.Second // termination may finish a little after Terminate returns
	gonePoll = 200 * time.Millisecond
)

// checkContainerGone reports with t.Errorf (it runs in a cleanup) when ctr
// still exists in Docker after its termination. A nil ctr, or one the library
// never created, has nothing to check. Inspect errors that say "no such
// container" mean it is gone; any other error is reported, as the check could
// not be made.
func checkContainerGone(t *testing.T, ctr *opensearch.OpenSearchContainer) {
	t.Helper()
	if ctr == nil || ctr.Container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), goneWait)
	defer cancel()
	var last string
	for {
		state, err := ctr.State(ctx)
		switch {
		case err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container"):
			return
		case err != nil:
			last = "cannot inspect: " + err.Error()
		default:
			last = "state " + string(state.Status)
		}
		select {
		case <-ctx.Done():
			t.Errorf("container %s was not removed by the cleanup (%s)", ctr.GetContainerID(), last)
			return
		case <-time.After(gonePoll):
		}
	}
}

// dockerProbeWait bounds the health probe: a daemon that accepts connections
// and never answers must give a skip (or a failure under CI), not a hang.
const dockerProbeWait = 30 * time.Second

// probeDocker runs health under a timeout. The probe runs in its own goroutine
// so that a call that ignores its context cannot hold the test; such a call is
// left behind and ends with the process. A panic in it is returned as an error.
func probeDocker(ctx context.Context, wait time.Duration, health func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("%v", r)
			}
		}()
		done <- health(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("docker did not answer within %v: %w", wait, ctx.Err())
	}
}
