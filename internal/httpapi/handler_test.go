package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// fakeSearcher records the query and the context deadline of its last call.
type fakeSearcher struct {
	result   catalog.Result
	err      error
	calls    int
	got      catalog.Query
	deadline time.Time
	hasDL    bool
	ctxErr   error // ctx.Err() observed at call time
}

func (f *fakeSearcher) Search(ctx context.Context, q catalog.Query) (catalog.Result, error) {
	f.calls++
	f.got = q
	f.deadline, f.hasDL = ctx.Deadline()
	f.ctxErr = ctx.Err()
	return f.result, f.err
}

func newTestHandler(s catalog.Searcher) (http.Handler, *bytes.Buffer) {
	var logs bytes.Buffer
	return New(s, slog.New(slog.NewTextHandler(&logs, nil))), &logs
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func assertJSONHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	// Result() snapshots the headers at WriteHeader time, as a real server does.
	hdr := rec.Result().Header
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestSearch_OKExactBody(t *testing.T) {
	s := &fakeSearcher{result: catalog.Result{Total: 42, Products: []catalog.Product{
		{ID: "123", Name: "Tea", Brands: []string{"Acme", "B"}, Categories: []string{"drinks"}, Nutriscore: "b"},
		{ID: "456", Name: "Milk", Brands: []string{}, Categories: []string{"dairy", "milk"}, Nutriscore: ""},
	}}}
	h, _ := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search")

	want := `{"total":42,"page":1,"size":20,"items":[` +
		`{"id":"123","name":"Tea","brands":["Acme","B"],"categories":["drinks"],"nutriscore":"b"},` +
		`{"id":"456","name":"Milk","brands":[],"categories":["dairy","milk"],"nutriscore":""}],` +
		`"facets":{"brands":[],"categories":[],"nutriscore":[]}}` + "\n"
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	assertJSONHeaders(t, rec)
	if !reflect.DeepEqual(s.got, catalog.Query{Text: "", Page: 1, Size: 20}) {
		t.Errorf("searcher got %+v, want {\"\",1,20}", s.got)
	}
}

func TestSearch_EchoesPageAndSizeAndTrimmedQuery(t *testing.T) {
	s := &fakeSearcher{result: catalog.Result{Total: 7}}
	h, _ := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search?q=+tea+&page=3&size=5")

	want := `{"total":7,"page":3,"size":5,"items":[],"facets":{"brands":[],"categories":[],"nutriscore":[]}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	if !reflect.DeepEqual(s.got, catalog.Query{Text: "tea", Page: 3, Size: 5}) {
		t.Errorf("searcher got %+v, want {tea,3,5}", s.got)
	}
}

func TestSearch_NeverNull(t *testing.T) {
	s := &fakeSearcher{result: catalog.Result{Total: 1, Products: []catalog.Product{{ID: "1", Name: "x"}}}}
	h, _ := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search")
	want := `{"total":1,"page":1,"size":20,"items":[{"id":"1","name":"x","brands":[],"categories":[],"nutriscore":""}],"facets":{"brands":[],"categories":[],"nutriscore":[]}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("nil slices: body = %q, want %q", rec.Body.String(), want)
	}

	s.result = catalog.Result{} // nil Products
	rec = do(h, http.MethodGet, "/search")
	want = `{"total":0,"page":1,"size":20,"items":[],"facets":{"brands":[],"categories":[],"nutriscore":[]}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("nil products: body = %q, want %q", rec.Body.String(), want)
	}
}

func TestSearch_NoHTMLEscaping(t *testing.T) {
	s := &fakeSearcher{result: catalog.Result{Total: 1, Products: []catalog.Product{{ID: "1", Name: "<b>&"}}}}
	h, _ := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search")

	if !strings.Contains(rec.Body.String(), `"name":"<b>&"`) {
		t.Errorf("body = %q, want unescaped <b>&", rec.Body.String())
	}
}

func TestSearch_BadParams(t *testing.T) {
	tests := []struct {
		name, target, body string
	}{
		{"invalid parameter", "/search?size=0",
			`{"error":{"code":"invalid_parameter","message":"size must be an integer between 1 and 100"}}` + "\n"},
		{"page out of range", "/search?page=501",
			`{"error":{"code":"page_out_of_range","message":"page must be at most 500 when size is 20"}}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeSearcher{}
			h, _ := newTestHandler(s)

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
		})
	}
}

func TestSearch_SearcherErrorIs503AndLeaksNothing(t *testing.T) {
	s := &fakeSearcher{err: errors.New("boom secret-detail\x1b[31m")}
	h, logs := newTestHandler(s)

	rec := do(h, http.MethodGet, "/search?q=privateword")

	want := `{"error":{"code":"search_unavailable","message":"search is temporarily unavailable"}}` + "\n"
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	assertJSONHeaders(t, rec)
	if strings.Contains(rec.Body.String(), "secret-detail") {
		t.Error("body leaks the searcher error")
	}
	log := logs.String()
	if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, `msg="search failed"`) || !strings.Contains(log, "error=\"boom secret-detail") {
		t.Errorf("log = %q, want ERROR 'search failed' with the error", log)
	}
	if strings.Contains(log, "privateword") {
		t.Errorf("log contains q: %q", log)
	}
}

