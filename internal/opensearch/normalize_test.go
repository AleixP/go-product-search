package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// engineCall is one request the fake engine saw.
type engineCall struct {
	kind   string // "analyze" or "search"
	method string
	path   string
	body   string
}

// engine is a fake OpenSearch that answers the ping, GET/POST
// /products/_analyze and POST /products/_search, and records the calls in order.
type engine struct {
	// analyze and search, when set, replace the default answers. n is the
	// 1-based number of the call among the calls of its kind.
	analyze func(w http.ResponseWriter, r *http.Request, texts []string, n int)
	search  func(w http.ResponseWriter, r *http.Request, body []byte, n int)

	mu    sync.Mutex
	calls []engineCall
}

func newEngine(t *testing.T) (*engine, *Client, *httptest.Server) {
	t.Helper()
	e := &engine{}
	c, srv := newSearchClient(t, e.serve)
	return e, c, srv
}

// fold is the fake's normalizer: lowercase plus the two accents the tests use.
func fold(s string) string {
	return strings.NewReplacer("é", "e", "É", "e").Replace(strings.ToLower(s))
}

func (e *engine) record(kind string, r *http.Request, body []byte) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, engineCall{kind: kind, method: r.Method, path: r.URL.Path, body: string(body)})
	n := 0
	for _, c := range e.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

func (e *engine) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/products/_analyze":
		n := e.record("analyze", r, body)
		var req struct {
			Text []string `json:"text"`
		}
		_ = json.Unmarshal(body, &req)
		if e.analyze != nil {
			e.analyze(w, r, req.Text, n)
			return
		}
		writeJSON(w, tokensBody(foldAll(req.Text)...))
	case "/products/_search":
		n := e.record("search", r, body)
		if e.search != nil {
			e.search(w, r, body, n)
			return
		}
		writeJSON(w, searchBodyWith(0, 0, false, "", aggsFor(body)))
	default:
		http.NotFound(w, r)
	}
}

func foldAll(texts []string) []string {
	out := make([]string, len(texts))
	for i, s := range texts {
		out[i] = fold(s)
	}
	return out
}

// of returns the recorded calls of one kind.
func (e *engine) of(kind string) []engineCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []engineCall
	for _, c := range e.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (e *engine) kinds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.calls))
	for i, c := range e.calls {
		out[i] = c.kind
	}
	return out
}

func tokensBody(tokens ...string) string {
	parts := make([]string, len(tokens))
	for i, tk := range tokens {
		b, _ := json.Marshal(tk)
		parts[i] = fmt.Sprintf(`{"token":%s,"start_offset":0,"end_offset":1,"type":"word","position":%d}`, b, i)
	}
	return `{"tokens":[` + strings.Join(parts, ",") + `]}`
}

// aggsFor builds a valid aggregations block for a search body: one selected
// bucket (count 1) per filter value and empty top lists.
func aggsFor(body []byte) string {
	var req struct {
		Aggs map[string]struct {
			Aggs map[string]struct {
				Filters struct {
					Filters []json.RawMessage `json:"filters"`
				} `json:"filters"`
			} `json:"aggs"`
		} `json:"aggs"`
	}
	_ = json.Unmarshal(body, &req)
	sel := func(name string) string {
		n := len(req.Aggs[name].Aggs["selected"].Filters.Filters)
		if n == 0 {
			return ""
		}
		return `,"selected":{"buckets":[` + strings.Repeat(`{"doc_count":1},`, n)[:n*16-1] + `]}`
	}
	return `{"brands":{"doc_count":0,"top":{"buckets":[]}` + sel("brands") + `},` +
		`"categories":{"doc_count":0,"top":{"buckets":[]}` + sel("categories") + `},` +
		`"nutriscore":{"doc_count":0,"grades":{"buckets":[{"doc_count":0},{"doc_count":0},{"doc_count":0},{"doc_count":0},{"doc_count":0}]}}}`
}

func TestNormalizerName_IsUsedByTheMapping(t *testing.T) {
	m := loadMapping(t)
	if _, ok := m.Settings.Anlys.Normalizer[normalizerName]; !ok {
		t.Errorf("settings do not define the normalizer %q", normalizerName)
	}
	for _, field := range []string{"brands", "categories"} {
		norm := sub(t, m.Mappings.Properties[field], "fields", "norm")
		if norm["normalizer"] != normalizerName {
			t.Errorf("%s.norm normalizer = %v, want %q", field, norm["normalizer"], normalizerName)
		}
	}
}

