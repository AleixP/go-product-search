package opensearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNew_RejectsCredentialsInAddress(t *testing.T) {
	// A live server, so that accepting the credentials would make New succeed.
	f := newFakeCluster(t)
	addr := strings.Replace(f.srv.URL, "http://", "http://admin:s3cr3t-pw@", 1)
	_, err := New(context.Background(), addr)
	if err == nil {
		t.Fatal("New with userinfo: error = nil, want error")
	}
	if strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Errorf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid OpenSearch address") {
		t.Errorf("error = %v, want an invalid-address error", err)
	}
}

func TestNew_RejectsCredentialsWithUserOnly(t *testing.T) {
	f := newFakeCluster(t)
	addr := strings.Replace(f.srv.URL, "http://", "http://s3cr3t-user@", 1)
	_, err := New(context.Background(), addr)
	if err == nil {
		t.Fatal("New with a user and no password: error = nil, want error")
	}
	if strings.Contains(err.Error(), "s3cr3t-user") {
		t.Errorf("error leaks the user name: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid OpenSearch address") {
		t.Errorf("error = %v, want an invalid-address error", err)
	}
}

func TestNew_InvalidAddressDoesNotEchoSecrets(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"empty", ""},
		{"ftp scheme", "ftp://x"},
		{"host and port without scheme", "localhost:9200"},
		{"ftp scheme with secret path", "ftp://example.invalid/s3cr3t-pw/x"},
		{"no host", "http://"},
		{"no scheme", "s3cr3t-pw"},
		{"invalid scheme with userinfo", "ftp://admin:s3cr3t-pw@example.invalid/x"},
		{"invalid scheme with query", "ftp://example.invalid/x?token=s3cr3t-pw"},
		{"invalid scheme with user only", "ftp://s3cr3t-pw@example.invalid/x"},
		{"unparsable with userinfo", "http://admin:s3cr3t-pw@example.invalid:badport/x"},
		{"no host with query", "http:///x?token=s3cr3t-pw"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(context.Background(), tt.addr)
			if err == nil {
				t.Fatalf("New(%q) error = nil, want error", tt.addr)
			}
			if strings.Contains(err.Error(), "s3cr3t-pw") {
				t.Errorf("New(%q) error leaks a secret: %v", tt.addr, err)
			}
			// Rejected by validation, before any client is built or any request is sent.
			if !strings.Contains(err.Error(), "invalid OpenSearch address") {
				t.Errorf("New(%q) error = %v, want an invalid-address error", tt.addr, err)
			}
		})
	}
}

func TestNew_UnreachableDoesNotEchoQuery(t *testing.T) {
	f := newFakeCluster(t)
	addr := f.srv.URL
	f.srv.Close()
	_, err := New(context.Background(), addr+"/?token=s3cr3t-query")
	if err == nil {
		t.Fatal("New with closed server: error = nil, want error")
	}
	if strings.Contains(err.Error(), "s3cr3t-query") {
		t.Errorf("error echoes the query string: %v", err)
	}
}

func TestNew_SilentServerFailsWithinPingTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 10 s")
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })

	// The outer deadline keeps a missing internal timeout from hanging the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := New(ctx, srv.URL)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("New against a silent server: error = nil, want error")
	}
	if elapsed < 9*time.Second || elapsed > 18*time.Second {
		t.Errorf("New failed after %v, want between 9s and 18s (pingTimeout is 10s)", elapsed)
	}
}

func TestCreateIndex_CreateFailure(t *testing.T) {
	f := newFakeCluster(t)
	f.failPaths["/products-20261109t120000z"] = true
	c := newTestClient(t, f)

	if name, err := c.CreateIndex(context.Background()); err == nil {
		t.Errorf("CreateIndex() = %q, nil, want error when create fails", name)
	}
}

func TestFinalize_SettingsFailureSkipsRefresh(t *testing.T) {
	f := newFakeCluster(t)
	f.failPaths["/products-x/_settings"] = true
	c := newTestClient(t, f)

	if err := c.Finalize(context.Background(), "products-x"); err == nil {
		t.Error("Finalize() error = nil, want error when settings PUT fails")
	}
	if n := f.count("POST /products-x/_refresh"); n != 0 {
		t.Errorf("refresh calls after failed settings = %d, want 0", n)
	}
}

func TestFinalize_RefreshFailure(t *testing.T) {
	f := newFakeCluster(t)
	f.failPaths["/products-x/_refresh"] = true
	c := newTestClient(t, f)

	if err := c.Finalize(context.Background(), "products-x"); err == nil {
		t.Error("Finalize() error = nil, want error when refresh fails")
	}
}

func TestCount_Failure(t *testing.T) {
	f := newFakeCluster(t)
	f.failPaths["/products-x/_count"] = true
	c := newTestClient(t, f)

	if n, err := c.Count(context.Background(), "products-x"); err == nil {
		t.Errorf("Count() = %d, nil, want error", n)
	}
}

func TestPublish_AliasReadFailureSendsNoSwap(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-new"}
	f.failPaths["/_alias/products"] = true
	c := newTestClient(t, f)

	if err := c.Publish(context.Background(), "products-new"); err == nil {
		t.Error("Publish() error = nil, want error when the alias read fails")
	}
	if n := f.count("POST /_aliases"); n != 0 {
		t.Errorf("alias swaps after failed read = %d, want 0", n)
	}
}

func TestDelete_APIFailure(t *testing.T) {
	f := newFakeCluster(t)
	f.failDelete["products-a"] = true
	c := newTestClient(t, f)

	if err := c.Delete(context.Background(), "products-a"); err == nil {
		t.Error("Delete() error = nil, want error when the API fails")
	}
}
