package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	osclient "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/errmask"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/AleixP/go-product-search/internal/catalog"
)

func TestBulk_BodyIsNDJSONWithIndexActions(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(2)...)))
	c := b.client(t)
	docs := []catalog.Product{
		{ID: "100", Name: "Cola", Brands: []string{"Acme"}, Categories: []string{"drinks"}, Nutriscore: "e"},
		{ID: "200", Name: "Water", Brands: []string{}, Categories: []string{}, Nutriscore: ""},
	}

	if _, err := c.Bulk(context.Background(), testIndex, docs); err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	body := b.lastBody()
	if !strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\n\n") {
		t.Errorf("body must end with exactly one newline: %q", body)
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("body has %d lines, want 4 (2 per document): %q", len(lines), body)
	}
	if b.methods[0] != http.MethodPost || b.paths[0] != "/"+testIndex+"/_bulk" {
		t.Errorf("request = %s %s, want POST /%s/_bulk", b.methods[0], b.paths[0], testIndex)
	}
	wantActions := []string{`{"index":{"_index":"` + testIndex + `","_id":"100"}}`, `{"index":{"_index":"` + testIndex + `","_id":"200"}}`}
	wantDocs := []string{`{"name":"Cola","brands":["Acme"],"categories":["drinks"],"nutriscore":"e"}`, `{"name":"Water","brands":[],"categories":[],"nutriscore":""}`}
	for i := range docs {
		if lines[2*i] != wantActions[i] {
			t.Errorf("action line %d = %s, want %s", i, lines[2*i], wantActions[i])
		}
		if lines[2*i+1] != wantDocs[i] {
			t.Errorf("document line %d = %s, want %s", i, lines[2*i+1], wantDocs[i])
		}
	}
}

func TestBulk_HostileIDsAndNamesKeepTwoLinesPerDocument(t *testing.T) {
	ids := []string{`a"b`, "a\nb", `a\b`, "café-中文", "a<b>&c", "tab\there", "line sep", "}{", `","_index":"evil`, "bad\xffutf8"}
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(len(ids))...)))
	c := b.client(t)
	docs := make([]catalog.Product, len(ids))
	for i, id := range ids {
		docs[i] = catalog.Product{ID: id, Name: "n\n" + id, Brands: []string{id}, Categories: []string{}}
	}

	if _, err := c.Bulk(context.Background(), testIndex, docs); err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(b.lastBody(), "\n"), "\n")
	if len(lines) != 2*len(ids) {
		t.Fatalf("body has %d lines, want %d: a code or name broke the NDJSON framing", len(lines), 2*len(ids))
	}
	for i, id := range ids {
		var act struct {
			Index map[string]string `json:"index"`
		}
		if err := json.Unmarshal([]byte(lines[2*i]), &act); err != nil {
			t.Fatalf("action line %d is not JSON: %v: %s", i, err, lines[2*i])
		}
		if len(act.Index) != 2 || act.Index["_index"] != testIndex {
			t.Errorf("action %d = %v, want only _index and _id", i, act.Index)
		}
		if want := strings.ToValidUTF8(id, "�"); act.Index["_id"] != want {
			t.Errorf("action %d _id = %q, want %q", i, act.Index["_id"], want)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(lines[2*i+1]), &doc); err != nil {
			t.Fatalf("document line %d is not JSON: %v", i, err)
		}
		if _, has := doc["ID"]; has || len(doc) != 4 {
			t.Errorf("document %d = %v, want exactly the 4 body fields", i, doc)
		}
	}
}

func TestBulk_UsesGivenIndexName(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(1)...)))
	c := b.client(t)

	if _, err := c.Bulk(context.Background(), "products-other", prods(1)); err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if !strings.Contains(b.lastBody(), `"_index":"products-other"`) || b.paths[0] != "/products-other/_bulk" {
		t.Errorf("index name not used: path %s body %s", b.paths[0], b.lastBody())
	}
}

