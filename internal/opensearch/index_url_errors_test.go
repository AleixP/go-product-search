package opensearch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// urlShape matches the `"METHOD" "url": ` prefix the transport puts on errors.
var urlShape = regexp.MustCompile(`"[A-Z]+" "`)

// requestPathParts are fragments of the request paths of every adapter call.
var requestPathParts = []string{"/_alias", "/_aliases", "/_bulk", "/_count", "/_settings", "/_refresh", "/products"}

// assertNoRequestURL fails when text carries the request URL in any form.
func assertNoRequestURL(t *testing.T, what, text, host string) {
	t.Helper()
	// The dial error of the net package names the address it could not reach;
	// that is the cause itself, not the request URL, so it is not a leak.
	text = strings.ReplaceAll(text, "dial tcp "+host, "dial tcp")
	for _, bad := range append([]string{host, "127.0.0.1", "http://"}, requestPathParts...) {
		if strings.Contains(text, bad) {
			t.Errorf("%s carries the request URL (%q found): %s", what, bad, text)
		}
	}
	if urlShape.MatchString(text) {
		t.Errorf("%s has the %q shape: %s", what, `"METHOD" "url"`, text)
	}
}

// causeKept reports whether err still wraps a dropped-connection or refused-connection cause.
func causeKept(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}

// dropServer serves f but closes the connection, without answering, for every
// request drop selects. n is how many requests with the same method and path
// were seen so far, this one included.
func dropServer(t *testing.T, f *fakeCluster, drop func(r *http.Request, n int) bool) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path]++
		n := seen[r.Method+" "+r.URL.Path]
		mu.Unlock()
		if drop != nil && drop(r, n) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		f.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func is(method, path string) func(*http.Request, int) bool {
	return func(r *http.Request, _ int) bool { return r.Method == method && r.URL.Path == path }
}

