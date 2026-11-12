package main

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// srcTestServer is an httptest TLS server plus a client built by the
// PRODUCTION constructor newHTTPClient (so its transport settings, redirect
// policy and DisableCompression are what the tests exercise); only the root
// CA is added so the self-signed certificate is trusted.
type srcTestServer struct {
	*httptest.Server
	cl      *http.Client
	tr      *http.Transport
	closed  atomic.Int32 // server-side connections that reached StateClosed
	release chan struct{}
}

func srcStart(t testing.TB, h http.Handler) *srcTestServer {
	t.Helper()
	return srcStartH(t, func(*srcTestServer) http.Handler { return h })
}

// srcStartH is srcStart for handlers that need the server (its release channel).
func srcStartH(t testing.TB, build func(*srcTestServer) http.Handler) *srcTestServer {
	t.Helper()
	s := &srcTestServer{release: make(chan struct{})}
	s.Server = httptest.NewUnstartedServer(build(s))
	s.Config.ErrorLog = log.New(io.Discard, "", 0) // the default-client TLS test makes handshake noise
	s.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed {
			s.closed.Add(1)
		}
	}
	s.StartTLS()
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	s.cl = newHTTPClient()
	s.tr = s.cl.Transport.(*http.Transport)
	s.tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	t.Cleanup(func() {
		close(s.release)
		s.tr.CloseIdleConnections()
		s.Close()
	})
	return s
}

func (s *srcTestServer) url(path string) string { return s.URL + path }

// srcTrackTransport records every response body so a test can check it was closed.
type srcTrackTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	bodies []*srcTrackBody
}

type srcTrackBody struct {
	io.ReadCloser
	closes atomic.Int32
}

func (b *srcTrackBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

func (tt *srcTrackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := tt.base.RoundTrip(req)
	if resp != nil {
		b := &srcTrackBody{ReadCloser: resp.Body}
		resp.Body = b
		tt.mu.Lock()
		tt.bodies = append(tt.bodies, b)
		tt.mu.Unlock()
	}
	return resp, err
}

func (s *srcTestServer) track() *srcTrackTransport {
	tt := &srcTrackTransport{base: s.tr}
	s.cl.Transport = tt
	return tt
}

func srcServe(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}
}

func srcOpenURL(t testing.TB, s *srcTestServer, path string) *source {
	t.Helper()
	src, err := openInput(t.Context(), s.cl, s.url(path))
	if err != nil {
		t.Fatalf("openInput(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

func TestOpenInput_HTTPSGzipBodyGivesDecompressedLines(t *testing.T) {
	text := "code\tname\n" + srcRepeated(3000)
	z := srcGzip(t, text)
	s := srcStart(t, srcServe(z))
	src := srcOpenURL(t, s, "/products.csv.gz")

	lines, err := srcLines(t, src)

	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3001 {
		t.Fatalf("got %d lines, want 3001", len(lines))
	}
	if lines[0] != "code\tname" || lines[3000] != "line of text" {
		t.Errorf("first line %q, last line %q", lines[0], lines[3000])
	}
	if src.Size() != int64(len(z)) {
		t.Errorf("Size() = %d, want Content-Length %d", src.Size(), len(z))
	}
	if src.BytesRead() != int64(len(z)) || int64(len(text)) <= src.BytesRead() {
		t.Errorf("BytesRead() = %d, want the compressed length %d (decompressed is %d)", src.BytesRead(), len(z), len(text))
	}
}

func TestOpenInput_HTTPSPlainBody(t *testing.T) {
	s := srcStart(t, srcServe([]byte("a\nb\nc\n")))
	src := srcOpenURL(t, s, "/x.tsv")

	got, err := io.ReadAll(src)

	if err != nil || string(got) != "a\nb\nc\n" {
		t.Errorf("ReadAll = %q, %v", got, err)
	}
	if src.Size() != 6 || src.BytesRead() != 6 {
		t.Errorf("Size/BytesRead = %d/%d, want 6/6", src.Size(), src.BytesRead())
	}
}

func TestOpenInput_HTTPSEmptyBody(t *testing.T) {
	s := srcStart(t, srcServe(nil))
	src := srcOpenURL(t, s, "/empty")

	n, err := src.Read(make([]byte, 4))

	if n != 0 || err != io.EOF {
		t.Errorf("Read = %d, %v; want 0, io.EOF", n, err)
	}
	if src.Size() != 0 {
		t.Errorf("Size() = %d, want 0", src.Size())
	}
}

func TestOpenInput_UnknownLengthGivesSizeMinusOne(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "abc\n")
		w.(http.Flusher).Flush() // chunked: no Content-Length
		_, _ = io.WriteString(w, "def\n")
	}))
	src := srcOpenURL(t, s, "/chunked")

	got, err := io.ReadAll(src)

	if err != nil || string(got) != "abc\ndef\n" {
		t.Errorf("ReadAll = %q, %v", got, err)
	}
	if src.Size() != -1 {
		t.Errorf("Size() = %d, want -1", src.Size())
	}
	if src.BytesRead() != 8 {
		t.Errorf("BytesRead() = %d, want 8 (the count does not depend on the size)", src.BytesRead())
	}
}