func TestBulk_EmptyBatchIsANoOp(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON()))
	c := b.client(t)

	for _, docs := range [][]catalog.Product{nil, {}} {
		got, err := c.Bulk(context.Background(), testIndex, docs)
		if err != nil {
			t.Fatalf("Bulk(empty) error = %v", err)
		}
		if got.Indexed != 0 || got.Failed != 0 || len(got.FailedIDs) != 0 {
			t.Errorf("Bulk(empty) = %+v, want zero", got)
		}
	}
	if n := b.calls(); n != 0 {
		t.Errorf("requests = %d, want 0 for an empty batch", n)
	}
}

func TestBulk_RefusesUnsafeIndexNames(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(1)...)))
	c := b.client(t)
	names := []string{"", "products", "other", "_all", "*", "products-*", "products-a,products", "products-a?x", "products-a/../products", "products-a b", "Products-a", "xproducts-a"}
	for _, name := range names {
		if _, err := c.Bulk(context.Background(), name, prods(1)); err == nil {
			t.Errorf("Bulk(%q) error = nil, want a refusal", name)
		}
		if _, err := c.Bulk(context.Background(), name, nil); err == nil {
			t.Errorf("Bulk(%q, empty) error = nil, want a refusal even for an empty batch", name)
		}
	}
	if n := b.calls(); n != 0 {
		t.Errorf("requests = %d, want 0: a refused name must not reach the cluster", n)
	}
}

func TestBulk_RefusesEmptyDocumentID(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(2)...)))
	c := b.client(t)

	// An empty _id would make OpenSearch generate one, so a re-run would duplicate the product.
	if _, err := c.Bulk(context.Background(), testIndex, []catalog.Product{prod("ok"), {Name: "no id"}}); err == nil {
		t.Fatal("Bulk() with an empty ID: error = nil, want error")
	}
	if n := b.calls(); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

func TestBulk_WholeRequestErrorsAreReturnedWrapped(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			b := newBulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				writeJSON(w, fmt.Sprintf(`{"error":{"type":"boom","reason":"boom"},"status":%d}`, status))
			})
			c := b.client(t)

			got, err := c.Bulk(context.Background(), testIndex, prods(2))
			if err == nil {
				t.Fatalf("Bulk() = %+v, nil, want an error", got)
			}
			if !strings.Contains(err.Error(), "bulk index "+testIndex) {
				t.Errorf("error = %q, want it to name the operation and index", err)
			}
			if errors.Unwrap(err) == nil {
				t.Errorf("error %v does not wrap the cause", err)
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Errorf("error = %q, want the cluster reason kept", err)
			}
			if got.Indexed != 0 || got.Failed != 0 {
				t.Errorf("result = %+v, want zero when the request failed as a whole", got)
			}
		})
	}
}

func TestBulk_MalformedResponseBodyIsAnError(t *testing.T) {
	b := newBulkServer(t, respondWith(`{"items": "not a list"`))
	c := b.client(t)

	if got, err := c.Bulk(context.Background(), testIndex, prods(1)); err == nil {
		t.Fatalf("Bulk() = %+v, nil, want an error for an unparsable response", got)
	}
}

func TestBulk_ClosedServerReturnsErrorWithoutPanic(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON()))
	c := b.client(t)
	b.srv.Close()

	for name, cl := range map[string]*Client{
		"masked":   c,
		"unmasked": newClientWith(t, osclient.Config{Addresses: []string{b.srv.URL}}, errmask.New()),
	} {
		got, err := cl.Bulk(context.Background(), testIndex, prods(1))
		if err == nil {
			t.Errorf("%s: Bulk() on a closed server = %+v, nil, want error", name, got)
		}
		if got.Indexed != 0 || got.Failed != 0 {
			t.Errorf("%s: result = %+v, want zero", name, got)
		}
	}
}

