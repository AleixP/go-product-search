package opensearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckHealth_AliasExistsSendsOneHeadRequest(t *testing.T) {
	var mu sync.Mutex
	var methods, uris []string
	c, _ := newSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods, uris = append(methods, r.Method), append(uris, r.RequestURI)
		mu.Unlock()
	})
	if err := c.CheckHealth(context.Background()); err != nil {
		t.Fatalf("CheckHealth error = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != http.MethodHead || uris[0] != "/_alias/products" {
		t.Errorf("fake saw %v %v, want exactly one HEAD /_alias/products", methods, uris)
	}
}

func TestCheckHealth_SendsNoRequestBeforeAliasHead(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.RequestURI)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	mu.Lock()
	seen = nil
	mu.Unlock()
	if err := c.CheckHealth(context.Background()); err != nil {
		t.Fatalf("CheckHealth error = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "HEAD /_alias/products" {
		t.Errorf("requests during CheckHealth = %q, want exactly [HEAD /_alias/products]", seen)
	}
}

func TestCheckHealth_MissingAlias(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	err := c.CheckHealth(context.Background())
	if err == nil {
		t.Fatal("CheckHealth error = nil, want an error")
	}
	if want := "check health: alias products does not exist"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestCheckHealth_OtherStatuses(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
			err := c.CheckHealth(context.Background())
			if err == nil {
				t.Fatal("CheckHealth error = nil, want an error")
			}
			if want := "check health: status " + strconv.Itoa(code); err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			if strings.Contains(err.Error(), "does not exist") {
				t.Errorf("error = %q, must not say the alias is missing", err)
			}
		})
	}
}

func TestCheckHealth_ConnectionErrorKeepsCauseDropsAddressPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c, err := New(context.Background(), srv.URL+"/tok3n")
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	srv.Close()
	err = c.CheckHealth(context.Background())
	if err == nil {
		t.Fatal("CheckHealth error = nil, want an error")
	}
	if !strings.HasPrefix(err.Error(), "check health: ") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want prefix %q and the cause kept", err, "check health: ")
	}
	if strings.Contains(err.Error(), "tok3n") || strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want no address path and no alias message", err)
	}
}

// checkHealthBounded runs CheckHealth in a goroutine and fails the test if it
// does not return within a second, so a dropped context cannot hang the suite.
func checkHealthBounded(t *testing.T, c *Client, ctx context.Context) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.CheckHealth(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("CheckHealth did not return within 1s, context not honoured")
		return nil
	}
}

func TestCheckHealth_DeadlineAgainstSilentServer(t *testing.T) {
	release := make(chan struct{})
	c, _ := newSearchClient(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := checkHealthBounded(t, c, ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CheckHealth error = %v, want DeadlineExceeded", err)
	}
	if !strings.HasPrefix(err.Error(), "check health: ") {
		t.Errorf("error = %q, want prefix %q", err, "check health: ")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("returned after %v, want under 1s", d)
	}
}

func TestCheckHealth_CancelledContext(t *testing.T) {
	c, _ := newSearchClient(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkHealthBounded(t, c, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckHealth error = %v, want Canceled", err)
	}
}
