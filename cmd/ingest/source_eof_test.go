package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// srcEOFWhenDone is a body that blocks until ctx is done and then reports a
// clean io.EOF: what net/http can do when the request is cancelled while the
// connection is closing.
type srcEOFWhenDone struct{ ctx context.Context }

func (b srcEOFWhenDone) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, io.EOF
}

// --- end of stream versus the idle watchdog ---

// A read that ends after the watchdog fired must never look like a complete
// body: the EOF may be the cancellation closing the connection.
func TestIdleReader_EOFAfterTheWatchdogFiredIsTheIdleTimeout(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	ir := newIdleReader(ctx, cancel, 20*time.Millisecond)
	ir.r = srcEOFWhenDone{ctx}

	n, err := ir.Read(make([]byte, 10))

	if n != 0 || !errors.Is(err, errIdleTimeout) {
		t.Errorf("Read = %d, %v; want 0 and errIdleTimeout (not a clean EOF)", n, err)
	}
}

// The timer function runs in its own goroutine: Stop reports false as soon as
// the timer expired, before the context is cancelled. The rule must not depend
// on the context alone.
func TestIdleReader_EOFWhileTheWatchdogIsFiringIsTheIdleTimeout(t *testing.T) {
	ctx, realCancel := context.WithCancelCause(t.Context())
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	slowCancel := func(cause error) {
		close(started)
		<-release // the context stays live while the timer function is running
		realCancel(cause)
	}
	ir := newIdleReader(ctx, slowCancel, 20*time.Millisecond)
	ir.r = readerFunc(func([]byte) (int, error) {
		<-started
		return 0, io.EOF
	})

	_, err := ir.Read(make([]byte, 10))

	if ctx.Err() != nil {
		t.Fatal("test setup: the context was cancelled before the read returned")
	}
	if !errors.Is(err, errIdleTimeout) {
		t.Errorf("Read error = %v, want errIdleTimeout", err)
	}
}

// A genuine EOF that arrives before the watchdog is a clean EOF, and the timer
// is stopped before the verdict: it cannot fire afterwards.
func TestIdleReader_GenuineEOFStaysCleanAndStopsTheTimer(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	ir := newIdleReader(ctx, cancel, time.Hour)
	ir.r = strings.NewReader("")

	_, err := ir.Read(make([]byte, 10))

	if err != io.EOF {
		t.Errorf("Read error = %v, want io.EOF", err)
	}
	if ir.timer.Stop() {
		t.Error("the watchdog timer was left pending after the read")
	}
	if ctx.Err() != nil {
		t.Errorf("context cancelled: %v", context.Cause(ctx))
	}
}

// EOF after the caller cancelled the request is not a complete body either; the
// error is the caller's cancellation, not the idle timeout.
func TestIdleReader_EOFAfterTheCallerCancelledIsCanceled(t *testing.T) {
	parent, cancelParent := context.WithCancel(t.Context())
	ctx, cancel := context.WithCancelCause(parent)
	ir := newIdleReader(ctx, cancel, time.Hour)
	ir.r = srcEOFWhenDone{ctx}
	time.AfterFunc(20*time.Millisecond, cancelParent)

	_, err := ir.Read(make([]byte, 10))

	if !errors.Is(err, context.Canceled) || errors.Is(err, errIdleTimeout) {
		t.Errorf("Read error = %v, want context.Canceled", err)
	}
}

// Data returned together with EOF after the watchdog fired is still delivered,
// with the error instead of the EOF.
func TestIdleReader_DataWithEOFAfterTheWatchdogFiredKeepsTheData(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	ir := newIdleReader(ctx, cancel, 20*time.Millisecond)
	ir.r = readerFunc(func(p []byte) (int, error) {
		<-ctx.Done()
		return copy(p, "ab"), io.EOF
	})

	n, err := ir.Read(make([]byte, 10))

	if n != 2 || !errors.Is(err, errIdleTimeout) {
		t.Errorf("Read = %d, %v; want 2 and errIdleTimeout", n, err)
	}
}