func TestBulk_PartialBulkErrorWithPopulatedResponseIsAccounted(t *testing.T) {
	specs := []itemSpec{{status: 201}, {status: 400, errType: "mapper_parsing_exception"}, {status: 429, errType: "es_rejected_execution_exception"}}
	b := newBulkServer(t, respondWith(bulkJSON(specs...)))
	// errmask.New() reports every category: the API client returns a
	// *PartialBulkError together with the populated response.
	c := newClientWith(t, osclient.Config{Addresses: []string{b.srv.URL}}, errmask.New())

	api, apiErr := c.api.Bulk(context.Background(), opensearchapi.BulkReq{Index: testIndex, Body: strings.NewReader(`{"index":{"_id":"a"}}` + "\n{}\n")})
	var pbe *opensearchapi.PartialBulkError
	if !errors.As(apiErr, &pbe) || api == nil || len(api.Items) != 3 {
		t.Fatalf("precondition: want a PartialBulkError with 3 items, got %v, %+v", apiErr, api)
	}

	got, err := c.Bulk(context.Background(), testIndex, prods(3))
	if err != nil {
		t.Fatalf("Bulk() error = %v, want nil: item failures belong in BulkResult", err)
	}
	if got.Indexed != 1 || got.Failed != 2 || got.FailureTypes["mapper_parsing_exception"] != 1 || got.FailureTypes["es_rejected_execution_exception"] != 1 {
		t.Errorf("Bulk() = %+v, want 1 indexed and 2 failed by type", got)
	}
	if !slices.Equal(got.FailedIDs, []string{"id1", "id2"}) {
		t.Errorf("FailedIDs = %v, want [id1 id2]", got.FailedIDs)
	}
}

func TestBulk_PartialErrorMaskedAndUnmaskedGiveTheSameResult(t *testing.T) {
	specs := []itemSpec{{status: 400, errType: "x_exception"}, {status: 201}}
	b := newBulkServer(t, respondWith(bulkJSON(specs...)))
	masked := b.client(t)
	unmasked := newClientWith(t, osclient.Config{Addresses: []string{b.srv.URL}}, errmask.New())

	a, errA := masked.Bulk(context.Background(), testIndex, prods(2))
	u, errU := unmasked.Bulk(context.Background(), testIndex, prods(2))
	if errA != nil || errU != nil {
		t.Fatalf("errors = %v, %v, want nil, nil", errA, errU)
	}
	if a.Indexed != u.Indexed || a.Failed != u.Failed || len(a.FailureTypes) != len(u.FailureTypes) {
		t.Errorf("masked %+v != unmasked %+v", a, u)
	}
}

// deadlineRecorder remembers the context deadline of each request.
type deadlineRecorder struct {
	next     http.RoundTripper
	deadline time.Time
	has      bool
}

func (d *deadlineRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	d.deadline, d.has = r.Context().Deadline()
	return d.next.RoundTrip(r)
}

func TestBulk_CallHasAOneMinuteTimeout(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(1)...)))
	rec := &deadlineRecorder{next: http.DefaultTransport}
	c := newClientWith(t, osclient.Config{Addresses: []string{b.srv.URL}, Transport: rec}, nil)

	start := time.Now()
	if _, err := c.Bulk(context.Background(), testIndex, prods(1)); err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if !rec.has {
		t.Fatal("request context has no deadline, want a 60 s timeout")
	}
	// The deadline is set after start, so it is at least 60 s later and, barring a
	// very slow scheduler, less than 60.5 s.
	if d := rec.deadline.Sub(start); d < 60*time.Second || d > 60*time.Second+500*time.Millisecond {
		t.Errorf("deadline is %v after the call started, want 60 s", d)
	}
}

func TestBulk_StalledServerTimesOut(t *testing.T) {
	old := bulkTimeout
	bulkTimeout = 150 * time.Millisecond
	t.Cleanup(func() { bulkTimeout = old })
	release := make(chan struct{})
	b := newBulkServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) })
	c := b.client(t)

	start := time.Now()
	_, err := c.Bulk(context.Background(), testIndex, prods(1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Bulk() error = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Bulk() took %v, want it to stop at the timeout", d)
	}
}

func TestBulk_CancelledContextIsFatal(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(1)...)))
	c := b.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := c.Bulk(ctx, testIndex, prods(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Bulk() = %+v, %v, want context.Canceled", got, err)
	}
	if got.Indexed != 0 || got.Failed != 0 {
		t.Errorf("result = %+v, want zero", got)
	}
}

