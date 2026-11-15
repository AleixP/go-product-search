package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// fakeSuggester records the query, the deadline and ctx.Err() of its calls.
type fakeSuggester struct {
	result   []catalog.Suggestion
	err      error
	calls    int
	got      catalog.SuggestQuery
	deadline time.Time
	hasDL    bool
	ctxErr   error           // ctx.Err() observed at call time
	ctx      context.Context // the context of the last call
	onCall   func(ctx context.Context)
}

func (f *fakeSuggester) Suggest(ctx context.Context, q catalog.SuggestQuery) ([]catalog.Suggestion, error) {
	f.calls++
	f.got = q
	f.deadline, f.hasDL = ctx.Deadline()
	f.ctxErr = ctx.Err()
	f.ctx = ctx
	if f.onCall != nil {
		f.onCall(ctx)
	}
	return f.result, f.err
}

// newSuggestHandler logs at Debug level so a stray Debug line shows up in the buffer.
func newSuggestHandler(sg catalog.Suggester) (*handler, *bytes.Buffer) {
	var logs bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	return &handler{suggester: sg, log: slog.New(slog.NewTextHandler(&logs, opts))}, &logs
}

func newSuggestHandlerJSON(sg catalog.Suggester) (*handler, *bytes.Buffer) {
	var logs bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	return &handler{suggester: sg, log: slog.New(slog.NewJSONHandler(&logs, opts))}, &logs
}

// handlerHung flips on the first timeout so every later test fails fast: a blocked
// handler then costs one 5s wait in the whole package, not one per test.
var handlerHung atomic.Bool

// doSuggest runs the handler in a goroutine so a hang fails the test within 5s.
// The recorder is only read after the goroutine has finished.
func doSuggest(t *testing.T, h *handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	return doSuggestReq(t, h, httptest.NewRequest(http.MethodGet, target, nil))
}

// doSuggestReq is doSuggest for a prepared request (for example with a parent context).
func doSuggestReq(t *testing.T, h *handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if handlerHung.Load() {
		t.Fatal("an earlier test found the handler hung; failing fast")
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.suggest(rec, req)
	}()
	select {
	case <-done:
		return rec
	case <-time.After(5 * time.Second):
		handlerHung.Store(true)
		t.Fatal("handler did not return within 5s")
		return nil
	}
}

const emptySuggestBody = `{"suggestions":[]}` + "\n"

func TestSuggest_OKExactBody(t *testing.T) {
	sg := &fakeSuggester{result: []catalog.Suggestion{{Text: "Chocolate Milk"}, {Text: "Choco Rice"}}}
	h, logs := newSuggestHandler(sg)

	rec := doSuggest(t, h, "/suggest?q=choc")

	want := `{"suggestions":[{"text":"Chocolate Milk"},{"text":"Choco Rice"}]}` + "\n"
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	assertJSONHeaders(t, rec)
	if logs.Len() != 0 {
		t.Errorf("log = %q, want no line on success", logs.String())
	}
}

func TestSuggest_NoHTMLEscaping(t *testing.T) {
	sg := &fakeSuggester{result: []catalog.Suggestion{{Text: "Green Tea <b>&"}}}
	h, _ := newSuggestHandler(sg)

	rec := doSuggest(t, h, "/suggest?q=te")

	want := `{"suggestions":[{"text":"Green Tea <b>&"}]}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestSuggest_EmptyTextStaysInTheObject(t *testing.T) {
	h, _ := newSuggestHandler(&fakeSuggester{result: []catalog.Suggestion{{Text: ""}}})

	rec := doSuggest(t, h, "/suggest?q=ab")

	want := `{"suggestions":[{"text":""}]}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestSuggest_NilAndEmptyResultAreEmptyList(t *testing.T) {
	for name, res := range map[string][]catalog.Suggestion{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			h, _ := newSuggestHandler(&fakeSuggester{result: res})

			rec := doSuggest(t, h, "/suggest?q=zz")

			if rec.Code != http.StatusOK || rec.Body.String() != emptySuggestBody {
				t.Errorf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), emptySuggestBody)
			}
			assertJSONHeaders(t, rec)
		})
	}
}

func TestSuggest_QueryReachesSuggester(t *testing.T) {
	tests := []struct {
		target string
		want   catalog.SuggestQuery
	}{
		{"/suggest?q=choc", catalog.SuggestQuery{Text: "choc", Size: 10}},
		{"/suggest?q=choc&size=20", catalog.SuggestQuery{Text: "choc", Size: 20}},
		{"/suggest?q=%20ab%20&size=1", catalog.SuggestQuery{Text: "ab", Size: 1}},
		{"/suggest?q=%C3%A9%C3%A9", catalog.SuggestQuery{Text: "éé", Size: 10}},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			sg := &fakeSuggester{}
			h, _ := newSuggestHandler(sg)

			doSuggest(t, h, tt.target)

			if sg.calls != 1 || sg.got != tt.want {
				t.Errorf("calls = %d, got %+v, want 1 call with %+v", sg.calls, sg.got, tt.want)
			}
		})
	}
}

