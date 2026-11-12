package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCluster is an in-memory stand-in for the OpenSearch REST API. It
// records every request and answers only the calls the adapter makes.
type fakeCluster struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	reqs       []string          // "METHOD /path" per request
	bodies     map[string]string // last body per "METHOD /path"
	indices    []string          // concrete indices
	aliased    []string          // indices the products alias points at
	failDelete map[string]bool
	pingStatus int
	failPaths  map[string]bool // URL paths answered with HTTP 500
	// aliasesHook, when set, answers POST /_aliases instead of the default
	// success. It runs with the fake's lock held.
	aliasesHook func(f *fakeCluster, w http.ResponseWriter, r *http.Request)
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	f := &fakeCluster{t: t, bodies: map[string]string{}, failDelete: map[string]bool{}, failPaths: map[string]bool{}, pingStatus: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func notFound(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"error":{"root_cause":[{"type":"index_not_found_exception","reason":"no such index [`+name+`]"}],"type":"index_not_found_exception","reason":"no such index [`+name+`]"},"status":404}`)
}

func writeJSON(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, s)
}

func indexGetBody(names []string) string {
	m := map[string]any{}
	for _, n := range names {
		m[n] = map[string]any{"aliases": map[string]any{}, "mappings": map[string]any{}, "settings": map[string]any{}}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (f *fakeCluster) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, key)
	f.bodies[key] = string(body)

	if f.failPaths[r.URL.Path] {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"error":"boom","status":500}`)
		return
	}

	switch {
	case key == "HEAD /":
		w.WriteHeader(f.pingStatus)
	case r.Method == http.MethodGet && r.URL.Path == "/products":
		switch {
		case slices.Contains(f.indices, "products"):
			writeJSON(w, indexGetBody([]string{"products"}))
		case len(f.aliased) > 0:
			writeJSON(w, indexGetBody(f.aliased))
		default:
			notFound(w, "products")
		}
	case r.Method == http.MethodGet && r.URL.Path == "/products-*":
		var names []string
		for _, n := range f.indices {
			if strings.HasPrefix(n, "products-") {
				names = append(names, n)
			}
		}
		writeJSON(w, indexGetBody(names))
	case r.Method == http.MethodGet && r.URL.Path == "/_alias/products":
		if len(f.aliased) == 0 {
			notFound(w, "products")
			return
		}
		m := map[string]any{}
		for _, n := range f.aliased {
			m[n] = map[string]any{"aliases": map[string]any{"products": map[string]any{}}}
		}
		b, _ := json.Marshal(m)
		writeJSON(w, string(b))
	case key == "POST /_aliases":
		if f.aliasesHook != nil {
			f.aliasesHook(f, w, r)
			return
		}
		writeJSON(w, `{"acknowledged":true}`)
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/_settings"):
		writeJSON(w, `{"acknowledged":true}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_refresh"):
		writeJSON(w, `{"_shards":{"total":1,"successful":1,"failed":0}}`)
	case strings.HasSuffix(r.URL.Path, "/_count"):
		writeJSON(w, `{"count":42,"_shards":{"total":1,"successful":1,"skipped":0,"failed":0}}`)
	case r.Method == http.MethodPut:
		name := strings.TrimPrefix(r.URL.Path, "/")
		f.indices = append(f.indices, name)
		writeJSON(w, `{"acknowledged":true,"shards_acknowledged":true,"index":"`+name+`"}`)
	case r.Method == http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/")
		if f.failDelete[name] {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, `{"error":"boom","status":500}`)
			return
		}
		f.indices = slices.DeleteFunc(f.indices, func(n string) bool { return n == name })
		writeJSON(w, `{"acknowledged":true}`)
	default:
		f.t.Errorf("unexpected request %s", key)
		w.WriteHeader(http.StatusTeapot)
	}
}

func (f *fakeCluster) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func (f *fakeCluster) count(key string) int {
	n := 0
	for _, r := range f.requests() {
		if r == key {
			n++
		}
	}
	return n
}

func (f *fakeCluster) body(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *fakeCluster) existing() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.indices)
	slices.Sort(out)
	return out
}

func newTestClient(t *testing.T, f *fakeCluster) *Client {
	t.Helper()
	c, err := New(context.Background(), f.srv.URL)
	if err != nil {
		t.Fatalf("New(%q) error = %v", f.srv.URL, err)
	}
	c.now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	return c
}

func TestNew_PingFailure(t *testing.T) {
	f := newFakeCluster(t)
	f.pingStatus = http.StatusServiceUnavailable
	if _, err := New(context.Background(), f.srv.URL); err == nil {
		t.Fatal("New with failing ping: error = nil, want error")
	}
}

func TestCreateIndex_NameAndBody(t *testing.T) {
	f := newFakeCluster(t)
	c := newTestClient(t, f)

	name, err := c.CreateIndex(context.Background())
	if err != nil {
		t.Fatalf("CreateIndex() error = %v", err)
	}
	if want := "products-20261109t120000z"; name != want {
		t.Errorf("CreateIndex() = %q, want %q", name, want)
	}
	if got := f.body("PUT /products-20261109t120000z"); !strings.Contains(got, `"edge_ngram"`) || !strings.Contains(got, `"strict"`) {
		t.Errorf("create body does not carry the embedded mapping: %s", got)
	}
}

func TestCreateIndex_RefusesConcreteProductsIndex(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products"}
	c := newTestClient(t, f)

	if _, err := c.CreateIndex(context.Background()); err == nil {
		t.Fatal("CreateIndex() with a concrete products index: error = nil, want error")
	}
	if n := f.count("PUT /products-20261109t120000z"); n != 0 {
		t.Errorf("index was created despite the guard (%d PUTs)", n)
	}
}

func TestCreateIndex_AllowsAlias(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-20260101t000000z"}
	f.aliased = []string{"products-20260101t000000z"}
	c := newTestClient(t, f)

	if _, err := c.CreateIndex(context.Background()); err != nil {
		t.Fatalf("CreateIndex() with products as alias: error = %v", err)
	}
}

func TestFinalizeAndCount(t *testing.T) {
	f := newFakeCluster(t)
	c := newTestClient(t, f)
	ctx := context.Background()

	if err := c.Finalize(ctx, "products-x"); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if got := f.body("PUT /products-x/_settings"); !strings.Contains(got, `"refresh_interval":"1s"`) {
		t.Errorf("settings body = %q, want refresh_interval 1s", got)
	}
	if n := f.count("POST /products-x/_refresh"); n != 1 {
		t.Errorf("refresh calls = %d, want 1", n)
	}
	reqs := f.requests()
	if si, ri := slices.Index(reqs, "PUT /products-x/_settings"), slices.Index(reqs, "POST /products-x/_refresh"); si > ri {
		t.Errorf("settings must precede refresh: %v", reqs)
	}

	n, err := c.Count(ctx, "products-x")
	if err != nil || n != 42 {
		t.Errorf("Count() = %d, %v, want 42, nil", n, err)
	}
}

type aliasAction struct {
	Remove *struct{ Index, Alias string } `json:"remove"`
	Add    *struct{ Index, Alias string } `json:"add"`
}

func aliasActions(t *testing.T, body string) []aliasAction {
	t.Helper()
	var b struct {
		Actions []aliasAction `json:"actions"`
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("alias body is not JSON: %v\n%s", err, body)
	}
	return b.Actions
}

func TestPublish_FirstRunSendsAddOnly(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-new"}
	c := newTestClient(t, f)

	if err := c.Publish(context.Background(), "products-new"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if n := f.count("POST /_aliases"); n != 1 {
		t.Fatalf("alias requests = %d, want 1", n)
	}
	acts := aliasActions(t, f.body("POST /_aliases"))
	if len(acts) != 1 || acts[0].Add == nil || acts[0].Add.Index != "products-new" || acts[0].Add.Alias != "products" {
		t.Errorf("actions = %+v, want a single add of products-new", acts)
	}
}

func TestPublish_SecondRunSwapsAtomicallyAndCleansUp(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-orphan", "products-new", "unrelated"}
	f.aliased = []string{"products-old"}
	c := newTestClient(t, f)

	if err := c.Publish(context.Background(), "products-new"); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if n := f.count("POST /_aliases"); n != 1 {
		t.Fatalf("alias requests = %d, want 1 (atomic)", n)
	}
	acts := aliasActions(t, f.body("POST /_aliases"))
	if len(acts) != 2 || acts[0].Remove == nil || acts[0].Remove.Index != "products-old" || acts[0].Remove.Alias != "products" || acts[1].Add == nil || acts[1].Add.Index != "products-new" {
		t.Errorf("actions = %+v, want remove products-old then add products-new", acts)
	}
	if got, want := f.existing(), []string{"products-new", "unrelated"}; !slices.Equal(got, want) {
		t.Errorf("remaining indices = %v, want %v", got, want)
	}
	reqs := f.requests()
	if ai, di := slices.Index(reqs, "POST /_aliases"), slices.Index(reqs, "DELETE /products-old"); ai > di {
		t.Errorf("deletes must follow the alias swap: %v", reqs)
	}
}

func TestPublish_DeleteFailureDoesNotFail(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-orphan", "products-new"}
	f.aliased = []string{"products-old"}
	f.failDelete["products-old"] = true
	c := newTestClient(t, f)
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))

	if err := c.Publish(context.Background(), "products-new"); err != nil {
		t.Fatalf("Publish() error = %v, want nil (best-effort delete)", err)
	}
	if got, want := f.existing(), []string{"products-new", "products-old"}; !slices.Equal(got, want) {
		t.Errorf("remaining indices = %v, want %v (orphan still deleted)", got, want)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "products-old") {
		t.Errorf("log = %q, want a WARN naming products-old", got)
	}
}

func TestDelete(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-a", "products"}
	c := newTestClient(t, f)

	if err := c.Delete(context.Background(), "products-a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got, want := f.existing(), []string{"products"}; !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
}

func TestDelete_RefusesUnsafeNames(t *testing.T) {
	f := newFakeCluster(t)
	c := newTestClient(t, f)
	for _, name := range []string{"", "products", "other", "products-*", "products-a,products", "_all", "*", "products-a?x", "products-a/../products", "products-a b"} {
		if err := c.Delete(context.Background(), name); err == nil {
			t.Errorf("Delete(%q) error = nil, want error", name)
		}
	}
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "DELETE ") {
			t.Errorf("a delete reached the cluster: %s", r)
		}
	}
}
