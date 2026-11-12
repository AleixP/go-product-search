package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	osclient "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/errmask"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// bulkServer answers pings and records every non-ping request; h writes the
// response of each bulk call.
type bulkServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	methods []string
	paths   []string
	bodies  []string
	h       func(w http.ResponseWriter, r *http.Request)
}

func newBulkServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *bulkServer {
	t.Helper()
	b := &bulkServer{h: h}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.methods = append(b.methods, r.Method)
		b.paths = append(b.paths, r.URL.Path)
		b.bodies = append(b.bodies, string(body))
		b.mu.Unlock()
		b.h(w, r)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bulkServer) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.bodies)
}

func (b *bulkServer) lastBody() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bodies[len(b.bodies)-1]
}

func (b *bulkServer) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(context.Background(), b.srv.URL)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

// newClientWith builds a Client with a custom transport config and error mask.
func newClientWith(t *testing.T, cfg osclient.Config, mask *errmask.ErrorMask) *Client {
	t.Helper()
	api, err := opensearchapi.NewClient(opensearchapi.Config{Client: cfg, Errors: mask})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return &Client{api: api, now: time.Now, log: slog.New(slog.DiscardHandler)}
}

type itemSpec struct {
	status  int
	errType string
}

// bulkJSON builds a _bulk response whose item _ids are "resp-<i>", so a test
// can tell the response ids from the request ids.
func bulkJSON(specs ...itemSpec) string {
	items := make([]map[string]any, 0, len(specs))
	anyErr := false
	for i, s := range specs {
		it := map[string]any{"_index": "products-x", "_id": fmt.Sprintf("resp-%d", i), "status": s.status}
		if s.errType != "" {
			anyErr = true
			it["error"] = map[string]any{"type": s.errType, "reason": "reason " + s.errType}
		}
		items = append(items, map[string]any{"index": it})
	}
	b, _ := json.Marshal(map[string]any{"took": 1, "errors": anyErr, "items": items})
	return string(b)
}

func respondWith(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) }
}

func okSpecs(n int) []itemSpec {
	out := make([]itemSpec, n)
	for i := range out {
		out[i] = itemSpec{status: 201}
	}
	return out
}

func prod(id string) catalog.Product {
	return catalog.Product{ID: id, Name: "name " + id, Brands: []string{"b"}, Categories: []string{"c"}, Nutriscore: "a"}
}

func prods(n int) []catalog.Product {
	out := make([]catalog.Product, n)
	for i := range out {
		out[i] = prod(fmt.Sprintf("id%d", i))
	}
	return out
}

const testIndex = "products-20261109t120000z"

func TestBulk_AllItemsSucceed(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(3)...)))
	c := b.client(t)

	got, err := c.Bulk(context.Background(), testIndex, prods(3))
	if err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if got.Indexed != 3 || got.Failed != 0 || len(got.FailureTypes) != 0 || len(got.FailedIDs) != 0 {
		t.Errorf("Bulk() = %+v, want 3 indexed and nothing failed", got)
	}
	if got.FailureTypes == nil {
		t.Error("FailureTypes = nil, want an empty non-nil map")
	}
}

func TestBulk_MixedItemsAreCountedPerItem(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(
		itemSpec{status: 201},
		itemSpec{status: 400, errType: "mapper_parsing_exception"},
		itemSpec{status: 429, errType: "es_rejected_execution_exception"},
		itemSpec{status: 200},
		itemSpec{status: 400, errType: "mapper_parsing_exception"},
	)))
	c := b.client(t)

	got, err := c.Bulk(context.Background(), testIndex, prods(5))
	if err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if got.Indexed != 2 || got.Failed != 3 {
		t.Errorf("Indexed, Failed = %d, %d, want 2, 3", got.Indexed, got.Failed)
	}
	want := map[string]int{"mapper_parsing_exception": 2, "es_rejected_execution_exception": 1}
	if fmt.Sprint(got.FailureTypes) != fmt.Sprint(want) {
		t.Errorf("FailureTypes = %v, want %v", got.FailureTypes, want)
	}
	// Ids come from the request (position), not from the response "resp-<i>" ids.
	if fmt.Sprint(got.FailedIDs) != "[id1 id2 id4]" {
		t.Errorf("FailedIDs = %v, want [id1 id2 id4]", got.FailedIDs)
	}
}

func TestBulk_StatusThreshold(t *testing.T) {
	tests := []struct {
		status     int
		wantFailed bool
	}{
		{0, true}, {199, true}, {200, false}, {201, false}, {299, false},
		{300, true}, {301, true}, {400, true}, {429, true}, {500, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			b := newBulkServer(t, respondWith(bulkJSON(itemSpec{status: tt.status, errType: "x_exception"})))
			c := b.client(t)

			got, err := c.Bulk(context.Background(), testIndex, prods(1))
			if err != nil {
				t.Fatalf("Bulk() error = %v", err)
			}
			if (got.Failed == 1) != tt.wantFailed || got.Indexed+got.Failed != 1 {
				t.Errorf("status %d: Indexed, Failed = %d, %d, wantFailed %v", tt.status, got.Indexed, got.Failed, tt.wantFailed)
			}
		})
	}
}

func TestBulk_FailedWithoutErrorObjectHasUnknownType(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(itemSpec{status: 503})))
	c := b.client(t)

	got, err := c.Bulk(context.Background(), testIndex, prods(1))
	if err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if got.Failed != 1 || got.FailureTypes["unknown"] != 1 {
		t.Errorf("Bulk() = %+v, want 1 failure of type unknown", got)
	}
}

