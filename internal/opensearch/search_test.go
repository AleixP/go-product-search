package opensearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

// newSearchClient starts a fake that answers HEAD / (the ping) and hands
// POST /products/_search to onSearch, and returns a client for it.
func newSearchClient(t *testing.T, onSearch http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/" {
			return
		}
		onSearch(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("New(%q) error = %v", srv.URL, err)
	}
	return c, srv
}

func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) }
}

// noFilterAggs is a valid aggregations block for a query without filters.
const noFilterAggs = `{"brands":{"doc_count":0,"top":{"buckets":[]}},"categories":{"doc_count":0,"top":{"buckets":[]}},` +
	`"nutriscore":{"doc_count":0,"grades":{"buckets":[{"doc_count":0},{"doc_count":0},{"doc_count":0},{"doc_count":0},{"doc_count":0}]}}}`

// emptyFacets is what noFilterAggs decodes to.
var emptyFacets = catalog.Facets{
	Brands:     []catalog.FacetValue{},
	Categories: []catalog.FacetValue{},
	Nutriscore: []catalog.FacetValue{{Value: "a"}, {Value: "b"}, {Value: "c"}, {Value: "d"}, {Value: "e"}},
}

func searchBody(total, failed int, timedOut bool, hits string) string {
	return searchBodyWith(total, failed, timedOut, hits, noFilterAggs)
}

// searchBodyWith is a search response; aggs "" leaves the aggregations out.
func searchBodyWith(total, failed int, timedOut bool, hits, aggs string) string {
	to := "false"
	if timedOut {
		to = "true"
	}
	out := `{"took":1,"timed_out":` + to + `,"_shards":{"total":1,"successful":1,"failed":` + strconv.Itoa(failed) + `},` +
		`"hits":{"total":{"value":` + strconv.Itoa(total) + `,"relation":"eq"},"max_score":1.0,"hits":[` + hits + `]}`
	if aggs != "" {
		out += `,"aggregations":` + aggs
	}
	return out + `}`
}

var okQuery = catalog.Query{Text: "tea", Page: 1, Size: 20}

func TestSearch_SendsBodyToAliasSearchPath(t *testing.T) {
	var method, path string
	var got []byte
	c, _ := newSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		got, _ = io.ReadAll(r.Body)
		writeJSON(w, searchBody(0, 0, false, ""))
	})
	q := catalog.Query{Text: "tea", Page: 2, Size: 10}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	want, err := search.Body(q)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/products/_search" {
		t.Errorf("request = %s %s, want POST /products/_search", method, path)
	}
	if string(got) != string(want) {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestSearch_DecodesHits(t *testing.T) {
	hits := `{"_id":"111","_source":{"name":"Green tea","brands":["Acme","Bo"],"categories":["teas"],"nutriscore":"a"}},` +
		`{"_id":"222","_source":{"name":"Black tea","brands":["Acme"],"categories":["teas","drinks"],"nutriscore":""}}`
	c, _ := newSearchClient(t, answer(searchBody(42, 0, false, hits)))
	got, err := c.Search(context.Background(), okQuery)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	want := catalog.Result{Total: 42, Products: []catalog.Product{
		{ID: "111", Name: "Green tea", Brands: []string{"Acme", "Bo"}, Categories: []string{"teas"}, Nutriscore: "a"},
		{ID: "222", Name: "Black tea", Brands: []string{"Acme"}, Categories: []string{"teas", "drinks"}},
	}, Facets: emptyFacets}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Search = %+v, want %+v", got, want)
	}
}

func TestSearch_NilListsBecomeEmptySlices(t *testing.T) {
	hits := `{"_id":"1","_source":{"name":"x","brands":null,"nutriscore":"b"}}`
	c, _ := newSearchClient(t, answer(searchBody(1, 0, false, hits)))
	got, err := c.Search(context.Background(), okQuery)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	p := got.Products[0]
	if p.Brands == nil || len(p.Brands) != 0 || p.Categories == nil || len(p.Categories) != 0 {
		t.Errorf("Brands = %#v, Categories = %#v, want non-nil empty slices", p.Brands, p.Categories)
	}
}

func TestSearch_NoHitsGivesNonNilProducts(t *testing.T) {
	c, _ := newSearchClient(t, answer(searchBody(0, 0, false, "")))
	got, err := c.Search(context.Background(), okQuery)
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if got.Products == nil || len(got.Products) != 0 || got.Total != 0 {
		t.Errorf("Search = %#v, want Total 0 and non-nil empty Products", got)
	}
}

func TestSearch_IndexNotFoundNamesTheType(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) { notFound(w, "products") })
	_, err := c.Search(context.Background(), okQuery)
	if err == nil || !strings.Contains(err.Error(), "index_not_found_exception") {
		t.Errorf("Search error = %v, want one naming index_not_found_exception", err)
	}
}

