package opensearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

func canonicalBody(t *testing.T, q catalog.Query) string {
	t.Helper()
	b, err := search.Body(q)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSearch_NoBrandOrCategoryFilterMakesNoAnalyzeCall(t *testing.T) {
	e, c, _ := newEngine(t)
	q := catalog.Query{Text: "tea", Page: 1, Size: 20, Nutriscore: []string{"a", "b"}}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if got := e.kinds(); !slices.Equal(got, []string{"search"}) {
		t.Errorf("calls = %v, want [search]", got)
	}
	// Nutriscore values reach the engine unchanged.
	if got := e.of("search")[0].body; got != canonicalBody(t, q) {
		t.Errorf("search body = %s, want %s", got, canonicalBody(t, q))
	}
}

func TestSearch_BrandsOnlyAnalyzesOnceThenSearches(t *testing.T) {
	e, c, _ := newEngine(t)
	q := catalog.Query{Page: 1, Size: 20, Brands: []string{"ACME", "Bo"}}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if got := e.kinds(); !slices.Equal(got, []string{"analyze", "search"}) {
		t.Fatalf("calls = %v, want [analyze search]", got)
	}
	a := e.of("analyze")[0]
	if a.method != http.MethodGet || a.path != "/products/_analyze" {
		t.Errorf("analyze = %s %s, want GET /products/_analyze", a.method, a.path)
	}
	if want := `{"normalizer":"folded_keyword","text":["ACME","Bo"]}`; a.body != want {
		t.Errorf("analyze body = %s, want %s", a.body, want)
	}
}

func TestSearch_BothFieldsShareOneAnalyzeCallBrandsThenCategories(t *testing.T) {
	e, c, _ := newEngine(t)
	q := catalog.Query{Page: 1, Size: 20, Brands: []string{"B1"}, Categories: []string{"C1", "C2"}}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if got := e.kinds(); !slices.Equal(got, []string{"analyze", "search"}) {
		t.Fatalf("calls = %v, want [analyze search]", got)
	}
	if want := `{"normalizer":"folded_keyword","text":["B1","C1","C2"]}`; e.of("analyze")[0].body != want {
		t.Errorf("analyze body = %s, want %s", e.of("analyze")[0].body, want)
	}
}

func TestSearch_SplitsTheTokensBackPerField(t *testing.T) {
	e, c, _ := newEngine(t)
	// Fixed tokens, in text order: brands (2 values) then categories (3 values).
	e.analyze = func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) {
		writeJSON(w, tokensBody("zb", "ab", "zc", "ac", "zc"))
	}
	q := catalog.Query{Page: 1, Size: 20, Brands: []string{"1", "2"}, Categories: []string{"3", "4", "5"}}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	want := canonicalBody(t, catalog.Query{Page: 1, Size: 20, Brands: []string{"ab", "zb"}, Categories: []string{"ac", "zc"}})
	if got := e.of("search")[0].body; got != want {
		t.Errorf("search body = %s, want %s", got, want)
	}
}

func TestSearch_CategoriesOnlyAnalyzesCategories(t *testing.T) {
	e, c, _ := newEngine(t)
	q := catalog.Query{Page: 1, Size: 20, Categories: []string{"Té"}}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	an := e.of("analyze")
	if len(an) != 1 || an[0].body != `{"normalizer":"folded_keyword","text":["Té"]}` {
		t.Errorf("analyze calls = %+v, want one for the categories", an)
	}
}

func TestSearch_BodyUsesDeduplicatedSortedCanonicalValues(t *testing.T) {
	e, c, _ := newEngine(t)
	q := catalog.Query{Text: "choc", Page: 2, Size: 10, Nutriscore: []string{"e"},
		Brands: []string{"NESTLÉ", "Zed", "nestle", "ACME"}, Categories: []string{"SNACKS", "Snacks"}}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	want := canonicalBody(t, catalog.Query{Text: "choc", Page: 2, Size: 10, Nutriscore: []string{"e"},
		Brands: []string{"acme", "nestle", "zed"}, Categories: []string{"snacks"}})
	if got := e.of("search")[0].body; got != want {
		t.Errorf("search body = %s, want %s", got, want)
	}
}

