package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Close must publish the closed flag BEFORE it closes the stream: a Read that
// fails because of the teardown relies on seeing the flag to answer os.ErrClosed.
// Deterministic white-box check: the stream closer looks at the flag itself.
func TestSource_CloseSetsTheClosedFlagBeforeClosingTheStream(t *testing.T) {
	var s *source
	var flagWhenClosing bool
	closeFn := func() error {
		flagWhenClosing = s.closed.Load()
		return nil
	}
	s, err := newSource(strings.NewReader("abc"), -1, closeFn)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if !flagWhenClosing {
		t.Error("the closed flag was still false while the stream was being closed")
	}
}

// A complete body whose final EOF arrives after the watchdog cancelled the
// context during an earlier Read that still returned data: the end is the idle
// timeout (the cause), never a bare context.Canceled.
func TestIdleReader_EOFAfterAReadThatTrippedTheWatchdogIsTheIdleTimeoutCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	ir := newIdleReader(ctx, cancel, 20*time.Millisecond)
	calls := 0
	ir.r = readerFunc(func(p []byte) (int, error) {
		calls++
		if calls == 1 {
			<-ctx.Done() // the watchdog fires while this Read waits, yet data arrives
			return copy(p, "ab"), nil
		}
		return 0, io.EOF
	})

	if n, err := ir.Read(make([]byte, 10)); n != 2 || err != nil {
		t.Fatalf("first Read = %d, %v; want 2, nil", n, err)
	}
	_, err := ir.Read(make([]byte, 10))

	if !errors.Is(err, errIdleTimeout) {
		t.Errorf("final Read error = %v, want errIdleTimeout (and not context.Canceled)", err)
	}
}

// A redirect target without a host name (a port only) is refused before any
// connection: Host is ":443" there, Hostname() is empty.
func TestCheckRedirect_RefusesATargetWithoutHostName(t *testing.T) {
	for _, target := range []string{"https://:443/x", "https://:/x", "https:///x"} {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}

		err = checkRedirect(&http.Request{URL: u}, []*http.Request{{}})

		if err == nil || !strings.Contains(err.Error(), "no host") {
			t.Errorf("checkRedirect(%q) = %v, want a refusal that names the missing host", target, err)
		}
	}
}