// An error that is not EOF keeps its identity, also after the watchdog fired.
func TestIdleReader_NonEOFErrorPassesThroughAfterTheWatchdogFired(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	ir := newIdleReader(ctx, cancel, 20*time.Millisecond)
	ir.r = readerFunc(func([]byte) (int, error) {
		<-ctx.Done()
		return 0, errSrcBoom
	})

	_, err := ir.Read(make([]byte, 10))

	if !errors.Is(err, errSrcBoom) || errors.Is(err, errIdleTimeout) {
		t.Errorf("Read error = %v, want errSrcBoom unchanged", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// --- Read after Close ---

// After Close no data is returned, whatever the layers below still buffer.
func TestSource_ReadAfterCloseFailsLocalPlain(t *testing.T) {
	s := srcOpenLocal(t, srcWriteFile(t, "a.bin", srcIncompressible(1<<20)))
	if _, err := io.ReadFull(s, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	n, err := s.Read(make([]byte, 10))

	if n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Errorf("Read after Close = %d, %v; want 0 and os.ErrClosed", n, err)
	}
}

func TestSource_ReadAfterCloseFailsLocalGzip(t *testing.T) {
	s := srcOpenLocal(t, srcWriteFile(t, "a.gz", srcGzip(t, srcRepeated(200000))))
	if _, err := io.ReadFull(s, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(s)

	if len(got) != 0 || !errors.Is(err, os.ErrClosed) {
		t.Errorf("ReadAll after Close = %d bytes, %v; want 0 and os.ErrClosed", len(got), err)
	}
}

func TestSource_ReadAfterCloseFailsHTTP(t *testing.T) {
	s := srcStart(t, srcServe(srcIncompressible(1<<20)))
	src := srcOpenURL(t, s, "/big")
	if _, err := io.ReadFull(src, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	n, err := src.Read(make([]byte, 10))

	if n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Errorf("Read after Close = %d, %v; want 0 and os.ErrClosed", n, err)
	}
}

// Close may be called while another goroutine is reading (it is how a blocked
// Read is unblocked). Under -race this must be clean for every layer, and the
// reader must stop with an error.
func TestSource_CloseDuringReadIsSafeAndStopsTheReader(t *testing.T) {
	text := srcRepeated(2_000_000)
	cases := map[string]string{
		"plain": srcWriteFile(t, "a.bin", srcIncompressible(32<<20)),
		"gzip":  srcWriteFile(t, "a.gz", srcGzip(t, text)),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			s := srcOpenLocal(t, path)
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				buf := make([]byte, 100)
				for first := true; ; first = false {
					_, err := s.Read(buf)
					if first {
						close(started)
					}
					if err != nil {
						done <- err
						return
					}
				}
			}()
			<-started

			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-done:
				if !errors.Is(err, os.ErrClosed) {
					t.Errorf("reader stopped with %v, want os.ErrClosed", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the reader kept reading 3s after Close")
			}
		})
	}
}

// Closing the stream under a blocked Read can make the layer below report a
// clean EOF (closing the writer end of a pipe, or an HTTP body closing before
// the context is cancelled). That EOF is not the end of the data: the reader
// must see an error, never a complete-looking stream.
func TestSource_CloseUnderABlockedReadNeverLooksLikeACleanEnd(t *testing.T) {
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("abc\n")) }()
	s, err := newSource(pr, -1, pw.Close) // closing pw makes pr.Read return io.EOF
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(s, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(s); done <- err }()
	time.Sleep(50 * time.Millisecond) // let the reader block

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, os.ErrClosed) {
			t.Errorf("ReadAll under Close = %v, want os.ErrClosed (not a clean end)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read still blocked 3s after Close")
	}
}

// srcEOFOnCloseBody serves "abc\n", then blocks until it is closed and reports
// io.EOF: what a body can do when Close runs under a blocked Read.
type srcEOFOnCloseBody struct {
	sent   atomic.Bool
	closed chan struct{}
	once   sync.Once
}

func (b *srcEOFOnCloseBody) Read(p []byte) (int, error) {
	if b.sent.CompareAndSwap(false, true) {
		return copy(p, "abc\n"), nil
	}
	<-b.closed
	return 0, io.EOF
}

func (b *srcEOFOnCloseBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type srcEOFOnCloseTransport struct{ base http.RoundTripper }

func (tt srcEOFOnCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := tt.base.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
		resp.Body = &srcEOFOnCloseBody{closed: make(chan struct{})}
	}
	return resp, err
}

// Close under a blocked Read, with the body reporting EOF as it closes: the
// reader sees os.ErrClosed, not the idle timeout (the closed watchdog timer is
// not an expired one) and not a clean end.
func TestSource_CloseUnderABlockedHTTPReadIsClosedNotIdleTimeoutNorEOF(t *testing.T) {
	s := srcStart(t, srcServe([]byte("ignored\n")))
	s.cl.Transport = srcEOFOnCloseTransport{base: s.tr}
	src := srcOpenURL(t, s, "/x")
	if _, err := io.ReadFull(src, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(src); done <- err }()
	time.Sleep(50 * time.Millisecond) // let the reader block

	_ = src.Close()

	select {
	case err := <-done:
		if !errors.Is(err, os.ErrClosed) || errors.Is(err, errIdleTimeout) {
			t.Errorf("ReadAll under Close = %v, want os.ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read still blocked 3s after Close")
	}
}
