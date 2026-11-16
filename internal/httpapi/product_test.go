package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// fakeGetter records every call of Get.
type fakeGetter struct {
	product  catalog.Product
	err      error
	ids      []string
	deadline time.Time
	hasDL    bool
	ctxErr   error           // ctx.Err() observed at call time
	ctx      context.Context // the context of the last call
	onCall   func(ctx context.Context)
}

func (f *fakeGetter) Get(ctx context.Context, id string) (catalog.Product, error) {
	f.ids = append(f.ids, id)
	f.deadline, f.hasDL = ctx.Deadline()
	f.ctxErr = ctx.Err()
	f.ctx = ctx
	if f.onCall != nil {
		f.onCall(ctx)
	}
	return f.product, f.err
}

// newProductHandler logs JSON at Debug level so a stray line shows up in the buffer.
func newProductHandler(g catalog.Getter) (*handler, *bytes.Buffer) {
	var logs bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	return &handler{getter: g, log: slog.New(slog.NewJSONHandler(&logs, opts))}, &logs
}

func productReq(id, rawQuery string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/products/x", nil)
	req.URL.RawQuery = rawQuery
	req.SetPathValue("id", id)
	return req
}

// doProduct runs the handler in a goroutine so a hang fails the test within 5s.
func doProduct(t *testing.T, h *handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if handlerHung.Load() {
		t.Fatal("an earlier test found the handler hung; failing fast")
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.product(rec, req)
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

const (
	notFoundBody     = `{"error":{"code":"not_found","message":"product not found"}}` + "\n"
	noParametersBody = `{"error":{"code":"invalid_parameter","message":"unknown parameter; this endpoint accepts no parameters"}}` + "\n"
)

func assertBody(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
	assertJSONHeaders(t, rec)
}

func TestProduct_OKExactBody(t *testing.T) {
	g := &fakeGetter{product: catalog.Product{ID: "123", Name: "Tea <b>&", Categories: []string{"drinks"}, Nutriscore: "b"}}
	h, logs := newProductHandler(g)

	rec := doProduct(t, h, productReq("123", ""))

	assertBody(t, rec, http.StatusOK, `{"id":"123","name":"Tea <b>&","brands":[],"categories":["drinks"],"nutriscore":"b"}`+"\n")
	if len(g.ids) != 1 || g.ids[0] != "123" {
		t.Errorf("getter ids = %q, want [123]", g.ids)
	}
	if logs.Len() != 0 {
		t.Errorf("log = %q, want no line on success", logs.String())
	}
}

func TestProduct_NilListsAreEmptyArrays(t *testing.T) {
	g := &fakeGetter{product: catalog.Product{ID: "1", Name: "N"}}
	h, _ := newProductHandler(g)

	rec := doProduct(t, h, productReq("1", ""))

	assertBody(t, rec, http.StatusOK, `{"id":"1","name":"N","brands":[],"categories":[],"nutriscore":""}`+"\n")
}

func TestProduct_ErrNotFoundIs404WithoutLog(t *testing.T) {
	g := &fakeGetter{err: fmt.Errorf("wrap: %w", catalog.ErrNotFound)}
	h, logs := newProductHandler(g)

	rec := doProduct(t, h, productReq("s3cr3t-id", ""))

	assertBody(t, rec, http.StatusNotFound, notFoundBody)
	if len(g.ids) != 1 {
		t.Errorf("getter calls = %d, want 1", len(g.ids))
	}
	if logs.Len() != 0 {
		t.Errorf("log = %q, want no line for a missing product", logs.String())
	}
}

func TestProduct_InvalidIDsAre404WithoutGetterCall(t *testing.T) {
	tests := []struct{ name, id string }{
		{"empty", ""},
		{"129 ascii bytes", strings.Repeat("9", 129)},
		{"129 bytes of euro signs", strings.Repeat("€", 43)},
		{"invalid utf-8", "\xff"},
		{"nul (C0)", "a\x00b"},
		{"newline (C0)", "a\nb"},
		{"unit separator (C0 end)", "a\x1fb"},
		{"del", "\x7f"},
		{"first C1", "\u0080"},
		{"nel (C1)", "\u0085"},
		{"last C1", "\u009f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGetter{}
			h, logs := newProductHandler(g)

			rec := doProduct(t, h, productReq(tt.id, ""))

			assertBody(t, rec, http.StatusNotFound, notFoundBody)
			if len(g.ids) != 0 {
				t.Errorf("getter calls = %d (ids %q), want 0", len(g.ids), g.ids)
			}
			if logs.Len() != 0 {
				t.Errorf("log = %q, want no line", logs.String())
			}
		})
	}
}

func TestProduct_ValidIDsReachTheGetter(t *testing.T) {
	tests := []struct{ name, id string }{
		{"128 ascii bytes", strings.Repeat("9", 128)},
		{"42 euro signs (126 bytes)", strings.Repeat("€", 42)},
		{"slash", "a/b"},
		{"leading space", " 1"},
		{"first printable after C0", " "},
		{"first rune after C1", " "},
		{"one byte", "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGetter{product: catalog.Product{ID: "r"}}
			h, _ := newProductHandler(g)

			rec := doProduct(t, h, productReq(tt.id, ""))

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if len(g.ids) != 1 || g.ids[0] != tt.id {
				t.Errorf("getter ids = %q, want [%q]", g.ids, tt.id)
			}
		})
	}
}

