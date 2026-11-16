package e2e

import (
	"context"
	"testing"
)

func TestRunIngestGuarded_ReturnsExitCodeAndOutput(t *testing.T) {
	skipWithoutUnix(t)
	code, stdout, stderr := runIngestGuarded(t, context.Background(), "/bin/sh",
		"-c", "echo out; echo err 1>&2; exit 3")
	if code != 3 || stdout != "out\n" || stderr != "err\n" {
		t.Errorf("got code %d stdout %q stderr %q, want 3 %q %q", code, stdout, stderr, "out\n", "err\n")
	}
}

func TestRunIngestGuarded_SuccessIsZero(t *testing.T) {
	skipWithoutUnix(t)
	if code, _, _ := runIngestGuarded(t, context.Background(), "/bin/sh", "-c", "exit 0"); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}