func TestSearch_NoLogOnSuccessOrBadParams(t *testing.T) {
	h, logs := newTestHandler(&fakeSearcher{})
	do(h, http.MethodGet, "/search")
	do(h, http.MethodGet, "/search?size=0")
	if logs.Len() != 0 {
		t.Errorf("log = %q, want empty", logs.String())
	}
}

func TestSearch_DeadlinePropagated(t *testing.T) {
	s := &fakeSearcher{}
	h, _ := newTestHandler(s)

	before := time.Now()
	do(h, http.MethodGet, "/search")

	if !s.hasDL {
		t.Fatal("searcher context has no deadline")
	}
	if !s.deadline.After(before.Add(2900*time.Millisecond)) || s.deadline.After(time.Now().Add(searchTimeout)) {
		t.Errorf("deadline = %v, want within (now+2.9s, now+3s]", time.Until(s.deadline))
	}
	if searchTimeout != 3*time.Second {
		t.Errorf("searchTimeout = %v, want 3s", searchTimeout)
	}
}

func TestSearch_EncodeFailureIs500Literal(t *testing.T) {
	orig := encodeJSON
	encodeJSON = func(any) ([]byte, error) { return nil, errors.New("encode boom") }
	t.Cleanup(func() { encodeJSON = orig })
	h, logs := newTestHandler(&fakeSearcher{})

	rec := do(h, http.MethodGet, "/search")

	want := `{"error":{"code":"internal_error","message":"internal error"}}` + "\n"
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	assertJSONHeaders(t, rec)
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "encode boom") {
		t.Errorf("log = %q, want ERROR with the encode error", logs.String())
	}
}

func TestSearch_MuxErrors(t *testing.T) {
	h, _ := newTestHandler(&fakeSearcher{})

	rec := do(h, http.MethodGet, "/nope")
	if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("GET /nope = %d %q, want plain 404", rec.Code, rec.Header().Get("Content-Type"))
	}

	rec = do(h, http.MethodPost, "/search")
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Header().Get("Allow"), "GET") {
		t.Errorf("POST /search = %d Allow=%q, want 405 with GET", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestSearch_HeadHasNoBody(t *testing.T) {
	h, _ := newTestHandler(&fakeSearcher{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Head(srv.URL + "/search")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Errorf("HEAD /search = %d with %d body bytes, want 200 and none", resp.StatusCode, len(body))
	}
}

func TestSearch_ParentCancellationReachesSearcher(t *testing.T) {
	var ctxErr error
	s := searcherFunc(func(ctx context.Context, _ catalog.Query) (catalog.Result, error) {
		ctxErr = ctx.Err()
		return catalog.Result{}, nil
	})
	h, _ := newTestHandler(s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/search", nil).WithContext(ctx)

	h.ServeHTTP(httptest.NewRecorder(), req)

	if !errors.Is(ctxErr, context.Canceled) {
		t.Errorf("searcher ctx.Err() = %v, want context.Canceled", ctxErr)
	}
}

func TestSearch_SearchContextReleasedOnReturn(t *testing.T) {
	var got context.Context
	s := searcherFunc(func(ctx context.Context, _ catalog.Query) (catalog.Result, error) {
		got = ctx
		if ctx.Err() != nil {
			t.Errorf("searcher ctx already done at call: %v", ctx.Err())
		}
		return catalog.Result{}, nil
	})
	h, _ := newTestHandler(s)

	do(h, http.MethodGet, "/search")

	if got == nil || !errors.Is(got.Err(), context.Canceled) {
		t.Errorf("searcher ctx after return = %v, want context.Canceled", got)
	}
}

func TestSearch_SearcherCalledWithLiveContext(t *testing.T) {
	f := &fakeSearcher{}
	h, _ := newTestHandler(f)

	do(h, http.MethodGet, "/search")

	if f.calls != 1 || f.ctxErr != nil {
		t.Errorf("searcher calls = %d, ctx already done at call: %v; want 1 and nil", f.calls, f.ctxErr)
	}
}

func TestSearch_HeadRunsSearchOnce(t *testing.T) {
	f := &fakeSearcher{}
	h, _ := newTestHandler(f)

	rec := do(h, http.MethodHead, "/search")

	// The recorder does not strip HEAD bodies (the server does); body absence is covered by TestSearch_HeadHasNoBody.
	if f.calls != 1 || rec.Code != http.StatusOK {
		t.Errorf("HEAD /search: searcher calls = %d, status = %d, want 1 and 200", f.calls, rec.Code)
	}
}

type searcherFunc func(context.Context, catalog.Query) (catalog.Result, error)

func (f searcherFunc) Search(ctx context.Context, q catalog.Query) (catalog.Result, error) {
	return f(ctx, q)
}

func TestEncodeJSON_DefaultReportsErrors(t *testing.T) {
	if _, err := encodeJSON(make(chan int)); err == nil {
		t.Error("encodeJSON(chan) error = nil, want an error")
	}
}

func TestInternalErrorBody_MatchesEnvelope(t *testing.T) {
	e := internalError()
	want, err := encodeJSON(errorBody{Error: errorDetail{Code: e.code, Message: e.message}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(internalErrorBody, want) {
		t.Errorf("internalErrorBody = %q, want %q", internalErrorBody, want)
	}
	if e.status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", e.status)
	}
}