// With DisableCompression the transport neither asks for nor undoes a
// Content-Encoding, so Content-Length is the length of the bytes we count.
func TestOpenInput_ContentEncodingGzipKeepsSizeAndCountReliable(t *testing.T) {
	z := srcGzip(t, srcRepeated(3000))
	var acceptEncoding atomic.Value
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding.Store(r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(z)))
		_, _ = w.Write(z)
	}))
	src := srcOpenURL(t, s, "/x")

	got, err := io.ReadAll(src)

	if err != nil || string(got) != srcRepeated(3000) {
		t.Fatalf("ReadAll: len %d, err %v", len(got), err)
	}
	if ae := acceptEncoding.Load(); ae != "" {
		t.Errorf("Accept-Encoding = %q, want none (DisableCompression)", ae)
	}
	if src.Size() != int64(len(z)) || src.BytesRead() != int64(len(z)) {
		t.Errorf("Size/BytesRead = %d/%d, want %d/%d", src.Size(), src.BytesRead(), len(z), len(z))
	}
}

func TestOpenInput_OnlyStatus200IsAccepted(t *testing.T) {
	for _, status := range []int{201, 204, 206, 401, 403, 404, 410, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "body with "+srcSecret)
			}))
			tt := s.track()

			src, err := openInput(t.Context(), s.cl, s.url("/data.csv.gz?token="+srcSecret))

			if err == nil {
				_ = src.Close()
				t.Fatalf("status %d accepted", status)
			}
			msg := err.Error()
			if !strings.Contains(msg, strconv.Itoa(status)) {
				t.Errorf("error %q does not name status %d", msg, status)
			}
			host := strings.TrimPrefix(s.URL, "https://")
			if !strings.Contains(msg, host+"/data.csv.gz") {
				t.Errorf("error %q does not name the redacted URL (host + path)", msg)
			}
			srcNoLeak(t, err)
			tt.mu.Lock()
			defer tt.mu.Unlock()
			if len(tt.bodies) != 1 || tt.bodies[0].closes.Load() < 1 {
				t.Errorf("the response body of a rejected status was not closed (%d bodies)", len(tt.bodies))
			}
		})
	}
}

func TestOpenInput_RedirectLoopedStatusAfterRedirectIsAnError(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/missing", http.StatusFound)
	}))

	_, err := openInput(t.Context(), s.cl, s.url("/start"))

	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404 error", err)
	}
}

// srcNoLeak fails if err carries a secret marker, a query, or the *url.Error
// wrapper (whose message embeds the full request URL).
func srcNoLeak(t testing.TB, err error) {
	t.Helper()
	var ue *url.Error
	switch {
	case err == nil:
		t.Error("expected an error")
	case strings.Contains(err.Error(), srcSecret), strings.Contains(err.Error(), "token="):
		t.Errorf("error leaks URL secrets: %q", err)
	case errors.As(err, &ue):
		t.Errorf("error chain still holds a *url.Error (request URL): %q", err)
	}
}