func TestProduct_QueryStringIs400WithoutGetterCall(t *testing.T) {
	tests := []struct{ name, id, query string }{
		{"unknown parameter", "1", "x=1"},
		{"id parameter", "1", "id=1"},
		{"2 KiB query", "1", strings.Repeat("a", 2048)},
		{"invalid id checked after the query", "\xff", "x=1"},
		{"empty id checked after the query", "", "x=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGetter{}
			h, logs := newProductHandler(g)

			rec := doProduct(t, h, productReq(tt.id, tt.query))

			assertBody(t, rec, http.StatusBadRequest, noParametersBody)
			if len(g.ids) != 0 {
				t.Errorf("getter calls = %d, want 0", len(g.ids))
			}
			if logs.Len() != 0 {
				t.Errorf("log = %q, want no line", logs.String())
			}
		})
	}
}

func TestProduct_BareQuestionMarkIsAccepted(t *testing.T) {
	g := &fakeGetter{product: catalog.Product{ID: "1"}}
	h, _ := newProductHandler(g)
	req := httptest.NewRequest(http.MethodGet, "/products/1?", nil)
	req.SetPathValue("id", "1")

	rec := doProduct(t, h, req)

	if rec.Code != http.StatusOK || len(g.ids) != 1 {
		t.Errorf("status = %d, calls = %d, want 200 and 1", rec.Code, len(g.ids))
	}
}

func TestProduct_GetterErrorIs503AndLoggedWithoutID(t *testing.T) {
	g := &fakeGetter{err: errors.New("boom secret-detail")}
	h, logs := newProductHandler(g)

	rec := doProduct(t, h, productReq("s3cr3t-id", ""))

	assertBody(t, rec, http.StatusServiceUnavailable, unavailableBody)
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("body %q leaks detail", rec.Body.String())
	}
	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log %q is not one JSON line: %v", logs.String(), err)
	}
	if line["msg"] != "get product failed" || line["level"] != "ERROR" || line["error"] != "boom secret-detail" {
		t.Errorf("log = %v, want ERROR get product failed with the error", line)
	}
	if strings.Contains(logs.String(), "s3cr3t-id") {
		t.Errorf("log %q contains the id", logs.String())
	}
}

func TestProduct_DeadlineIsSearchTimeout(t *testing.T) {
	g := &fakeGetter{}
	h, _ := newProductHandler(g)
	before := time.Now()

	doProduct(t, h, productReq("1", ""))

	if !g.hasDL {
		t.Fatal("getter context has no deadline")
	}
	if g.deadline.Before(before.Add(searchTimeout)) || g.deadline.After(time.Now().Add(searchTimeout)) {
		t.Errorf("deadline = %v, want within [%v, now+%v]", g.deadline, before.Add(searchTimeout), searchTimeout)
	}
}

func TestProduct_ParentCancellationReachesGetter(t *testing.T) {
	g := &fakeGetter{err: context.Canceled}
	h, _ := newProductHandler(g)
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	rec := doProduct(t, h, productReq("1", "").WithContext(parent))

	if !errors.Is(g.ctxErr, context.Canceled) {
		t.Errorf("ctx.Err() in getter = %v, want context.Canceled", g.ctxErr)
	}
	assertBody(t, rec, http.StatusServiceUnavailable, unavailableBody)
}

func TestProduct_ContextCancelledAfterReturn(t *testing.T) {
	g := &fakeGetter{}
	h, _ := newProductHandler(g)

	doProduct(t, h, productReq("1", ""))

	if g.ctxErr != nil {
		t.Errorf("ctx.Err() during the call = %v, want nil", g.ctxErr)
	}
	if !errors.Is(g.ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() after return = %v, want context.Canceled", g.ctx.Err())
	}
}

func TestProduct_EncodeFailureIs500Literal(t *testing.T) {
	orig := encodeJSON
	encodeJSON = func(any) ([]byte, error) { return nil, errors.New("encode boom") }
	t.Cleanup(func() { encodeJSON = orig })
	h, _ := newProductHandler(&fakeGetter{})

	rec := doProduct(t, h, productReq("1", ""))

	assertBody(t, rec, http.StatusInternalServerError, string(internalErrorBody))
}

func TestProduct_MaxIDBytes(t *testing.T) {
	if maxProductIDBytes != 128 {
		t.Errorf("maxProductIDBytes = %d, want 128", maxProductIDBytes)
	}
}

func TestItemOf_NilListsBecomeEmpty(t *testing.T) {
	got := itemOf(catalog.Product{ID: "1", Name: "n", Nutriscore: "a"})

	if got.Brands == nil || len(got.Brands) != 0 || got.Categories == nil || len(got.Categories) != 0 {
		t.Errorf("itemOf(nil lists) = %+v, want non-nil empty slices", got)
	}
	if got.ID != "1" || got.Name != "n" || got.Nutriscore != "a" {
		t.Errorf("itemOf = %+v, want fields copied", got)
	}
}
