package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// userAgent identifies the project on every request, redirects included.
	userAgent = "go-product-search-ingest/1 (+https://github.com/AleixP/go-product-search)"
	// maxRedirects is how many redirects a download may follow.
	maxRedirects = 5
	// idleTimeout is how long a read may wait for bytes before the request is cancelled.
	idleTimeout = 60 * time.Second
	// Connection-phase timeouts of the production transport. There is deliberately
	// no http.Client.Timeout: per the net/http docs it covers connection time,
	// redirects AND reading the response body, so it would abort a healthy
	// download of 1.5 GB. The idle-read watchdog bounds a stalled body instead.
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// errIdleTimeout is returned (wrapped) when no bytes arrived within the idle timeout.
var errIdleTimeout = errors.New("no data received before the idle timeout")

// errUnframedBody is the end of a plain HTTP/1.x body that has neither a
// Content-Length nor chunked framing, see unframedReader.
var errUnframedBody = errors.New("response has no Content-Length and no chunked framing; " +
	"a cut body cannot be told from a complete one")

// httpDialer dials with the connect timeout; it is a variable so a test can
// check its settings (a func value cannot be inspected).
var httpDialer = &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}

// inputBufBytes is the size of the bufio layer between the raw stream and gzip.
const inputBufBytes = 64 << 10

// source is the input of the ingest: the decompressed bytes of a local file or
// of an https response body. It implements io.Reader (decompressed data), so it
// plugs into newLineReader.
type source struct {
	r       io.Reader // decompressed stream: the bufio reader or the gzip reader
	counter *countingReader
	size    int64
	closeFn func() error
	once    sync.Once
	err     error
	closed  atomic.Bool
	gzipped bool        // the stream is gzip: its trailer tells a cut stream from a complete one
	idle    *idleReader // nil for a local file
}

// countingReader counts the bytes read from the raw (still compressed) stream.
// It wraps the raw stream BEFORE the bufio and gzip layers, so the count is the
// number of compressed bytes consumed from the file or the connection (bufio
// reads ahead, so it can exceed what gzip has decoded by up to one buffer). The
// counter is atomic: BytesRead may be called from another goroutine (a progress
// report) while the main goroutine reads.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// newSource layers counting, buffering and gzip detection over raw. size is the
// raw size (-1 when unknown). closeFn releases the file or the connection; it
// also runs when newSource fails, so the caller has nothing to clean up.
//
// Gzip is detected by peeking the two magic bytes 1f 8b; anything else is plain
// text. A stream shorter than two bytes (io.EOF while peeking) is plain; any
// other peek error is returned. gzip.NewReader is multistream by default: a
// concatenation of gzip members reads as one stream.
func newSource(raw io.Reader, size int64, closeFn func() error) (*source, error) {
	counter := &countingReader{r: raw}
	br := bufio.NewReaderSize(counter, inputBufBytes)
	var r io.Reader = br
	gzipped := false
	magic, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = closeFn()
		return nil, err
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			_ = closeFn()
			return nil, err
		}
		r, gzipped = gz, true
	}
	return &source{r: r, counter: counter, size: size, closeFn: closeFn, gzipped: gzipped}, nil
}

// Read reads decompressed bytes. A truncated or corrupt gzip stream, or a
// plain body cut short, ends with a non-EOF error (io.ErrUnexpectedEOF or a
// gzip error), never with a clean io.EOF. After Close it returns (0, os.ErrClosed)
// even if the layers below still hold buffered data. Close may be called from
// another goroutine while a Read is in flight (that is how a blocked Read is
// unblocked); that Read ends with os.ErrClosed, or with data it had already
// buffered, and the next Read fails. Concurrent Reads are not supported.
func (s *source) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, os.ErrClosed
	}
	n, err := s.r.Read(p)
	// Closing the stream under a blocked Read makes the layers below report
	// whatever the teardown looks like from inside: a clean EOF (the body closes
	// before the context is cancelled), context.Canceled, a closed-connection
	// error. None of those is the end of the data, and the caller asked for the
	// close, so the answer is always os.ErrClosed. closed is set before the
	// stream is closed, so it is already visible here. Fails safe: an error that
	// coincides with Close is also reported as closed.
	if err != nil && s.closed.Load() {
		err = os.ErrClosed
	}
	return n, err
}

// BytesRead returns the compressed (raw) bytes consumed so far. Safe to call
// from another goroutine.
func (s *source) BytesRead() int64 { return s.counter.n.Load() }

// Size returns the raw size in bytes: the file size or the Content-Length of
// the response; -1 when unknown.
func (s *source) Size() int64 { return s.size }