func TestOpenInput_UserAgentOnEveryRequestIncludingRedirects(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("User-Agent"))
		mu.Unlock()
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "x")
	}))
	src := srcOpenURL(t, s, "/start")
	_, _ = io.ReadAll(src)

	want := "go-product-search-ingest/1 (+https://github.com/AleixP/go-product-search)"
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(seen))
	}
	for i, ua := range seen {
		if ua != want {
			t.Errorf("request %d User-Agent = %q, want %q", i+1, ua, want)
		}
	}
}

// --- truncated bodies: never a clean EOF ---

func TestOpenInput_PlainBodyShorterThanContentLengthIsUnexpectedEOF(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "abc\ndef\n") // handler returns: the server drops the connection
	}))
	src := srcOpenURL(t, s, "/cut")

	got, err := srcLines(t, src)

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(got) != 2 || got[0] != "abc" || got[1] != "def" {
		t.Errorf("complete lines before the cut = %q", got)
	}
}

func TestOpenInput_ChunkedBodyCutWithoutTerminatorIsUnexpectedEOF(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nabc\n\r\n")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	src := srcOpenURL(t, s, "/cut")

	_, err := io.ReadAll(src)

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestOpenInput_HalfGzipStreamIsUnexpectedEOF(t *testing.T) {
	z := srcGzip(t, string(srcIncompressible(200_000)))
	half := z[:len(z)/2]
	tests := map[string]http.HandlerFunc{
		"content-length matches the half": srcServe(half),
		"content-length of the full file": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(z)))
			_, _ = w.Write(half)
		},
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			s := srcStart(t, h)
			src := srcOpenURL(t, s, "/half.gz")

			_, err := io.ReadAll(src)

			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestOpenInput_HTTPSGzipChecksumErrorIsFatal(t *testing.T) {
	z := srcGzip(t, srcRepeated(2000))
	z[len(z)-8] ^= 0xff
	s := srcStart(t, srcServe(z))
	src := srcOpenURL(t, s, "/bad.gz")

	_, err := io.ReadAll(src)

	if !errors.Is(err, gzip.ErrChecksum) {
		t.Errorf("err = %v, want a gzip checksum error", err)
	}
}

func TestOpenInput_CancelledContextBeforeOpenFails(t *testing.T) {
	s := srcStart(t, srcServe([]byte("x")))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := openInput(ctx, s.cl, s.url("/x"))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	srcNoLeak(t, err)
}

// A body that dies before the first byte fails while peeking for the gzip magic:
// the error keeps its cause, names the redacted URL and the body is closed.
func TestOpenInput_BodyCutBeforeFirstByteFailsToOpen(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
	}))
	tt := s.track()

	_, err := openInput(t.Context(), s.cl, s.url("/empty-cut?token="+srcSecret))

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if want := "get " + s.URL + "/empty-cut: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q should start with %q", err, want)
	}
	srcNoLeak(t, err)
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if len(tt.bodies) != 1 || tt.bodies[0].closes.Load() < 1 {
		t.Error("the body was not closed after the failed open")
	}
}

// Signed URLs live in the query: it must reach the server untouched.
func TestOpenInput_RequestKeepsTheQuery(t *testing.T) {
	var got atomic.Value
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.RawQuery)
		_, _ = io.WriteString(w, "x")
	}))
	src := srcOpenURL(t, s, "/p?token="+srcSecret+"&b=2")
	_, _ = io.ReadAll(src)

	if q := got.Load(); q != "token="+srcSecret+"&b=2" {
		t.Errorf("server saw query %q", q)
	}
}

func TestOpenInput_UsesGET(t *testing.T) {
	var method atomic.Value
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method.Store(r.Method)
		_, _ = io.WriteString(w, "x")
	}))
	src := srcOpenURL(t, s, "/p")
	_, _ = io.ReadAll(src)

	if method.Load() != http.MethodGet {
		t.Errorf("method = %v, want GET", method.Load())
	}
}

// openInput (default idle timeout, 60 s) must tolerate a short pause.
func TestOpenInput_DefaultIdleTimeoutToleratesAShortPause(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, "x")
	}))
	src := srcOpenURL(t, s, "/pause")

	got, err := io.ReadAll(src)

	if err != nil || string(got) != "x" {
		t.Errorf("ReadAll = %q, %v", got, err)
	}
}