func TestSearch_DoesNotMutateTheCallersQuery(t *testing.T) {
	_, c, _ := newEngine(t)
	q := catalog.Query{Page: 1, Size: 20, Brands: []string{"nestle", "NESTLÉ", "ACME"}, Categories: []string{"SNACKS", "Snacks"}, Nutriscore: []string{"a"}}
	before := catalog.Query{Page: 1, Size: 20, Brands: []string{"nestle", "NESTLÉ", "ACME"}, Categories: []string{"SNACKS", "Snacks"}, Nutriscore: []string{"a"}}
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if !reflect.DeepEqual(q, before) {
		t.Errorf("query after Search = %+v, want unchanged %+v", q, before)
	}
}

func TestSearch_TokenCountMismatchFailsWithoutSearching(t *testing.T) {
	e, c, _ := newEngine(t)
	e.analyze = func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) { writeJSON(w, tokensBody("only-one")) }
	// One token for three values across both fields: the total is what counts.
	q := catalog.Query{Page: 1, Size: 20, Brands: []string{"A", "B"}, Categories: []string{"C"}}
	_, err := c.Search(context.Background(), q)
	if want := "search products: normalize filters: got 1 tokens for 3 values"; err == nil || err.Error() != want {
		t.Errorf("Search error = %v, want %q", err, want)
	}
	if n := len(e.of("search")); n != 0 {
		t.Errorf("search calls = %d, want 0", n)
	}
}

func TestSearch_EmptyTokenIsAnErrorNamingTheField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens []string
		want   string
	}{
		{"brand", []string{"", "c"}, "search products: normalize brands: empty token"},
		{"category", []string{"b", ""}, "search products: normalize categories: empty token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, c, _ := newEngine(t)
			e.analyze = func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) {
				writeJSON(w, tokensBody(tc.tokens...))
			}
			q := catalog.Query{Page: 1, Size: 20, Brands: []string{"SECRETB"}, Categories: []string{"SECRETC"}}
			_, err := c.Search(context.Background(), q)
			if err == nil || err.Error() != tc.want {
				t.Errorf("Search error = %v, want %q", err, tc.want)
			}
			if n := len(e.of("search")); n != 0 {
				t.Errorf("search calls = %d, want 0", n)
			}
		})
	}
}

func TestSearch_SearchBadRequestKeepsOnlyStatusAndType(t *testing.T) {
	const secret = "SECRETVALUE"
	e, c, _ := newEngine(t)
	e.search = func(w http.ResponseWriter, _ *http.Request, _ []byte, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"root_cause":[{"type":"parsing_exception","reason":"bad `+secret+`"}],`+
			`"type":"search_phase_execution_exception","reason":"bad `+secret+`","caused_by":{"type":"x","reason":"`+secret+`"}},"status":400}`)
	}
	_, err := c.Search(context.Background(), catalog.Query{Page: 1, Size: 20, Brands: []string{secret}})
	if want := "search products: status 400, type search_phase_execution_exception"; err == nil || err.Error() != want {
		t.Errorf("Search error = %v, want %q", err, want)
	}
}

func TestSearch_AnalyzeBadRequestNamesTheTypeWithoutValues(t *testing.T) {
	e, c, _ := newEngine(t)
	e.analyze = func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"root_cause":[],"type":"illegal_argument_exception","reason":"cannot find SECRETVALUE"},"status":400}`)
	}
	_, err := c.Search(context.Background(), catalog.Query{Page: 1, Size: 20, Brands: []string{"SECRETVALUE"}})
	if want := "search products: normalize filters: status 400, type illegal_argument_exception"; err == nil || err.Error() != want {
		t.Errorf("Search error = %v, want %q", err, want)
	}
	if n := len(e.of("search")); n != 0 {
		t.Errorf("search calls = %d, want 0", n)
	}
}