func TestSuggest_ShortQueryIsEmptyListWithoutEngineCall(t *testing.T) {
	for _, target := range []string{
		"/suggest?q=a", "/suggest?q=", "/suggest", "/suggest?q=%20a%20", "/suggest?q=%C2%85",
		"/suggest?q=%C3%A9", "/suggest?q=a&size=5", "/suggest?size=3",
	} {
		t.Run(target, func(t *testing.T) {
			sg := &fakeSuggester{result: []catalog.Suggestion{{Text: "must not appear"}}}
			h, logs := newSuggestHandler(sg)

			rec := doSuggest(t, h, target)

			if rec.Code != http.StatusOK || rec.Body.String() != emptySuggestBody {
				t.Errorf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), emptySuggestBody)
			}
			assertJSONHeaders(t, rec)
			if sg.calls != 0 {
				t.Errorf("suggester calls = %d, want 0", sg.calls)
			}
			if logs.Len() != 0 {
				t.Errorf("log = %q, want empty", logs.String())
			}
		})
	}
}

func TestSuggest_TwoRunesReachEngine(t *testing.T) {
	sg := &fakeSuggester{}
	h, _ := newSuggestHandler(sg)

	doSuggest(t, h, "/suggest?q=%C3%A9a") // two runes, three bytes

	if sg.calls != 1 {
		t.Errorf("calls = %d, want 1 (two runes reach the engine)", sg.calls)
	}
}

func TestSuggest_ParseErrorsAreExact400WithoutEngineCall(t *testing.T) {
	tests := []struct {
		name, target, code, msg string
	}{
		{"generic unknown parameter", "/suggest?page=1", "invalid_parameter", "unknown parameter; allowed parameters are q and size"},
		{"unknown key is never named", "/suggest?q=ab&secretkey=1", "invalid_parameter", "unknown parameter; allowed parameters are q and size"},
		{"repeated q", "/suggest?q=ab&q=cd", "invalid_parameter", "parameter q must appear at most once"},
		{"repeated size", "/suggest?q=ab&size=1&size=2", "invalid_parameter", "parameter size must appear at most once"},
		{"control character", "/suggest?q=a%00b", "invalid_parameter", "q must not contain control characters"},
		{"invalid utf8", "/suggest?q=%FF", "invalid_parameter", "q must be valid UTF-8"},
		{"too long", "/suggest?q=" + strings.Repeat("a", 101), "invalid_parameter", "q must be at most 100 characters"},
		{"size zero", "/suggest?q=ab&size=0", "invalid_parameter", "size must be an integer between 1 and 20"},
		{"size 21", "/suggest?q=ab&size=21", "invalid_parameter", "size must be an integer between 1 and 20"},
		{"invalid size wins over short q", "/suggest?q=a&size=0", "invalid_parameter", "size must be an integer between 1 and 20"},
		{"invalid size wins over absent q", "/suggest?size=abc", "invalid_parameter", "size must be an integer between 1 and 20"},
		{"unknown wins over short q", "/suggest?q=a&foo=1", "invalid_parameter", "unknown parameter; allowed parameters are q and size"},
		{"malformed query string", "/suggest?q=%zz", "invalid_parameter", "malformed query string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sg := &fakeSuggester{}
			h, logs := newSuggestHandler(sg)

			rec := doSuggest(t, h, tt.target)

			want := `{"error":{"code":"` + tt.code + `","message":"` + tt.msg + `"}}` + "\n"
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if rec.Body.String() != want {
				t.Errorf("body = %q, want %q", rec.Body.String(), want)
			}
			assertJSONHeaders(t, rec)
			if sg.calls != 0 {
				t.Errorf("suggester calls = %d, want 0", sg.calls)
			}
			if logs.Len() != 0 {
				t.Errorf("log = %q, want no line on 400", logs.String())
			}
		})
	}
}

