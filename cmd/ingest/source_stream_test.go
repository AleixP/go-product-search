package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

// --- newSource: the stream layers, with injected readers ---

var errSrcBoom = errors.New("boom")

func TestNewSource_PeekErrorIsReturnedAndClosesTheStream(t *testing.T) {
	closed := 0

	s, err := newSource(iotest.ErrReader(errSrcBoom), -1, func() error { closed++; return nil })

	if s != nil || !errors.Is(err, errSrcBoom) {
		t.Fatalf("newSource = %v, %v; want nil source and the read error", s, err)
	}
	if closed != 1 {
		t.Errorf("closer called %d times on failure, want 1", closed)
	}
}

func TestNewSource_UnexpectedEOFWhilePeekingIsAnError(t *testing.T) {
	_, err := newSource(iotest.ErrReader(io.ErrUnexpectedEOF), -1, func() error { return nil })

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestNewSource_GzipHeaderErrorClosesTheStream(t *testing.T) {
	closed := 0

	_, err := newSource(bytes.NewReader([]byte{0x1f, 0x8b, 0x00}), -1, func() error { closed++; return nil })

	if err == nil || closed != 1 {
		t.Errorf("err = %v, closed = %d; want an error and 1 close", err, closed)
	}
}

func TestNewSource_PlainStreamErrorSurfacesAtTheEnd(t *testing.T) {
	r := io.MultiReader(strings.NewReader("abc\ndef"), iotest.ErrReader(io.ErrUnexpectedEOF))
	s, err := newSource(r, -1, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(s)

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF (never a clean EOF)", err)
	}
	if !strings.HasPrefix(string(got), "abc\n") {
		t.Errorf("data before the error = %q", got)
	}
}

func TestSource_CloseIsIdempotentAndRunsTheCloserOnce(t *testing.T) {
	calls := 0
	s, err := newSource(strings.NewReader("x"), 1, func() error { calls++; return errSrcBoom })
	if err != nil {
		t.Fatal(err)
	}

	e1, e2, e3 := s.Close(), s.Close(), s.Close()

	if calls != 1 {
		t.Errorf("closer called %d times, want 1", calls)
	}
	if !errors.Is(e1, errSrcBoom) || e2 != e1 || e3 != e1 {
		t.Errorf("Close results = %v, %v, %v; want the same error each time", e1, e2, e3)
	}
}

func TestSource_SizeIsWhatTheConstructorGot(t *testing.T) {
	for _, size := range []int64{-1, 0, 42} {
		s, err := newSource(strings.NewReader("x"), size, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if s.Size() != size {
			t.Errorf("Size() = %d, want %d", s.Size(), size)
		}
	}
}

// BytesRead may be polled from another goroutine while the main one reads
// (a progress report): it must be race-free (go test -race).
func TestSource_BytesReadIsSafeToPollFromAnotherGoroutine(t *testing.T) {
	z := srcGzip(t, srcRepeated(20000))
	s := srcOpenLocal(t, srcWriteFile(t, "p.gz", z))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.BytesRead()
			}
		}
	}()

	_, err := io.Copy(io.Discard, s)
	close(stop)
	wg.Wait()

	if err != nil || s.BytesRead() != int64(len(z)) {
		t.Errorf("err = %v, BytesRead = %d, want %d", err, s.BytesRead(), len(z))
	}
}

// Reading after Close must hit the closed file, not only the bufio buffer, so
// the file is big enough that the buffer cannot hold it.
func TestSource_CloseReleasesTheLocalFile(t *testing.T) {
	data := srcIncompressible(1 << 20)
	s, err := openInput(t.Context(), nil, srcWriteFile(t, "big.bin", data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(s, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	_, err = io.ReadAll(s)
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("read after Close: err = %v, want os.ErrClosed", err)
	}
}

func TestSource_CloseReleasesTheGzipLocalFile(t *testing.T) {
	z := srcGzip(t, string(srcIncompressible(1<<20)))
	s, err := openInput(t.Context(), nil, srcWriteFile(t, "big.gz", z))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(s, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = io.ReadAll(s)
	if err == nil {
		t.Error("read after Close succeeded to the end")
	}
}

// Mid-stream the count is what was pulled from the raw stream (bufio reads
// ahead), never the file size and never less than what the consumer got.
func TestSource_BytesReadMidStream(t *testing.T) {
	data := srcIncompressible(1 << 20)
	s := srcOpenLocal(t, srcWriteFile(t, "big.bin", data))

	if _, err := io.ReadFull(s, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}

	if n := s.BytesRead(); n < 100 || n >= int64(len(data)) {
		t.Errorf("BytesRead() = %d after reading 100 of %d bytes", n, len(data))
	}
}
