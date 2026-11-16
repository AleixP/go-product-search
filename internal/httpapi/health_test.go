package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeHealth records every call of CheckHealth.
type fakeHealth struct {
	err      error
	calls    int
	deadline time.Time
	hasDL    bool
	ctxErr   error           // ctx.Err() observed at call time
	ctx      context.Context // the context of the last call
}

func (f *fakeHealth) CheckHealth(ctx context.Context) error {
	f.calls++
	f.deadline, f.hasDL = ctx.Deadline()
	f.ctxErr = ctx.Err()
	f.ctx = ctx
	return f.err
}

// newHealthHandler logs JSON at Debug level so a stray line shows up in the buffer.
func newHealthHandler(hc *fakeHealth) (*handler, *bytes.Buffer) {
	var logs bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	return &handler{health: hc, log: slog.New(slog.NewJSONHandler(&logs, opts))}, &logs
}

func healthReq(rawQuery string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.URL.RawQuery = rawQuery
	return req
}

// doHealth runs the handler in a goroutine so a hang fails the test within 5s.
func doHealth(t *testing.T, h *handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if handlerHung.Load() {
		t.Fatal("an earlier test found the handler hung; failing fast")
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.healthz(rec, req)
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

func TestHealthz_OKExactBody(t *testing.T) {
	hc := &fakeHealth{}
	h, logs := newHealthHandler(hc)

	rec := doHealth(t, h, healthReq(""))

	assertBody(t, rec, http.StatusOK, `{"status":"ok"}`+"\n")
	if hc.calls != 1 {
		t.Errorf("CheckHealth calls = %d, want 1", hc.calls)
	}
	if logs.Len() != 0 {
		t.Errorf("log = %q, want no line on success", logs.String())
	}
}

func TestHealthz_ErrorIs503AndLoggedWithDetail(t *testing.T) {
	hc := &fakeHealth{err: errors.New("alias missing secret-detail")}
	h, logs := newHealthHandler(hc)

	rec := doHealth(t, h, healthReq(""))

	assertBody(t, rec, http.StatusServiceUnavailable, unavailableBody)
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("body %q leaks detail", rec.Body.String())
	}
	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log %q is not one JSON line: %v", logs.String(), err)
	}
	if line["msg"] != "health check failed" || line["level"] != "ERROR" || line["error"] != "alias missing secret-detail" {
		t.Errorf("log = %v, want ERROR health check failed with the error", line)
	}
}

func TestHealthz_QueryStringIs400WithoutCheck(t *testing.T) {
	for _, q := range []string{"x=1", strings.Repeat("a", 2048)} {
		hc := &fakeHealth{}
		h, logs := newHealthHandler(hc)

		rec := doHealth(t, h, healthReq(q))

		assertBody(t, rec, http.StatusBadRequest, noParametersBody)
		if hc.calls != 0 {
			t.Errorf("query %.10q: CheckHealth calls = %d, want 0", q, hc.calls)
		}
		if logs.Len() != 0 {
			t.Errorf("log = %q, want no line", logs.String())
		}
	}
}

func TestHealthz_BareQuestionMarkIsAccepted(t *testing.T) {
	hc := &fakeHealth{}
	h, _ := newHealthHandler(hc)

	rec := doHealth(t, h, httptest.NewRequest(http.MethodGet, "/healthz?", nil))

	assertBody(t, rec, http.StatusOK, `{"status":"ok"}`+"\n")
	if hc.calls != 1 {
		t.Errorf("CheckHealth calls = %d, want 1", hc.calls)
	}
}

func TestHealthz_DeadlineIsSearchTimeout(t *testing.T) {
	hc := &fakeHealth{}
	h, _ := newHealthHandler(hc)
	before := time.Now()

	doHealth(t, h, healthReq(""))

	if !hc.hasDL {
		t.Fatal("CheckHealth context has no deadline")
	}
	if hc.deadline.Before(before.Add(searchTimeout)) || hc.deadline.After(time.Now().Add(searchTimeout)) {
		t.Errorf("deadline = %v, want within [%v, now+%v]", hc.deadline, before.Add(searchTimeout), searchTimeout)
	}
}

func TestHealthz_ParentCancellationReachesChecker(t *testing.T) {
	hc := &fakeHealth{err: context.Canceled}
	h, _ := newHealthHandler(hc)
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	rec := doHealth(t, h, healthReq("").WithContext(parent))

	if !errors.Is(hc.ctxErr, context.Canceled) {
		t.Errorf("ctx.Err() in checker = %v, want context.Canceled", hc.ctxErr)
	}
	assertBody(t, rec, http.StatusServiceUnavailable, unavailableBody)
}

func TestHealthz_ContextCancelledAfterReturn(t *testing.T) {
	hc := &fakeHealth{}
	h, _ := newHealthHandler(hc)

	doHealth(t, h, healthReq(""))

	if hc.ctx == nil {
		t.Fatal("CheckHealth was not called")
	}
	if hc.ctxErr != nil {
		t.Errorf("ctx.Err() during the call = %v, want nil", hc.ctxErr)
	}
	if !errors.Is(hc.ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() after return = %v, want context.Canceled", hc.ctx.Err())
	}
}

func TestHealthz_EncodeFailureIs500Literal(t *testing.T) {
	orig := encodeJSON
	encodeJSON = func(any) ([]byte, error) { return nil, errors.New("encode boom") }
	t.Cleanup(func() { encodeJSON = orig })
	h, _ := newHealthHandler(&fakeHealth{})

	rec := doHealth(t, h, healthReq(""))

	assertBody(t, rec, http.StatusInternalServerError, string(internalErrorBody))
}
