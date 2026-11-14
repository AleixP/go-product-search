package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file holds the T-5 subtests of TestAPI_EndToEnd (filters and facets).
// It is a separate file only to keep each file under 500 lines.

// searchHead is the start of what the stub OpenSearch answers to POST
// /products/_search: two hits and an exact total of 42. The second hit has
// "brands": null and no "nutriscore", to pin the API's empty-array and
// empty-string defaults. The aggregations are appended by searchBody.
const searchHead = `{"took":1,"timed_out":false,"_shards":{"total":1,"successful":1,"skipped":0,"failed":0},` +
	`"hits":{"total":{"value":42,"relation":"eq"},"max_score":1.0,"hits":[` +
	`{"_index":"products-20240101t000000z","_id":"0046872021545","_score":1.0,"_source":{"name":"Smoked Mussels In Cottonseed Oil","brands":["Family"],"categories":["en:undefined"],"nutriscore":"a"}},` +
	`{"_index":"products-20240101t000000z","_id":"9990000000002","_score":0.5,"_source":{"name":"Green Tea <b>&","brands":null,"categories":["en:beverages","en:teas"]}}` +
	`]}`

// selectedCount is the doc_count the stub gives the i-th selected bucket of a
// facet: 5, 6, 7... so that the facet order differs from the key order.
func selectedCount(i int) int { return 5 + i }

// selectedAggs mirrors the part of the request body the stub reads: the number
// of filters of each facet's "selected" aggregation.
type selectedAggs struct {
	Aggs map[string]struct {
		Aggs struct {
			Selected struct {
				Filters struct {
					Filters []json.RawMessage `json:"filters"`
				} `json:"filters"`
			} `json:"selected"`
		} `json:"aggs"`
	} `json:"aggs"`
}

// termsAgg answers one terms facet: no top buckets, and one selected bucket
// per filter the request sent (ParseFacets requires that count).
func termsAgg(req selectedAggs, name string) string {
	n := len(req.Aggs[name].Aggs.Selected.Filters.Filters)
	buckets := make([]string, n)
	for i := range buckets {
		buckets[i] = fmt.Sprintf(`{"doc_count":%d}`, selectedCount(i))
	}
	return fmt.Sprintf(`"%s":{"doc_count":0,"top":{"buckets":[]},"selected":{"buckets":[%s]}}`, name, strings.Join(buckets, ","))
}

// searchBody builds the stub's search answer for a request body. Without
// aggregations (noAggs) it is a well-formed answer the API must refuse.
func searchBody(reqBody string, noAggs bool) string {
	if noAggs {
		return searchHead + `}`
	}
	var req selectedAggs
	_ = json.Unmarshal([]byte(reqBody), &req) // a body that does not decode yields no selected buckets
	return searchHead + `,"aggregations":{` + termsAgg(req, "brands") + `,` + termsAgg(req, "categories") + `,` +
		`"nutriscore":{"doc_count":0,"grades":{"buckets":[{"doc_count":0},{"doc_count":0},{"doc_count":0},{"doc_count":0},{"doc_count":0}]}}}}`
}

const (
	namedBrandJSON = `{"error":{"code":"invalid_parameter","message":"parameter \"brand\" is not allowed; allowed parameters are q, page, size, brands, categories and nutriscore"}}` + "\n"
	tooManyJSON    = `{"error":{"code":"invalid_parameter","message":"brands accepts at most 20 values"}}` + "\n"
	badGradeJSON   = `{"error":{"code":"invalid_parameter","message":"nutriscore values must be one of a, b, c, d, e"}}` + "\n"
	controlJSON    = `{"error":{"code":"invalid_parameter","message":"brands values must not contain control characters"}}` + "\n"
	qTwiceJSON     = `{"error":{"code":"invalid_parameter","message":"parameter q must appear at most once"}}` + "\n"

	// brandMarker is a brand value no response, log line or error may carry.
	brandMarker = "Zz-Marker-Qq7"
)

// withFacets returns wantSearchJSON with the brands and categories facets
// replaced by the given JSON arrays.
func withFacets(brands, categories string) string {
	s := strings.Replace(wantSearchJSON, `"facets":{"brands":[],"categories":[]`, `"facets":{"brands":`+brands+`,"categories":`+categories, 1)
	if s == wantSearchJSON && (brands != "[]" || categories != "[]") {
		panic("withFacets: wantSearchJSON has no empty facets to replace")
	}
	return s
}

