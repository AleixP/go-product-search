package opensearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	osclient "github.com/opensearch-project/opensearch-go/v4"
)

// dropAfterApplying applies the swap (the alias points at to), then kills the
// connection without an answer, like a server that crashed after committing.
// Later attempts, such as the transport's retry, find the old index already
// removed from the alias and fail with HTTP 404 aliases_not_found.
func dropAfterApplying(to string) func(*fakeCluster, http.ResponseWriter, *http.Request) {
	calls := 0
	return func(f *fakeCluster, w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > 1 {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, `{"error":{"type":"aliases_not_found_exception","reason":"aliases [products] missing"},"status":404}`)
			return
		}
		f.aliased = []string{to}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
}

func TestPublish_SwapAppliedThenConnectionDroppedSucceeds(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.aliasesHook = dropAfterApplying("products-new")
	c := newTestClient(t, f)

	if err := c.Publish(context.Background(), "products-new"); err != nil {
		t.Fatalf("Publish() error = %v, want nil: the swap was applied", err)
	}
	if got, want := f.existing(), []string{"products-new"}; !slices.Equal(got, want) {
		t.Errorf("remaining indices = %v, want %v (stale index still deleted)", got, want)
	}
}

func TestPublish_SwapNotAppliedIsAPlainError(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.failPaths["/_aliases"] = true
	c := newTestClient(t, f)

	err := c.Publish(context.Background(), "products-new")
	if err == nil {
		t.Fatal("Publish() error = nil, want an error")
	}
	if errors.Is(err, ErrPublishStateUnknown) {
		t.Errorf("error = %v, want a plain error: the alias still points at the old index", err)
	}
	if got, want := f.existing(), []string{"products-new", "products-old"}; !slices.Equal(got, want) {
		t.Errorf("indices = %v, want %v (nothing deleted)", got, want)
	}
}

func TestPublish_FirstRunSwapNotAppliedIsAPlainError(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-new"}
	f.failPaths["/_aliases"] = true
	c := newTestClient(t, f)

	err := c.Publish(context.Background(), "products-new")
	if err == nil || errors.Is(err, ErrPublishStateUnknown) {
		t.Errorf("Publish() error = %v, want a plain error (no alias before, none after)", err)
	}
}

func TestPublish_StateUnknownWhenTheAliasCannotBeReRead(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.aliasesHook = func(f *fakeCluster, w http.ResponseWriter, _ *http.Request) {
		f.aliased = []string{"products-new"}
		f.failPaths["/_alias/products"] = true // the re-read fails from now on
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"error":"boom","status":500}`)
	}
	c := newTestClient(t, f)

	err := c.Publish(context.Background(), "products-new")
	if !errors.Is(err, ErrPublishStateUnknown) {
		t.Fatalf("Publish() error = %v, want ErrPublishStateUnknown", err)
	}
	if got, want := f.existing(), []string{"products-new", "products-old"}; !slices.Equal(got, want) {
		t.Errorf("indices = %v, want %v (nothing deleted when the state is unknown)", got, want)
	}
}

func TestPublish_StateUnknownWhenTheAliasVanished(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.aliasesHook = func(f *fakeCluster, w http.ResponseWriter, _ *http.Request) {
		f.aliased = nil // the remove applied, the add did not (or was undone)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"error":"boom","status":500}`)
	}
	c := newTestClient(t, f)

	err := c.Publish(context.Background(), "products-new")
	if !errors.Is(err, ErrPublishStateUnknown) {
		t.Fatalf("Publish() error = %v, want ErrPublishStateUnknown", err)
	}
}