func TestSearch_CancelledDuringAnalyzeStopsEveryCall(t *testing.T) {
	e, c, _ := newEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.analyze = func(_ http.ResponseWriter, r *http.Request, _ []string, _ int) {
		cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	_, err := c.Search(ctx, catalog.Query{Page: 1, Size: 20, Brands: []string{"B"}, Categories: []string{"C"}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Search error = %v, want one wrapping context.Canceled", err)
	}
	if got := e.kinds(); !slices.Equal(got, []string{"analyze"}) {
		t.Errorf("calls = %v, want only the analyze", got)
	}
}

func TestSearch_AlreadyCancelledContextMakesNoSearch(t *testing.T) {
	e, c, _ := newEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Search(ctx, catalog.Query{Page: 1, Size: 20, Brands: []string{"B"}, Categories: []string{"C"}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Search error = %v, want one wrapping context.Canceled", err)
	}
	if n := len(e.of("search")); n != 0 {
		t.Errorf("search calls = %d, want 0", n)
	}
}

func TestSearch_MissingAggregationsIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"absent": searchBodyWith(0, 0, false, "", ""),
		"null":   strings.Replace(searchBodyWith(0, 0, false, "", ""), `"hits":`, `"aggregations":null,"hits":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newSearchClient(t, answer(body))
			_, err := c.Search(context.Background(), okQuery)
			if want := "search products: missing aggregations"; err == nil || err.Error() != want {
				t.Errorf("Search error = %v, want %q", err, want)
			}
		})
	}
}

func TestSearch_MalformedAggregationsErrorHasNoResponseText(t *testing.T) {
	c, _ := newSearchClient(t, answer(searchBodyWith(0, 0, false, "", `{"brands":"secret-body"}`)))
	_, err := c.Search(context.Background(), okQuery)
	if err == nil || strings.Contains(err.Error(), "secret-body") || !strings.HasPrefix(err.Error(), "search products: ") {
		t.Errorf("Search error = %v, want a search products error without response text", err)
	}
}

func TestSearch_ErrorOrderKeepsAggregationsLast(t *testing.T) {
	hit := `{"_id":"1","_source":{"name":5}}`
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"shards before aggregations", searchBodyWith(0, 1, false, "", ""), "search products: 1 of 1 shards failed"},
		{"timeout before aggregations", searchBodyWith(0, 0, true, "", ""), "search products: timed out"},
		{"hits before aggregations", searchBodyWith(1, 0, false, hit, ""), "search products: decode hit 0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newSearchClient(t, answer(tt.body))
			_, err := c.Search(context.Background(), okQuery)
			if err == nil || err.Error() != tt.want {
				t.Errorf("Search error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSearch_DecodesFacetsAgainstTheCanonicalQuery(t *testing.T) {
	e, c, _ := newEngine(t)
	e.search = func(w http.ResponseWriter, _ *http.Request, _ []byte, _ int) {
		writeJSON(w, searchBodyWith(7, 0, false, "", `{`+
			`"brands":{"doc_count":9,"top":{"buckets":[{"key":"acme","doc_count":5},{"key":"nestle","doc_count":3}]},"selected":{"buckets":[{"doc_count":3}]}},`+
			`"categories":{"doc_count":9,"top":{"buckets":[{"key":"teas","doc_count":2}]}},`+
			`"nutriscore":{"doc_count":9,"grades":{"buckets":[{"doc_count":1},{"doc_count":2},{"doc_count":3},{"doc_count":4},{"doc_count":5}]}}}`))
	}
	got, err := c.Search(context.Background(), catalog.Query{Page: 1, Size: 20, Brands: []string{"NESTLÉ"}})
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	want := catalog.Facets{
		Brands:     []catalog.FacetValue{{Value: "acme", Count: 5}, {Value: "nestle", Count: 3}},
		Categories: []catalog.FacetValue{{Value: "teas", Count: 2}},
		Nutriscore: []catalog.FacetValue{{Value: "a", Count: 1}, {Value: "b", Count: 2}, {Value: "c", Count: 3}, {Value: "d", Count: 4}, {Value: "e", Count: 5}},
	}
	if got.Total != 7 || !reflect.DeepEqual(got.Facets, want) {
		t.Errorf("Search = total %d facets %+v, want total 7 facets %+v", got.Total, got.Facets, want)
	}
}