// Close releases the file or the connection. It is idempotent: the first
// call closes, every call returns the first result.
func (s *source) Close() error {
	s.closed.Store(true)
	s.once.Do(func() { s.err = s.closeFn() })
	return s.err
}

// openInput opens a local file or an https URL. It classifies input itself and
// does not rely on parseConfig having validated it. client is only used for
// URLs and must not be nil then; newHTTPClient builds the production one.
func openInput(ctx context.Context, client *http.Client, input string) (*source, error) {
	return openInputIdle(ctx, client, input, idleTimeout)
}

// openInputIdle is openInput with an injectable idle-read timeout.
func openInputIdle(
	ctx context.Context, client *http.Client, input string, idle time.Duration,
) (*source, error) {
	u, err := classifyInput(input)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return openFile(input)
	}
	return openURL(ctx, client, u, idle)
}

// openURL GETs u. Every error names at most scheme + host + path.
func openURL(
	ctx context.Context, client *http.Client, u *url.URL, idle time.Duration,
) (*source, error) {
	if client == nil {
		return nil, errors.New("no HTTP client for a URL input")
	}
	name := redactURL(u)
	ctx, cancel := context.WithCancelCause(ctx)
	// The watchdog is armed from now on: it also covers the wait for the response
	// headers (and redirects). Reads re-arm it one by one, see idleReader.
	ir := newIdleReader(ctx, cancel, idle)
	// net/http reports a request cancelled with a cause as that cause (verified:
	// Go 1.27.3 returns errIdleTimeout itself from Do and from the body Read), so
	// errors.Is(err, errIdleTimeout) works with no mapping here.
	fail := func(err error) (*source, error) {
		ir.timer.Stop()
		cancel(nil)
		return nil, fmt.Errorf("get %s: %w", name, withoutRequestURL(err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	ir.timer.Stop()
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel(nil)
		return nil, fmt.Errorf("get %s: unexpected status %d", name, resp.StatusCode)
	}
	ir.r = resp.Body
	closeFn := func() error {
		ir.timer.Stop()
		err := resp.Body.Close()
		cancel(nil)
		return err
	}
	// Size: with DisableCompression a Content-Encoding is not undone by the
	// transport, so Content-Length is the length of the bytes BytesRead counts.
	s, err := newSource(ir, resp.ContentLength, closeFn)
	if err != nil {
		return fail(err)
	}
	s.idle = ir
	// A gzip body needs no framing check: a cut stream misses its trailer and
	// fails inside the gzip layer. A plain body does, see unframedReader.
	if !s.gzipped && isUnframed(resp) {
		s.r = &unframedReader{r: s.r}
	}
	return s, nil
}

// isUnframed reports an HTTP/1.x response whose body ends where the server
// closes the connection: no Content-Length (ContentLength < 0) and no
// Transfer-Encoding (chunked is the only one net/http keeps). HTTP/2 frames the
// end of the stream itself (END_STREAM), so it never counts. HEAD, 204 and 304
// do not matter: only a GET answered with 200 gets here.
func isUnframed(resp *http.Response) bool {
	return resp.ProtoMajor == 1 && resp.ContentLength < 0 && len(resp.TransferEncoding) == 0
}

// unframedReader turns the clean io.EOF of a plain, unframed HTTP/1.x body into
// errUnframedBody. When the server closes the connection a cut body reads as a
// clean EOF, indistinguishable from a complete one, and publishing a silently
// shorter index is worse than refusing. Tradeoff: a complete unframed plain
// body is refused too (it cannot be told apart); the server must send a
// Content-Length, chunked encoding, HTTP/2 or gzip. The bytes read before the
// end are still delivered. The error surfaces at the end of the read, not at
// open, because only the end shows whether the body is trusted. An empty
// unframed body is refused for the same reason. Local files are not affected:
// their Size is known.
type unframedReader struct{ r io.Reader }

func (u *unframedReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errUnframedBody
	}
	return n, err
}

// idleReader is the idle-read watchdog around the response body. The timer is
// armed only while a Read is waiting for bytes, so a consumer that pauses
// between reads (a slow bulk request) never trips it; each Read that returns
// re-arms it for the next one. When it fires it cancels the request context
// with errIdleTimeout as the cause, which aborts the blocked read; net/http
// returns that cause as the read error. There is no goroutine of its own:
// time.AfterFunc starts one only at the moment it fires.
type idleReader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
	ctx   context.Context
}

// newIdleReader returns an idleReader with the timer already armed; the caller
// sets r once the response body exists.
func newIdleReader(
	ctx context.Context, cancel context.CancelCauseFunc, idle time.Duration,
) *idleReader {
	ir := &idleReader{idle: idle, ctx: ctx}
	ir.timer = time.AfterFunc(idle, func() { cancel(errIdleTimeout) })
	return ir
}