func TestSearch_UndecodableErrorBodyIsNotLeaked(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<html>secret-body</html>")
	})
	_, err := c.Search(context.Background(), okQuery)
	if err == nil {
		t.Fatal("Search error = nil, want error")
	}
	if strings.Contains(err.Error(), "secret-body") {
		t.Errorf("error leaks the response body: %v", err)
	}
	if want := "search products: undecodable response (status 500)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSearch_StringErrorBodyIsNotLeaked(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":400,"error":"secret-body"}`)
	})
	_, err := c.Search(context.Background(), okQuery)
	if err == nil || strings.Contains(err.Error(), "secret-body") {
		t.Errorf("Search error = %v, want one without the body text", err)
	}
	if want := "search products: undecodable response (status 400)"; err != nil && err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSearch_UnknownErrorShapeIsNotLeaked(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":400,"error":{"foo":"secret-body"}}`)
	})
	_, err := c.Search(context.Background(), okQuery)
	if err == nil || strings.Contains(err.Error(), "secret-body") {
		t.Errorf("Search error = %v, want one without the body text", err)
	}
	if want := "search products: undecodable response (status 400)"; err != nil && err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSearch_MalformedSuccessBodyIsNotLeaked(t *testing.T) {
	c, _ := newSearchClient(t, answer(`{"hits": secret-body`))
	_, err := c.Search(context.Background(), okQuery)
	if err == nil {
		t.Fatal("Search error = nil, want error")
	}
	if strings.Contains(err.Error(), "secret-body") {
		t.Errorf("error leaks the response body: %v", err)
	}
	if want := "search products: undecodable response"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSearch_FailedShardsIsAnError(t *testing.T) {
	body := `{"took":1,"timed_out":false,"_shards":{"total":3,"successful":2,"failed":1},"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`
	c, _ := newSearchClient(t, answer(body))
	_, err := c.Search(context.Background(), okQuery)
	if want := "search products: 1 of 3 shards failed"; err == nil || err.Error() != want {
		t.Errorf("Search error = %v, want %q", err, want)
	}
}

func TestSearch_TimedOutIsAnError(t *testing.T) {
	c, _ := newSearchClient(t, answer(searchBody(0, 0, true, "")))
	_, err := c.Search(context.Background(), okQuery)
	if want := "search products: timed out"; err == nil || err.Error() != want {
		t.Errorf("Search error = %v, want %q", err, want)
	}
}

func TestSearch_UndecodableHitFailsWithoutSourceText(t *testing.T) {
	hits := `{"_id":"1","_source":{"name":"ok","brands":[],"categories":[],"nutriscore":""}},` +
		`{"_id":"2","_source":{"name":5,"note":"secret-body"}}`
	c, _ := newSearchClient(t, answer(searchBody(2, 0, false, hits)))
	_, err := c.Search(context.Background(), okQuery)
	if err == nil {
		t.Fatal("Search error = nil, want error")
	}
	if strings.Contains(err.Error(), "secret-body") || strings.Contains(err.Error(), "5") {
		t.Errorf("error leaks source text: %v", err)
	}
	if want := "search products: decode hit 1"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSearch_UndecodableFirstHitIsReportedAsHitZero(t *testing.T) {
	hits := `{"_id":"1","_source":{"name":5}},{"_id":"2","_source":{"name":"ok"}}`
	c, _ := newSearchClient(t, answer(searchBody(2, 0, false, hits)))
	_, err := c.Search(context.Background(), okQuery)
	if err == nil {
		t.Fatal("Search error = nil, want error")
	}
	if want := "search products: decode hit 0"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestSearch_ErrorNeverCarriesTheRequestURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c, err := New(context.Background(), srv.URL+"/p4th")
	if err != nil {
		t.Fatal(err)
	}
	host := mustHost(t, srv.URL)
	srv.Close()
	_, err = c.Search(context.Background(), okQuery)
	if err == nil {
		t.Fatal("Search error = nil, want error")
	}
	if strings.Contains(err.Error(), "p4th") {
		t.Errorf("error carries the URL path: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "search products: ") {
		t.Errorf("error = %q, want the prefix %q", err, "search products: ")
	}
	assertNoRequestURL(t, "search error", err.Error(), host)
}

func TestSearch_ContextDeadlineReturnsPromptly(t *testing.T) {
	release := make(chan struct{})
	c, _ := newSearchClient(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Search(ctx, okQuery)
	if err == nil {
		t.Fatal("Search error = nil, want error")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Search took %v, want < 1s", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, DeadlineExceeded) = false for %v", err)
	}
}

func TestSearch_ServiceUnavailableIsRetriedThenFails(t *testing.T) {
	var attempts atomic.Int32
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"root_cause":[],"type":"unavailable","reason":"busy"},"status":503}`)
	})
	_, err := c.Search(context.Background(), okQuery)
	if err == nil {
		t.Fatal("Search error = nil, want error")
	}
	if got := attempts.Load(); got != 7 {
		t.Errorf("attempts = %d, want 7 (1 try + 6 retries)", got)
	}
}