func TestBulk_FailedIDsCapAtTen(t *testing.T) {
	for _, failures := range []int{9, 10, 11, 25} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			specs := make([]itemSpec, failures)
			for i := range specs {
				specs[i] = itemSpec{status: 400, errType: "mapper_parsing_exception"}
			}
			b := newBulkServer(t, respondWith(bulkJSON(specs...)))
			c := b.client(t)

			got, err := c.Bulk(context.Background(), testIndex, prods(failures))
			if err != nil {
				t.Fatalf("Bulk() error = %v", err)
			}
			if want := min(failures, 10); len(got.FailedIDs) != want {
				t.Errorf("len(FailedIDs) = %d, want %d", len(got.FailedIDs), want)
			}
			if got.FailedIDs[0] != "id0" || got.FailedIDs[len(got.FailedIDs)-1] != fmt.Sprintf("id%d", min(failures, 10)-1) {
				t.Errorf("FailedIDs = %v, want the first ids in order", got.FailedIDs)
			}
			if got.Failed != failures {
				t.Errorf("Failed = %d, want %d (the cap must not limit the count)", got.Failed, failures)
			}
		})
	}
}

func TestBulk_FailureTypesCapAt32ThenOther(t *testing.T) {
	for _, distinct := range []int{31, 32, 33, 40} {
		t.Run(fmt.Sprint(distinct), func(t *testing.T) {
			// One failure per distinct type, then 5 more of the first type.
			specs := make([]itemSpec, 0, distinct+5)
			for i := range distinct {
				specs = append(specs, itemSpec{status: 400, errType: fmt.Sprintf("type_%02d", i)})
			}
			for range 5 {
				specs = append(specs, itemSpec{status: 400, errType: "type_00"})
			}
			b := newBulkServer(t, respondWith(bulkJSON(specs...)))
			c := b.client(t)

			got, err := c.Bulk(context.Background(), testIndex, prods(len(specs)))
			if err != nil {
				t.Fatalf("Bulk() error = %v", err)
			}
			wantOther := max(0, distinct-32)
			wantKeys := min(distinct, 32)
			if wantOther > 0 {
				wantKeys++
			}
			if len(got.FailureTypes) != wantKeys || got.FailureTypes["other"] != wantOther {
				t.Errorf("keys, other = %d, %d, want %d, %d (%v)", len(got.FailureTypes), got.FailureTypes["other"], wantKeys, wantOther, got.FailureTypes)
			}
			if got.FailureTypes["type_00"] != 6 {
				t.Errorf("type_00 = %d, want 6: a known type keeps counting after the cap", got.FailureTypes["type_00"])
			}
			sum := 0
			for _, n := range got.FailureTypes {
				sum += n
			}
			if sum != got.Failed || got.Failed != len(specs) {
				t.Errorf("sum of types = %d, Failed = %d, want both %d", sum, got.Failed, len(specs))
			}
		})
	}
}

func TestBulk_ItemsMissingFromResponseCountAsFailed(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantIdxd   int
		wantFailed int
		wantIDs    string
	}{
		{"fewer items", bulkJSON(okSpecs(1)...), 1, 2, "[id1 id2]"},
		{"no items", `{"took":1,"errors":false,"items":[]}`, 0, 3, "[id0 id1 id2]"},
		{"no items field", `{"took":1,"errors":false}`, 0, 3, "[id0 id1 id2]"},
		{"empty item object", `{"errors":false,"items":[{"index":{"_id":"r","status":201}},{},{"index":{"_id":"r","status":201}}]}`, 2, 1, "[id1]"},
		{"item with two actions", `{"errors":false,"items":[{"index":{"status":201},"create":{"status":201}},{"index":{"status":201}},{"index":{"status":201}}]}`, 2, 1, "[id0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newBulkServer(t, respondWith(tt.body))
			c := b.client(t)

			got, err := c.Bulk(context.Background(), testIndex, prods(3))
			if err != nil {
				t.Fatalf("Bulk() error = %v", err)
			}
			if got.Indexed != tt.wantIdxd || got.Failed != tt.wantFailed {
				t.Errorf("Indexed, Failed = %d, %d, want %d, %d", got.Indexed, got.Failed, tt.wantIdxd, tt.wantFailed)
			}
			if got.FailureTypes["missing from bulk response"] != tt.wantFailed {
				t.Errorf("FailureTypes = %v, want %d x %q", got.FailureTypes, tt.wantFailed, "missing from bulk response")
			}
			if fmt.Sprint(got.FailedIDs) != tt.wantIDs {
				t.Errorf("FailedIDs = %v, want %s", got.FailedIDs, tt.wantIDs)
			}
		})
	}
}

func TestBulk_ExtraItemsInResponseAreIgnoredAndWarned(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(5)...)))
	c := b.client(t)
	var logs strings.Builder
	c.log = slog.New(slog.NewTextHandler(&logs, nil))

	got, err := c.Bulk(context.Background(), testIndex, prods(2))
	if err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if got.Indexed != 2 || got.Failed != 0 {
		t.Errorf("Indexed, Failed = %d, %d, want 2, 0 (never more than the documents sent)", got.Indexed, got.Failed)
	}
	if l := logs.String(); !strings.Contains(l, "level=WARN") || !strings.Contains(l, "items=5") || !strings.Contains(l, "docs=2") {
		t.Errorf("log = %q, want a WARN with items=5 docs=2", l)
	}
}

func TestBulk_ExactItemCountLogsNothing(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(2)...)))
	c := b.client(t)
	var logs strings.Builder
	c.log = slog.New(slog.NewTextHandler(&logs, nil))

	if _, err := c.Bulk(context.Background(), testIndex, prods(2)); err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("log = %q, want nothing when item and document counts match", logs.String())
	}
}
