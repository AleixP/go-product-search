package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// unfRawServer answers every request with the given raw bytes written straight
// to the connection (so the framing is exactly what the test wrote), then closes
// the connection: that is how a server cut looks to the client.
func unfRawServer(t testing.TB, wire string) *srcTestServer {
	t.Helper()
	return srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString(wire)
		_ = buf.Flush()
		_ = conn.Close()
	}))
}

// An HTTP/1.x body with neither Content-Length nor chunked framing ends where the
// server closes the connection: a cut body and a complete one look the same, so
// a plain body of that kind is never trusted. Gzip carries its own trailer, so
// a gzip body needs no framing; HTTP/2 frames the end of the stream itself.
func TestOpenInput_UnframedPlainBodyIsAnError(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n"
	const text = "line1\nline2\n"
	tests := []struct {
		name string
		wire string
	}{
		{"cut by the server", head + "\r\n" + text},
		{"complete, Connection: close", head + "Connection: close\r\n\r\n" + text},
		{"HTTP/1.0 close-delimited", "HTTP/1.0 200 OK\r\n\r\n" + text},
		{"empty close-delimited", head + "\r\n"},
		{"one byte close-delimited", head + "\r\nx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := srcOpenURL(t, unfRawServer(t, tt.wire), "/x")

			got, err := io.ReadAll(src)

			if !errors.Is(err, errUnframedBody) {
				t.Fatalf("ReadAll error = %v, want errUnframedBody", err)
			}
			if want := strings.SplitN(tt.wire, "\r\n\r\n", 2)[1]; string(got) != want {
				t.Errorf("ReadAll data = %q, want the bytes received before the error %q", got, want)
			}
		})
	}
}

func TestOpenInput_FramedPlainBodyIsAccepted(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\n"
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"Content-Length", head + "Content-Length: 12\r\n\r\nline1\nline2\n", "line1\nline2\n"},
		{"Content-Length 0", head + "Content-Length: 0\r\n\r\n", ""},
		{"chunked", head + "Transfer-Encoding: chunked\r\n\r\n6\r\nline1\n\r\n6\r\nline2\n\r\n0\r\n\r\n", "line1\nline2\n"},
		{"empty chunked", head + "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := srcOpenURL(t, unfRawServer(t, tt.wire), "/x")

			got, err := io.ReadAll(src)

			if err != nil || string(got) != tt.want {
				t.Errorf("ReadAll = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}

// The framing check applies to plain bodies only: the gzip trailer already tells
// a complete stream from a cut one.
func TestOpenInput_UnframedGzipBodyCompleteIsAcceptedAndCutIsAnError(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Type: application/gzip\r\n\r\n"
	text := srcRepeated(500)
	z := srcGzip(t, text)

	t.Run("complete", func(t *testing.T) {
		src := srcOpenURL(t, unfRawServer(t, head+string(z)), "/x.gz")

		got, err := io.ReadAll(src)

		if err != nil || string(got) != text {
			t.Errorf("ReadAll = %d bytes, %v; want %d bytes, nil", len(got), err, len(text))
		}
	})
	t.Run("cut", func(t *testing.T) {
		src := srcOpenURL(t, unfRawServer(t, head+string(z[:len(z)-10])), "/x.gz")

		_, err := io.ReadAll(src)

		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("ReadAll error = %v, want io.ErrUnexpectedEOF from the gzip layer", err)
		}
		if errors.Is(err, errUnframedBody) {
			t.Errorf("a gzip body must not be judged by the framing check: %v", err)
		}
	})
}

// HTTP/2 ends a body with END_STREAM, so a response with no Content-Length is
// fully framed there.
func TestOpenInput_HTTP2BodyWithoutContentLengthIsAccepted(t *testing.T) {
	var protoMajor atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protoMajor.Store(int32(r.ProtoMajor))
		_, _ = io.WriteString(w, "line1\n")
		w.(http.Flusher).Flush() // headers go out before the end: no Content-Length
		_, _ = io.WriteString(w, "line2\n")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	cl := newHTTPClient()
	tr := cl.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	t.Cleanup(tr.CloseIdleConnections)

	src, err := openInput(t.Context(), cl, srv.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	got, err := io.ReadAll(src)

	if err != nil || string(got) != "line1\nline2\n" {
		t.Errorf("ReadAll = %q, %v; want the two lines and no error", got, err)
	}
	if src.Size() != -1 {
		t.Errorf("Size() = %d, want -1", src.Size())
	}
	if protoMajor.Load() != 2 {
		t.Errorf("the server saw HTTP/%d, want HTTP/2 (the test would prove nothing)", protoMajor.Load())
	}
}

// Unit check of the wrapper itself: only a clean EOF is replaced; the data that
// comes with it and every other error pass through unchanged.
func TestUnframedReader_OnlyACleanEOFIsReplaced(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		in      error
		wantErr error
	}{
		{"data with EOF keeps the data", 2, io.EOF, errUnframedBody},
		{"EOF without data", 0, io.EOF, errUnframedBody},
		{"data without error", 2, nil, nil},
		{"unexpected EOF is untouched", 0, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF},
		{"other error is untouched", 0, errSrcBoom, errSrcBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &unframedReader{r: readerFunc(func(p []byte) (int, error) { return tt.n, tt.in })}

			n, err := u.Read(make([]byte, 10))

			if n != tt.n || err != tt.wantErr {
				t.Errorf("Read = %d, %v; want %d, %v", n, err, tt.n, tt.wantErr)
			}
		})
	}
}