// filterSubtests runs the filter and facet subtests. They only add requests
// the stub answers, and leave every switch cleared.
func filterSubtests(t *testing.T, stub *stubSearch, client *http.Client, base string, proc *apiProc) {
	t.Run("filters are normalized once and reach the search as post_filter", func(t *testing.T) {
		before := len(stub.analyzed())
		got := do(t, client, http.MethodGet, base+"/search?q=tea&size=2&brands=ACME&brands=acme&nutriscore=a")
		wantJSONReply(t, got, http.StatusOK, withFacets(`[{"value":"acme","count":5}]`, `[]`))

		calls := stub.analyzed()[before:]
		if len(calls) != 1 {
			t.Fatalf("stub saw %d analyze calls, want 1", len(calls))
		}
		wantAnalyze(t, calls[0], []string{"ACME", "acme"})

		body, _, _, _ := stub.last()
		for _, want := range []string{
			`"post_filter"`,
			`{"terms":{"brands.norm":["acme"]}}`,
			`{"terms":{"nutriscore":["a"]}}`,
			`{"term":{"brands.norm":"acme"}}`,
			`"track_total_hits":true`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("search body %s lacks %s", body, want)
			}
		}
		if strings.Contains(body, `"include"`) {
			t.Errorf("search body %s has an include", body)
		}
	})

	t.Run("brands and categories share one analyze call and keep their order", func(t *testing.T) {
		before := len(stub.analyzed())
		got := do(t, client, http.MethodGet, base+"/search?q=tea&size=2&brands=b&brands=a&categories=x")
		wantJSONReply(t, got, http.StatusOK,
			withFacets(`[{"value":"b","count":6},{"value":"a","count":5}]`, `[{"value":"x","count":5}]`))

		calls := stub.analyzed()[before:]
		if len(calls) != 1 {
			t.Fatalf("stub saw %d analyze calls, want 1", len(calls))
		}
		wantAnalyze(t, calls[0], []string{"a", "b", "x"})

		body, _, _, _ := stub.last()
		start := strings.Index(body, `"post_filter"`)
		end := strings.Index(body, `"aggs"`)
		if start < 0 || end < start {
			t.Fatalf("search body %s: post_filter must come before aggs", body)
		}
		const want = `"post_filter":{"bool":{"filter":[{"terms":{"brands.norm":["a","b"]}},{"terms":{"categories.norm":["x"]}}]}}`
		if got := strings.TrimSuffix(body[start:end], ","); got != want {
			t.Errorf("post_filter = %s, want %s", got, want)
		}
	})

	t.Run("no brand or category filter means no analyze call", func(t *testing.T) {
		before := len(stub.analyzed())
		for _, q := range []string{"/search?q=tea&size=2&nutriscore=a", "/search?q=tea&size=2"} {
			wantJSONReply(t, do(t, client, http.MethodGet, base+q), http.StatusOK, wantSearchJSON)
		}
		if n := len(stub.analyzed()) - before; n != 0 {
			t.Errorf("stub saw %d analyze calls, want 0", n)
		}
	})

	t.Run("invalid filters are 400 and reach neither _analyze nor _search", func(t *testing.T) {
		_, _, _, searches := stub.last()
		analyzes := len(stub.analyzed())
		many := strings.Repeat("brands=x&", 21)
		tests := []struct{ name, url, body string }{
			{"singular name", "/search?brand=x", namedBrandJSON},
			{"21 brands", "/search?" + many, tooManyJSON},
			{"grade f", "/search?nutriscore=f", badGradeJSON},
			{"control character", "/search?brands=%00", controlJSON},
			{"q twice", "/search?q=a&q=b", qTwiceJSON},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantJSONReply(t, do(t, client, http.MethodGet, base+tt.url), http.StatusBadRequest, tt.body)
			})
		}
		if _, _, _, after := stub.last(); after != searches {
			t.Errorf("stub saw %d searches for invalid requests, want 0", after-searches)
		}
		if n := len(stub.analyzed()) - analyzes; n != 0 {
			t.Errorf("stub saw %d analyze calls for invalid requests, want 0", n)
		}
	})

	t.Run("a response without aggregations is a fixed 503 and the API recovers", func(t *testing.T) {
		stub.noAggs.Store(true)
		t.Cleanup(func() { stub.noAggs.Store(false) })
		got := do(t, client, http.MethodGet, base+"/search?q=tea")
		wantJSONReply(t, got, http.StatusServiceUnavailable, unavailableJSON)
		for _, leak := range []string{"aggregations", "stub", "missing"} {
			if strings.Contains(got.body, leak) {
				t.Errorf("body leaks %q: %s", leak, got.body)
			}
		}

		stub.noAggs.Store(false)
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/search?q=tea&size=2"), http.StatusOK, wantSearchJSON)
	})

	t.Run("a failed analyze is a fixed 503 and no filter value leaks", func(t *testing.T) {
		stub.analyzeFail.Store(true)
		t.Cleanup(func() { stub.analyzeFail.Store(false) })
		_, _, _, searches := stub.last()
		analyzesBefore := len(stub.analyzed())
		got := do(t, client, http.MethodGet, base+"/search?brands="+brandMarker)
		wantJSONReply(t, got, http.StatusServiceUnavailable, unavailableJSON)
		if n := len(stub.analyzed()) - analyzesBefore; n != 1 {
			t.Errorf("stub saw %d analyze calls, want exactly 1", n)
		}
		// The stderr copy of the child may lag the response: wait, bounded,
		// for the failure log line before the absence checks.
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(proc.stderr.String(), "search failed") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !strings.Contains(proc.stderr.String(), "search failed") {
			t.Fatalf("no \"search failed\" log line within 5s:\n%s", proc.stderr.String())
		}
		if _, _, _, after := stub.last(); after != searches {
			t.Errorf("stub saw %d searches after a failed analyze, want 0", after-searches)
		}
		// Whole-word and lowercase forms: the stub quotes the text it received.
		for _, marker := range []string{brandMarker, strings.ToLower(brandMarker)} {
			if strings.Contains(got.body, marker) {
				t.Errorf("response body leaks the marker: %s", got.body)
			}
			if log := proc.stderr.String(); strings.Contains(log, marker) {
				t.Errorf("API stderr leaks the marker:\n%s", log)
			}
		}

		stub.analyzeFail.Store(false)
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/search?q=tea&size=2"), http.StatusOK, wantSearchJSON)
	})

	t.Run("HEAD with filters is 200 without a body", func(t *testing.T) {
		_, _, _, searches := stub.last()
		analyzes := len(stub.analyzed())
		got := do(t, client, http.MethodHead, base+"/search?q=tea&size=2&brands=acme&categories=x&nutriscore=a")
		if got.status != http.StatusOK || got.body != "" {
			t.Errorf("got %d %q, want 200 and no body", got.status, got.body)
		}
		if _, _, _, after := stub.last(); after-searches != 1 {
			t.Errorf("stub saw %d searches for HEAD, want 1", after-searches)
		}
		if n := len(stub.analyzed()) - analyzes; n != 1 {
			t.Errorf("stub saw %d analyze calls for HEAD, want 1", n)
		}
	})
}

