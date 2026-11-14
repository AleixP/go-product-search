package httpapi

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const (
	unavailableBody = `{"error":{"code":"search_unavailable","message":"search is temporarily unavailable"}}` + "\n"
	emptyFacets     = `"facets":{"brands":[],"categories":[],"nutriscore":[]}`
)

func TestSearch_FacetsExactBodyAndFieldOrder(t *testing.T) {
	s := &fakeSearcher{result: catalog.Result{
		Total:    3,
		Products: []catalog.Product{{ID: "1", Name: "x", Brands: []string{"Acme"}, Categories: []string{"c"}, Nutriscore: "a"}},
		Facets: catalog.Facets{
			Brands:     []catalog.FacetValue{{Value: "acme", Count: 2}, {Value: "zed", Count: 1}},
			Categories: []catalog.FacetValue{{Value: "snacks", Count: 3}},
			Nutriscore: []catalog.FacetValue{{Value: "a", Count: 1}, {Value: "b", Count: 0}},
		},
	}}
	h, _ := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search")

	want := `{"total":3,"page":1,"size":20,"items":[` +
		`{"id":"1","name":"x","brands":["Acme"],"categories":["c"],"nutriscore":"a"}],` +
		`"facets":{"brands":[{"value":"acme","count":2},{"value":"zed","count":1}],` +
		`"categories":[{"value":"snacks","count":3}],` +
		`"nutriscore":[{"value":"a","count":1},{"value":"b","count":0}]}}` + "\n"
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	assertJSONHeaders(t, rec)
}

func TestSearch_FacetValuesNotHTMLEscaped(t *testing.T) {
	s := &fakeSearcher{result: catalog.Result{Facets: catalog.Facets{
		Brands: []catalog.FacetValue{{Value: "<b>&", Count: 1}},
	}}}
	h, _ := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search")

	if !strings.Contains(rec.Body.String(), `{"value":"<b>&","count":1}`) {
		t.Errorf("body = %q, want unescaped <b>& in the facet value", rec.Body.String())
	}
}

func TestSearch_NilFacetListsAreEmptyArrays(t *testing.T) {
	full := []catalog.FacetValue{{Value: "v", Count: 1}}
	tests := []struct {
		name   string
		facets catalog.Facets
		want   string
	}{
		{"all nil", catalog.Facets{}, `"facets":{"brands":[],"categories":[],"nutriscore":[]}`},
		{"only brands", catalog.Facets{Brands: full}, `"facets":{"brands":[{"value":"v","count":1}],"categories":[],"nutriscore":[]}`},
		{"only categories", catalog.Facets{Categories: full}, `"facets":{"brands":[],"categories":[{"value":"v","count":1}],"nutriscore":[]}`},
		{"only nutriscore", catalog.Facets{Nutriscore: full}, `"facets":{"brands":[],"categories":[],"nutriscore":[{"value":"v","count":1}]}`},
		{"empty non-nil", catalog.Facets{Brands: []catalog.FacetValue{}, Categories: []catalog.FacetValue{}, Nutriscore: []catalog.FacetValue{}}, emptyFacets},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestHandler(&fakeSearcher{result: catalog.Result{Facets: tt.facets}})

			rec := do(h, http.MethodGet, "/search")

			want := `{"total":0,"page":1,"size":20,"items":[],` + tt.want + "}\n"
			if rec.Body.String() != want {
				t.Errorf("body = %q, want %q", rec.Body.String(), want)
			}
		})
	}
}

func TestSearch_SearcherReceivesFilters(t *testing.T) {
	tests := []struct {
		name, target string
		want         catalog.Query
	}{
		{"repeated filters sorted", "/search?brands=b&brands=a&nutriscore=c",
			catalog.Query{Page: 1, Size: 20, Brands: []string{"a", "b"}, Nutriscore: []string{"c"}}},
		{"all fields", "/search?q=+tea+&page=2&size=5&brands=B&categories=y&categories=x&nutriscore=e",
			catalog.Query{Text: "tea", Page: 2, Size: 5, Brands: []string{"B"}, Categories: []string{"x", "y"}, Nutriscore: []string{"e"}}},
		{"categories only", "/search?categories=c",
			catalog.Query{Page: 1, Size: 20, Categories: []string{"c"}}},
		{"empty filter values are no filter", "/search?brands=&categories=+&nutriscore=",
			catalog.Query{Page: 1, Size: 20}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeSearcher{}
			h, _ := newTestHandler(s)

			rec := do(h, http.MethodGet, tt.target)

			if rec.Code != http.StatusOK || s.calls != 1 {
				t.Fatalf("status = %d, calls = %d, want 200 and 1", rec.Code, s.calls)
			}
			if !reflect.DeepEqual(s.got, tt.want) {
				t.Errorf("searcher got %#v, want %#v", s.got, tt.want)
			}
		})
	}
}

func TestSearch_FilterBadRequestsAreExactAndSkipSearcher(t *testing.T) {
	tests := []struct {
		name, target, body string
	}{
		{"named unknown parameter", "/search?brand=x",
			`{"error":{"code":"invalid_parameter","message":"parameter \"brand\" is not allowed; allowed parameters are q, page, size, brands, categories and nutriscore"}}` + "\n"},
		{"21 brands", "/search?" + brandsN(21),
			`{"error":{"code":"invalid_parameter","message":"brands accepts at most 20 values"}}` + "\n"},
		{"bad grade", "/search?nutriscore=f",
			`{"error":{"code":"invalid_parameter","message":"nutriscore values must be one of a, b, c, d, e"}}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeSearcher{}
			h, logs := newTestHandler(s)

			rec := do(h, http.MethodGet, tt.target)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if rec.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.body)
			}
			assertJSONHeaders(t, rec)
			if s.calls != 0 {
				t.Errorf("searcher called %d times, want 0", s.calls)
			}
			if logs.Len() != 0 {
				t.Errorf("log = %q, want empty", logs.String())
			}
		})
	}
}

func brandsN(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "brands=b" + string(rune('a'+i))
	}
	return strings.Join(parts, "&")
}

func TestSearch_SearcherErrorWithFiltersIsFixed503AndLogsNoFilterValue(t *testing.T) {
	s := &fakeSearcher{err: errors.New("boom engine text")}
	h, logs := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search?q=qmarker&brands=bmarker&categories=cmarker&nutriscore=c")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Body.String() != unavailableBody {
		t.Errorf("body = %q, want %q", rec.Body.String(), unavailableBody)
	}
	if s.calls != 1 {
		t.Fatalf("searcher calls = %d, want 1", s.calls)
	}
	log := logs.String()
	if !strings.Contains(log, `msg="search failed"`) || !strings.Contains(log, "boom engine text") {
		t.Errorf("log = %q, want the search failed error", log)
	}
	for _, marker := range []string{"qmarker", "bmarker", "cmarker", "nutriscore", "brands", "categories"} {
		if strings.Contains(log, marker) {
			t.Errorf("log contains %q: %q", marker, log)
		}
	}
}

func TestSearch_NutriscoreValueNeverLogged(t *testing.T) {
	// nutriscore is validated to a..e, so its value is a one-letter marker;
	// the log line must not carry the query at all.
	s := &fakeSearcher{err: errors.New("boom")}
	h, logs := newTestHandler(s)

	do(h, http.MethodGet, "/search?nutriscore=d&nutriscore=e")

	if strings.Contains(logs.String(), "d,e") || strings.Contains(logs.String(), "[d e]") || strings.Contains(logs.String(), "Nutriscore") {
		t.Errorf("log carries the nutriscore filter: %q", logs.String())
	}
}