func TestBulk_CancelWhileInFlightReturnsPromptly(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	b := newBulkServer(t, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) })
	c := b.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()

	start := time.Now()
	_, err := c.Bulk(ctx, testIndex, prods(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Bulk() error = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Bulk() took %v after cancel, want it to return at once", d)
	}
}

func TestBulk_ErrorsAndLogsDoNotEchoAddressPathOrDocuments(t *testing.T) {
	b := newBulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, `{"error":{"type":"boom","reason":"boom"},"status":500}`)
	})
	secret := "/tok3n-secret/path"
	c, err := New(context.Background(), b.srv.URL+secret)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var logs strings.Builder
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	doc := catalog.Product{ID: "id-secret-doc", Name: "secret product name", Brands: []string{}, Categories: []string{}}

	_, errHTTP := c.Bulk(context.Background(), testIndex, []catalog.Product{doc})
	b.srv.Close()
	_, errNet := c.Bulk(context.Background(), testIndex, []catalog.Product{doc})
	for _, e := range []error{errHTTP, errNet} {
		if e == nil {
			t.Fatal("Bulk() error = nil, want error")
		}
		if s := e.Error(); strings.Contains(s, "tok3n") || strings.Contains(s, "secret product name") || strings.Contains(s, "id-secret-doc") {
			t.Errorf("error echoes a path or document: %q", s)
		}
	}
	if s := logs.String(); strings.Contains(s, "tok3n") || strings.Contains(s, "secret product name") {
		t.Errorf("log echoes a path or document: %q", s)
	}
}

func TestBulk_ErrorStatusWithUnparsableBodyKeepsTheStatus(t *testing.T) {
	b := newBulkServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("plain text"))
	})
	c := b.client(t)

	_, err := c.Bulk(context.Background(), testIndex, prods(1))
	if err == nil || !strings.Contains(err.Error(), "418") {
		t.Errorf("Bulk() error = %v, want an error that names status 418", err)
	}
}

func TestBulk_ErrorObjectWithEmptyTypeIsUnknown(t *testing.T) {
	b := newBulkServer(t, respondWith(`{"errors":true,"items":[{"index":{"status":400,"error":{"type":"","reason":"r"}}}]}`))
	c := b.client(t)

	got, err := c.Bulk(context.Background(), testIndex, prods(1))
	if err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if got.FailureTypes["unknown"] != 1 || len(got.FailureTypes) != 1 {
		t.Errorf("FailureTypes = %v, want {unknown:1}, not an empty-string key", got.FailureTypes)
	}
}

func TestBulk_RetryResendsTheSameBody(t *testing.T) {
	b := newBulkServer(t, nil)
	b.h = func(w http.ResponseWriter, _ *http.Request) {
		if b.calls() == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, `{"error":"busy","status":503}`)
			return
		}
		writeJSON(w, bulkJSON(okSpecs(2)...))
	}
	c := b.client(t)

	got, err := c.Bulk(context.Background(), testIndex, prods(2))
	if err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if b.calls() != 2 || b.bodies[0] == "" || b.bodies[0] != b.bodies[1] || got.Indexed != 2 {
		t.Errorf("calls = %d, bodies equal = %v, Indexed = %d, want a retry with the identical non-empty body", b.calls(), b.bodies[0] == b.bodies[len(b.bodies)-1], got.Indexed)
	}
}

func TestBulk_BodyKeepsHTMLCharactersUnescaped(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(1)...)))
	c := b.client(t)

	d := prod("1")
	d.Name = "Ben & Jerry's <Cookie>"
	if _, err := c.Bulk(context.Background(), testIndex, []catalog.Product{d}); err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if body := b.lastBody(); !strings.Contains(body, `Ben & Jerry's <Cookie>`) || strings.Contains(body, `\u00`) {
		t.Errorf("body = %q, want HTML characters written as they are", body)
	}
}