// wantAnalyze checks that c is a GET _analyze for the folded_keyword
// normalizer with exactly the given text array.
func wantAnalyze(t *testing.T, c analyzeCall, text []string) {
	t.Helper()
	if c.method != http.MethodGet {
		t.Errorf("analyze method = %s, want GET", c.method)
	}
	var req struct {
		Normalizer string   `json:"normalizer"`
		Text       []string `json:"text"`
	}
	if err := json.Unmarshal([]byte(c.body), &req); err != nil {
		t.Fatalf("analyze body %q: %v", c.body, err)
	}
	if req.Normalizer != "folded_keyword" || !reflect.DeepEqual(req.Text, text) {
		t.Errorf("analyze body = %s, want normalizer folded_keyword and text %q", c.body, text)
	}
}

// TestStubSearch_AnswersClientBackgroundPolls guards the stub against the
// opensearch-go background pollers: the client polls node stats (and, on
// multi-node pools, cluster health) on a timer of at least 5 s, so a stub
// that fails unknown paths would break any API process that lives longer.
// The test hits the stub directly to stay fast. It was also checked once, on
// a scratch copy, by sleeping 6 s before filterSubtests: the suite passed.
func TestStubSearch_AnswersClientBackgroundPolls(t *testing.T) {
	stub := newStubSearch(t)
	for _, path := range []string{
		"/_nodes/_local/stats/jvm,breaker,thread_pool",
		"/_cluster/health?local=true",
	} {
		resp, err := http.Get(stub.srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}