func TestAdapterErrorsNeverCarryTheRequestURL(t *testing.T) {
	const newIndex = "products-new"
	const createdIndex = "products-20261109t120000z"
	docs := []catalog.Product{{ID: "p1"}}

	tests := []struct {
		name string
		// drop selects the requests that lose their connection; nil with closed
		// set means the whole server is gone.
		drop   func(*http.Request, int) bool
		closed bool
		call   func(ctx context.Context, c *Client) error
		// logged: the call returns no error and the failure is logged instead.
		logged string
		// want is the operation context the error text must keep.
		want string
		// wantIs is an extra errors.Is target.
		wantIs error
	}{
		{name: "create index check", drop: is("GET", "/products"), want: `check for index "products"`,
			call: func(ctx context.Context, c *Client) error { _, err := c.CreateIndex(ctx); return err }},
		{name: "create index create", drop: is("PUT", "/"+createdIndex), want: "create index " + createdIndex,
			call: func(ctx context.Context, c *Client) error { _, err := c.CreateIndex(ctx); return err }},
		{name: "bulk", drop: is("POST", "/"+newIndex+"/_bulk"), want: "bulk index " + newIndex,
			call: func(ctx context.Context, c *Client) error { _, err := c.Bulk(ctx, newIndex, docs); return err }},
		{name: "finalize settings", drop: is("PUT", "/"+newIndex+"/_settings"), want: "set refresh interval of " + newIndex,
			call: func(ctx context.Context, c *Client) error { return c.Finalize(ctx, newIndex) }},
		{name: "finalize refresh", drop: is("POST", "/"+newIndex+"/_refresh"), want: "refresh " + newIndex,
			call: func(ctx context.Context, c *Client) error { return c.Finalize(ctx, newIndex) }},
		{name: "count", drop: is("POST", "/"+newIndex+"/_count"), want: "count " + newIndex,
			call: func(ctx context.Context, c *Client) error { _, err := c.Count(ctx, newIndex); return err }},
		{name: "publish alias read", drop: is("GET", "/_alias/products"), want: "read alias products",
			call: func(ctx context.Context, c *Client) error { return c.Publish(ctx, newIndex) }},
		{name: "publish swap", drop: is("POST", "/_aliases"), want: "swap alias products to " + newIndex,
			call: func(ctx context.Context, c *Client) error { return c.Publish(ctx, newIndex) }},
		{name: "publish swap and re-read", want: "swap alias products to " + newIndex, wantIs: ErrPublishStateUnknown,
			drop: func(r *http.Request, n int) bool {
				return r.URL.Path == "/_aliases" || (r.URL.Path == "/_alias/products" && n >= 2)
			},
			call: func(ctx context.Context, c *Client) error { return c.Publish(ctx, newIndex) }},
		{name: "publish list old indices", drop: is("GET", "/products-*"), logged: "list old indices failed",
			call: func(ctx context.Context, c *Client) error { return c.Publish(ctx, newIndex) }},
		{name: "publish delete old index", drop: is("DELETE", "/products-old"), logged: "delete old index failed",
			call: func(ctx context.Context, c *Client) error { return c.Publish(ctx, newIndex) }},
		{name: "delete", drop: is("DELETE", "/"+newIndex), want: "delete index " + newIndex,
			call: func(ctx context.Context, c *Client) error { return c.Delete(ctx, newIndex) }},

		{name: "closed create index", closed: true, want: `check for index "products"`,
			call: func(ctx context.Context, c *Client) error { _, err := c.CreateIndex(ctx); return err }},
		{name: "closed bulk", closed: true, want: "bulk index " + newIndex,
			call: func(ctx context.Context, c *Client) error { _, err := c.Bulk(ctx, newIndex, docs); return err }},
		{name: "closed finalize", closed: true, want: "set refresh interval of " + newIndex,
			call: func(ctx context.Context, c *Client) error { return c.Finalize(ctx, newIndex) }},
		{name: "closed count", closed: true, want: "count " + newIndex,
			call: func(ctx context.Context, c *Client) error { _, err := c.Count(ctx, newIndex); return err }},
		{name: "closed publish", closed: true, want: "read alias products",
			call: func(ctx context.Context, c *Client) error { return c.Publish(ctx, newIndex) }},
		{name: "closed delete", closed: true, want: "delete index " + newIndex,
			call: func(ctx context.Context, c *Client) error { return c.Delete(ctx, newIndex) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeCluster(t)
			f.aliased = []string{"products-old"}
			f.indices = []string{"products-old", newIndex}
			srv := dropServer(t, f, tt.drop)
			closeIdle := trackTransport(t)
			c, err := New(context.Background(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			c.now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
			var logs bytes.Buffer
			c.log = slog.New(slog.NewTextHandler(&logs, nil))
			if tt.closed {
				closeIdle()
				srv.Close()
			}
			host := mustHost(t, srv.URL)

			err = tt.call(context.Background(), c)

			if tt.logged != "" {
				if err != nil {
					t.Fatalf("%s: error = %v, want nil (failure is logged)", tt.name, err)
				}
				if !strings.Contains(logs.String(), tt.logged) {
					t.Fatalf("log = %q, want it to contain %q", logs.String(), tt.logged)
				}
				assertNoRequestURL(t, "log line", logs.String(), host)
				return
			}
			if err == nil {
				t.Fatalf("%s: error = nil, want a transport error", tt.name)
			}
			assertNoRequestURL(t, "error", err.Error(), host)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want the operation context %q kept", err, tt.want)
			}
			if !causeKept(err) {
				t.Errorf("error = %q, want the transport cause kept (errors.Is EOF or ECONNREFUSED)", err)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want errors.Is %v", err, tt.wantIs)
			}
		})
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestNew_PingErrorCarriesNoRequestURLShape(t *testing.T) {
	addr := closedServerURL(t)
	_, err := New(context.Background(), addr+"/tok3n-secret")
	if err == nil {
		t.Fatal("New with closed server: error = nil, want error")
	}
	// New names scheme://host on purpose; the path, method and URL shape must not appear.
	text := strings.ReplaceAll(err.Error(), "ping OpenSearch at "+addr, "ping OpenSearch at")
	assertNoRequestURL(t, "ping error", text, mustHost(t, addr))
	if !causeKept(err) {
		t.Errorf("error = %q, want the transport cause kept", err)
	}
}