// Read reads from the body under the watchdog. A clean io.EOF is trusted only
// if the stream finished BEFORE the watchdog fired or the request was
// cancelled: a cancelled request can make the transport report a plain EOF
// while the connection is being torn down, and a truncated body must never
// look complete. So the timer is stopped first. Stop returns false when the
// timer had already expired (its function may still be about to cancel the
// context, so the context alone is not a reliable signal), and then the EOF
// becomes errIdleTimeout. (Close also stops the timer, so Stop is false then
// too; source.Read maps any error after Close to os.ErrClosed.) If the context
// is done for another reason (the caller cancelled, or Close ran), the EOF
// becomes the cancellation cause. A genuine EOF that arrives with the timer
// still pending and the context live stays a clean EOF; the opposite race (a
// complete body whose EOF coincides with the timer expiring) fails safe, as an
// error, never as a truncation. Errors other than EOF pass through unchanged.
func (i *idleReader) Read(p []byte) (int, error) {
	i.timer.Reset(i.idle)
	n, err := i.r.Read(p)
	stopped := i.timer.Stop()
	if errors.Is(err, io.EOF) {
		if !stopped {
			return n, errIdleTimeout
		}
		if i.ctx.Err() != nil {
			return n, context.Cause(i.ctx)
		}
	}
	return n, err
}

// newHTTPClient builds the production client: https only (also across
// redirects, at most maxRedirects), default TLS verification (never
// InsecureSkipVerify) with TLS 1.2 pinned as the minimum (Go's client default
// already, stated so it survives a change of default), no transparent
// decompression, connect/TLS/header timeouts and no Client.Timeout (see the
// constants above).
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           httpDialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			DisableCompression:    true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: checkRedirect,
	}
}

// checkRedirect allows at most maxRedirects redirects, only to https targets
// without credentials. via holds the requests already made, so the Nth
// redirect arrives with len(via) == N.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects (target %s)", maxRedirects, redactURL(req.URL))
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to %s refused: only https is allowed", redactURL(req.URL))
	}
	if req.URL.User != nil {
		return fmt.Errorf("redirect to %s refused: the target carries credentials", redactURL(req.URL))
	}
	// Hostname, not Host: "https://:443/x" has Host ":443" but no host name, and
	// the dialer would turn it into localhost.
	if req.URL.Hostname() == "" {
		return fmt.Errorf("redirect to %s refused: the target has no host", redactURL(req.URL))
	}
	return nil
}

// openFile opens a local input: a regular file, or any other non-directory
// (a pipe gives an unknown size, -1). Errors name the path and keep their cause
// (errors.Is(err, fs.ErrNotExist) works). The file is closed on every failure.
func openFile(path string) (*source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err) // the *PathError already says "open <path>"
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("input: stat %s: %w", path, err)
	}
	if st.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("input %s is a directory", path)
	}
	size := int64(-1)
	if st.Mode().IsRegular() {
		size = st.Size()
	}
	return newSource(f, size, f.Close)
}

// classifyInput decides what the --input value is. A value containing "://" is
// a URL: it must parse, use https, have a host and carry no userinfo; the
// returned *url.URL is non-nil. Any other value is a local path (nil URL, nil
// error). Errors never echo the value: it may carry credentials or a query.
func classifyInput(input string) (*url.URL, error) {
	if input == "" {
		return nil, errors.New("input must not be empty")
	}
	if !strings.Contains(input, "://") {
		return nil, nil
	}
	u, err := url.Parse(input)
	if err != nil {
		return nil, errors.New("input is not a valid URL")
	}
	// url.Parse lowercases the scheme, so HTTPS:// is accepted and HTTP:// is not.
	if u.Scheme != "https" {
		return nil, fmt.Errorf("input URL must use https, got scheme %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("input URL must not contain credentials (%s)", redactURL(u))
	}
	// Hostname, not Host: "https://:443/x" has Host ":443" but no host name.
	if u.Hostname() == "" {
		return nil, errors.New("input URL has no host")
	}
	return u, nil
}

// redactURL keeps scheme, host and path: never the userinfo, the query or the
// fragment, which is where credentials and tokens live. The decoded Path is
// used (policy: the path identifies the file and reads well).
// shortcut: a %0A in the path is printed as a real newline; switch to
// EscapedPath if these messages ever reach a line-based log parser.
func redactURL(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.Path
}

// withoutRequestURL drops the *url.Error wrapper that net/http adds to every
// client error: its message embeds the full request URL, query and userinfo
// included. The cause is kept, so errors.Is still works.
func withoutRequestURL(err error) error {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) {
			return err
		}
		err = ue.Err
	}
}