func TestNormalize_SendsNormalizerAndTextInOrder(t *testing.T) {
	e, c, _ := newEngine(t)
	if _, _, err := c.normalize(context.Background(), []string{"ZED", "Acme"}, []string{"NESTLÉ"}); err != nil {
		t.Fatalf("normalize error = %v", err)
	}
	calls := e.of("analyze")
	if len(calls) != 1 {
		t.Fatalf("analyze calls = %d, want 1", len(calls))
	}
	if calls[0].method != http.MethodGet || calls[0].path != "/products/_analyze" {
		t.Errorf("request = %s %s, want GET /products/_analyze", calls[0].method, calls[0].path)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(calls[0].body), &got); err != nil {
		t.Fatalf("body %q: %v", calls[0].body, err)
	}
	want := map[string]any{"normalizer": "folded_keyword", "text": []any{"ZED", "Acme", "NESTLÉ"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body = %v, want %v", got, want)
	}
}

func TestNormalize_NothingToNormalizeMakesNoCall(t *testing.T) {
	e, c, _ := newEngine(t)
	b, cat, err := c.normalize(context.Background(), nil, nil)
	if b != nil || cat != nil || err != nil {
		t.Errorf("normalize = %v, %v, %v, want nil, nil, nil", b, cat, err)
	}
	if n := len(e.of("analyze")); n != 0 {
		t.Errorf("analyze calls = %d, want 0", n)
	}
}

func TestNormalize_OneFieldOnlyLeavesTheOtherNil(t *testing.T) {
	_, c, _ := newEngine(t)
	b, cat, err := c.normalize(context.Background(), nil, []string{"Té"})
	if err != nil || b != nil || !slices.Equal(cat, []string{"te"}) {
		t.Errorf("normalize = %v, %v, %v, want nil, [te], nil", b, cat, err)
	}
	b, cat, err = c.normalize(context.Background(), []string{"Té"}, nil)
	if err != nil || cat != nil || !slices.Equal(b, []string{"te"}) {
		t.Errorf("normalize = %v, %v, %v, want [te], nil, nil", b, cat, err)
	}
}

func TestNormalize_DedupesAndSortsBytewisePerField(t *testing.T) {
	e, c, _ := newEngine(t)
	// Fixed tokens: the fake does not fold here. Brands get the first four.
	e.analyze = func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) {
		writeJSON(w, tokensBody("b", "B", "a", "b", "b", "a"))
	}
	b, cat, err := c.normalize(context.Background(), []string{"1", "2", "3", "4"}, []string{"5", "6"})
	if err != nil {
		t.Fatalf("normalize error = %v", err)
	}
	if want := []string{"B", "a", "b"}; !slices.Equal(b, want) {
		t.Errorf("brands = %q, want %q", b, want)
	}
	if want := []string{"a", "b"}; !slices.Equal(cat, want) {
		t.Errorf("categories = %q, want %q", cat, want)
	}
}

func TestNormalize_FoldedSpellingsCollapse(t *testing.T) {
	_, c, _ := newEngine(t)
	got, _, err := c.normalize(context.Background(), []string{"NESTLÉ", "nestle"}, nil)
	if err != nil || !slices.Equal(got, []string{"nestle"}) {
		t.Errorf("normalize = %q, %v, want [nestle]", got, err)
	}
}

func TestNormalize_TokenCountMismatchIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens []string
		want   string
	}{
		{"fewer", []string{"a"}, "search products: normalize filters: got 1 tokens for 2 values"},
		{"none", nil, "search products: normalize filters: got 0 tokens for 2 values"},
		{"more", []string{"a", "b", "c"}, "search products: normalize filters: got 3 tokens for 2 values"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, c, _ := newEngine(t)
			e.analyze = func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) {
				writeJSON(w, tokensBody(tc.tokens...))
			}
			_, _, err := c.normalize(context.Background(), []string{"SECRETA", "SECRETB"}, nil)
			if err == nil || err.Error() != tc.want {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNormalize_ErrorsNameTheCallAndNeverCarryValues(t *testing.T) {
	const secret = "SECRETVALUE"
	structBody := func(reason string) string {
		return `{"error":{"root_cause":[{"type":"illegal_argument_exception","reason":"` + reason + `"}],` +
			`"type":"illegal_argument_exception","reason":"` + reason + `",` +
			`"caused_by":{"type":"x","reason":"` + reason + `"}},"status":400}`
	}
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"struct error quoting the value", 400, structBody("bad text [" + secret + "]"),
			"search products: normalize filters: status 400, type illegal_argument_exception"},
		{"html error body", 500, "<html>" + secret + "</html>",
			"search products: normalize filters: undecodable response (status 500)"},
		{"string error", 400, `{"status":400,"error":"` + secret + `"}`,
			"search products: normalize filters: undecodable response (status 400)"},
		{"unknown error shape", 400, `{"status":400,"error":{"foo":"` + secret + `"}}`,
			"search products: normalize filters: undecodable response (status 400)"},
		{"reason error", 400, `{"status":"bad","reason":"` + secret + `"}`,
			"search products: normalize filters: undecodable response (status 400)"},
		{"malformed success", 200, `{"tokens": ` + secret,
			"search products: normalize filters: undecodable response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, c, _ := newEngine(t)
			e.analyze = func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}
			_, _, err := c.normalize(context.Background(), nil, []string{secret})
			if err == nil {
				t.Fatal("normalize error = nil, want error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks a value: %v", err)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestNormalize_TransportErrorCarriesNeitherURLNorValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c, err := New(context.Background(), srv.URL+"/p4th")
	if err != nil {
		t.Fatal(err)
	}
	host := mustHost(t, srv.URL)
	srv.Close()
	_, _, err = c.normalize(context.Background(), []string{"SECRETVALUE"}, nil)
	if err == nil {
		t.Fatal("normalize error = nil, want error")
	}
	if !strings.HasPrefix(err.Error(), "search products: normalize filters: ") {
		t.Errorf("error = %q, want the prefix %q", err, "search products: normalize filters: ")
	}
	if strings.Contains(err.Error(), "SECRETVALUE") || strings.Contains(err.Error(), "p4th") {
		t.Errorf("error leaks a value or the URL path: %v", err)
	}
	assertNoRequestURL(t, "normalize error", err.Error(), host)
}