func TestPublish_StateUnknownWhenTheAliasPointsElsewhere(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-other", "products-new"}
	f.aliased = []string{"products-old"}
	f.aliasesHook = func(f *fakeCluster, w http.ResponseWriter, _ *http.Request) {
		f.aliased = []string{"products-other"} // somebody else moved it
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"error":"boom","status":500}`)
	}
	c := newTestClient(t, f)

	if err := c.Publish(context.Background(), "products-new"); !errors.Is(err, ErrPublishStateUnknown) {
		t.Fatalf("Publish() error = %v, want ErrPublishStateUnknown", err)
	}
}

func TestPublish_UnknownErrorKeepsTheCauseAndNoURL(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.aliasesHook = func(f *fakeCluster, w http.ResponseWriter, _ *http.Request) {
		f.aliased = nil
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"error":"boom","status":500}`)
	}
	c := newTestClient(t, f)

	err := c.Publish(context.Background(), "products-new")
	if err == nil || strings.Contains(err.Error(), f.srv.URL) {
		t.Fatalf("error = %v, want one without the server URL", err)
	}
	var se *osclient.StringError
	if !errors.Is(err, ErrPublishStateUnknown) || !errors.As(err, &se) {
		t.Errorf("error = %v, want ErrPublishStateUnknown wrapping the swap cause", err)
	}
	if !strings.Contains(err.Error(), "check the products alias") {
		t.Errorf("error = %v, want advice to check the alias", err)
	}
}

func TestPublish_UnknownErrorAfterTransportFailureHasNoURL(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.aliasesHook = func(f *fakeCluster, w http.ResponseWriter, _ *http.Request) {
		f.failPaths["/_alias/products"] = true
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	c := newTestClient(t, f)

	err := c.Publish(context.Background(), "products-new")
	if !errors.Is(err, ErrPublishStateUnknown) {
		t.Fatalf("Publish() error = %v, want ErrPublishStateUnknown", err)
	}
	if strings.Contains(err.Error(), f.srv.URL) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error leaks the server address: %v", err)
	}
}

func TestPublish_CancelledContextStillResolvesTheSwap(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	ctx, cancel := context.WithCancel(context.Background())
	drop := dropAfterApplying("products-new")
	f.aliasesHook = func(f *fakeCluster, w http.ResponseWriter, r *http.Request) {
		cancel() // the run is interrupted while the swap is in flight
		drop(f, w, r)
	}
	c := newTestClient(t, f)

	if err := c.Publish(ctx, "products-new"); err != nil {
		t.Errorf("Publish() error = %v, want nil: the swap was applied and the re-read must not use the cancelled context", err)
	}
}

func TestPublish_AliasOnNewAndOtherIndicesIsSuccess(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-other", "products-new"}
	f.aliased = []string{"products-old"}
	f.aliasesHook = func(f *fakeCluster, w http.ResponseWriter, _ *http.Request) {
		f.aliased = []string{"products-new", "products-other"}
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"error":"boom","status":500}`)
	}
	c := newTestClient(t, f)

	if err := c.Publish(context.Background(), "products-new"); err != nil {
		t.Fatalf("Publish() error = %v, want nil: the alias contains the new index", err)
	}
	if got, want := f.existing(), []string{"products-new"}; !slices.Equal(got, want) {
		t.Errorf("indices = %v, want %v (the others are deleted as stale)", got, want)
	}
}

func TestPublish_RereadThatNeverAnswersIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 10 s re-read bound")
	}
	var mu sync.Mutex
	gets := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/_alias/products":
			mu.Lock()
			gets++
			n := gets
			mu.Unlock()
			if n == 1 {
				writeJSON(w, `{"products-old":{"aliases":{"products":{}}}}`)
				return
			}
			select { // never answer until the test is over
			case <-r.Context().Done():
			case <-release:
			}
		case r.Method == http.MethodPost && r.URL.Path == "/_aliases":
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close, which waits for handlers
	c, err := New(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Publish(context.Background(), "products-new") }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrPublishStateUnknown) {
			t.Fatalf("Publish() error = %v, want ErrPublishStateUnknown", err)
		}
		// The re-read is cut by its 10 s bound: not earlier, and well before 15 s.
		if elapsed := time.Since(start); elapsed < 10*time.Second || elapsed >= 15*time.Second {
			t.Errorf("Publish() took %v, want [10 s, 15 s)", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Publish() did not return within 20 s: the re-read has no time bound")
	}
}
