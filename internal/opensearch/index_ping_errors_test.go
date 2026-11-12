package opensearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// closedServerURL returns the address of a server that no longer listens.
func closedServerURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	return addr
}

func TestNew_PingErrorDoesNotEchoAddressPath(t *testing.T) {
	base := closedServerURL(t)
	for _, path := range []string{"/tok3n-secret", "/tok3n/a/b/c", "/tok3n-secret/"} {
		t.Run(path, func(t *testing.T) {
			_, err := New(context.Background(), base+path)
			if err == nil {
				t.Fatal("New with closed server: error = nil, want error")
			}
			if strings.Contains(err.Error(), "tok3n") {
				t.Errorf("ping error echoes the address path: %v", err)
			}
			if !strings.Contains(err.Error(), "ping OpenSearch at http://127.0.0.1:") {
				t.Errorf("ping error = %q, want the scheme and host", err)
			}
			if !strings.Contains(err.Error(), "connection refused") {
				t.Errorf("ping error = %q, want the cause kept", err)
			}
		})
	}
}

func TestNew_PingErrorKeepsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(ctx, closedServerURL(t)+"/tok3n-secret")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("New with cancelled context: error = %v, want errors.Is context.Canceled", err)
	}
	if strings.Contains(err.Error(), "tok3n") {
		t.Errorf("ping error echoes the address path: %v", err)
	}
}

func TestNew_PingErrorKeepsStatusWithoutPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	_, err := New(context.Background(), srv.URL+"/tok3n-secret")
	if err == nil {
		t.Fatal("New with a 503 ping: error = nil, want error")
	}
	if strings.Contains(err.Error(), "tok3n") {
		t.Errorf("ping error echoes the address path: %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("ping error = %q, want the HTTP status kept", err)
	}
}

func TestNew_PingTimeoutDoesNotEchoAddressPath(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 10 s")
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := New(ctx, srv.URL+"/tok3n-secret")
	if err == nil {
		t.Fatal("New against a silent server: error = nil, want error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want errors.Is context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "tok3n") {
		t.Errorf("ping error echoes the address path: %v", err)
	}
}
