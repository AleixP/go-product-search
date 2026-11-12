//go:build unix

package main

import (
	"errors"
	"os"
	"runtime/debug"
	"syscall"
	"testing"
)

// srcLimitFDs lowers the soft file-descriptor limit to the current usage plus
// spare, so a descriptor that is not closed shows up within a few iterations.
func srcLimitFDs(t *testing.T, spare int) {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("cannot count open descriptors: %v", err)
	}
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Skipf("cannot read the descriptor limit: %v", err)
	}
	lowered := old
	lowered.Cur = uint64(len(entries) + spare)
	if lowered.Cur >= old.Cur {
		return // the limit is already low enough: a leak shows without lowering it
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Skipf("cannot lower the descriptor limit: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old) })
	// An unclosed *os.File is closed by its finalizer at the next GC, which would
	// hide the leak: keep the collector off while the test runs.
	prev := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(prev) })
}

// Every failure path and every normal Close must release the file: with the
// limit this low, 200 leaked descriptors would make os.Open fail.
func TestOpenInput_NeverLeaksFileDescriptors(t *testing.T) {
	dir := t.TempDir()
	magicOnly := srcWriteFile(t, "magic.gz", []byte{0x1f, 0x8b})
	good := srcWriteFile(t, "good.gz", srcGzip(t, "a\nb\n"))
	srcLimitFDs(t, 40)

	for i := range 200 {
		if _, err := openInput(t.Context(), nil, dir); err == nil || err.Error() != "input "+dir+" is a directory" {
			t.Fatalf("iteration %d, directory: err = %v", i, err)
		}
		if _, err := openInput(t.Context(), nil, magicOnly); err == nil || errors.Is(err, syscall.EMFILE) {
			t.Fatalf("iteration %d, truncated gzip header: err = %v", i, err)
		}
		s, err := openInput(t.Context(), nil, good)
		if err != nil {
			t.Fatalf("iteration %d, good file: %v", i, err)
		}
		_ = s.Close()
	}
}