func TestSuggest_EngineErrorIsFixed503AndLogged(t *testing.T) {
	sg := &fakeSuggester{err: errors.New("boom secret-detail")}
	h, logs := newSuggestHandler(sg)

	rec := doSuggest(t, h, "/suggest?q=qmarker")

	if sg.calls != 1 {
		t.Errorf("suggester calls = %d, want exactly 1 (no retry)", sg.calls)
	}

	want := `{"error":{"code":"search_unavailable","message":"search is temporarily unavailable"}}` + "\n"
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
	assertJSONHeaders(t, rec)
	lines := strings.Split(strings.TrimRight(logs.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d (%q), want 1", len(lines), logs.String())
	}
	for _, w := range []string{"level=ERROR", `msg="suggest failed"`, `error="boom secret-detail"`} {
		if !strings.Contains(lines[0], w) {
			t.Errorf("log = %q, want it to contain %q", lines[0], w)
		}
	}
	if strings.Contains(logs.String(), "qmarker") {
		t.Errorf("log = %q, must not contain q", logs.String())
	}
}

func TestSuggest_DeadlineIsSearchTimeout(t *testing.T) {
	sg := &fakeSuggester{}
	h, _ := newSuggestHandler(sg)
	before := time.Now()

	doSuggest(t, h, "/suggest?q=choc")

	if !sg.hasDL {
		t.Fatal("context has no deadline")
	}
	if !sg.deadline.After(before.Add(2900*time.Millisecond)) || sg.deadline.After(time.Now().Add(searchTimeout)) {
		t.Errorf("deadline in %v, want within (now+2.9s, now+3s]", time.Until(sg.deadline))
	}
}

func TestSuggest_ContextLiveAtCallAndReleasedAfterReturn(t *testing.T) {
	sg := &fakeSuggester{}
	h, _ := newSuggestHandler(sg)

	doSuggest(t, h, "/suggest?q=choc")

	if sg.ctxErr != nil {
		t.Errorf("ctx.Err() at call time = %v, want nil", sg.ctxErr)
	}
	select {
	case <-sg.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context not cancelled after the handler returned")
	}
	if !errors.Is(sg.ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() after return = %v, want context.Canceled", sg.ctx.Err())
	}
}

func TestSuggest_ParentCancellationReachesSuggester(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var observed error
	sg := &fakeSuggester{onCall: func(ctx context.Context) {
		cancel()
		select {
		case <-ctx.Done():
			observed = ctx.Err()
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}}
	h, _ := newSuggestHandler(sg)

	// observed is written in the handler goroutine and read after it finished.
	doSuggestReq(t, h, httptest.NewRequest(http.MethodGet, "/suggest?q=choc", nil).WithContext(parent))

	if !errors.Is(observed, context.Canceled) {
		t.Errorf("ctx.Err() seen by the suggester = %v, want context.Canceled", observed)
	}
}

func TestSuggest_EncodeFailureIs500Literal(t *testing.T) {
	orig := encodeJSON
	encodeJSON = func(any) ([]byte, error) { return nil, errors.New("encode boom") }
	t.Cleanup(func() { encodeJSON = orig })
	sg := &fakeSuggester{}
	h, logs := newSuggestHandler(sg)

	rec := doSuggest(t, h, "/suggest?q=choc")

	if sg.calls != 1 {
		t.Errorf("suggester calls = %d, want 1", sg.calls)
	}

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

func TestSuggest_QueryCaseIsNotFoldedByHandler(t *testing.T) {
	sg := &fakeSuggester{}
	h, _ := newSuggestHandler(sg)

	doSuggest(t, h, "/suggest?q=CHOC")

	if sg.calls != 1 || sg.got.Text != "CHOC" {
		t.Errorf("calls = %d, text = %q, want 1 call with %q (analyzers fold case, not the handler)", sg.calls, sg.got.Text, "CHOC")
	}
}

func TestSuggest_LogsNeverCarryQueryEvenAtDebug(t *testing.T) {
	const qm = "zqmarkerz"
	const em = "errmarkerz"
	handlers := map[string]func(catalog.Suggester) (*handler, *bytes.Buffer){
		"text": newSuggestHandler,
		"json": newSuggestHandlerJSON,
	}
	silent := map[string]string{
		"success": "/suggest?q=" + qm,
		"short":   "/suggest?q=%CE%A9", // one rune
		"400":     "/suggest?q=" + qm + "&bogus=1",
		"400 ctl": "/suggest?q=" + qm + "%00",
	}
	for format, mk := range handlers {
		for name, target := range silent {
			t.Run(format+"/"+name, func(t *testing.T) {
				h, logs := mk(&fakeSuggester{err: nil})

				doSuggest(t, h, target)

				if logs.Len() != 0 {
					t.Errorf("log = %q, want no line at all, even at Debug", logs.String())
				}
			})
		}
		t.Run(format+"/503", func(t *testing.T) {
			sg := &fakeSuggester{err: errors.New("boom " + em)}
			h, logs := mk(sg)

			doSuggest(t, h, "/suggest?q="+qm)

			lines := strings.Split(strings.TrimRight(logs.String(), "\n"), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], "suggest failed") || !strings.Contains(lines[0], "ERROR") {
				t.Fatalf("log = %q, want exactly one ERROR \"suggest failed\" line", logs.String())
			}
			if !strings.Contains(lines[0], em) {
				t.Errorf("log = %q, want the error text", lines[0])
			}
			if strings.Contains(logs.String(), qm) {
				t.Errorf("log = %q, must not contain q", logs.String())
			}
		})
	}
}
