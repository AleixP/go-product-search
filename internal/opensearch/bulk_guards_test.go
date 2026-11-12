package opensearch

import (
	"context"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// A 200 response with a malformed body gives an error without a single cause
// (Unwrap is nil); the message must keep the decoding cause.
func TestBulk_MalformedResponseBodyKeepsItsCause(t *testing.T) {
	b := newBulkServer(t, respondWith(`{"items": "not a list"`))
	c := b.client(t)

	_, err := c.Bulk(context.Background(), testIndex, prods(1))
	if err == nil {
		t.Fatal("Bulk() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("error = %q, want the cause (unmarshal) kept", err)
	}
	if strings.Contains(err.Error(), "%!w") {
		t.Errorf("error = %q, want no formatting artifact from a nil cause", err)
	}
}

func TestBulk_RefusesBlankIDs(t *testing.T) {
	for _, id := range []string{"", " ", "\t", "\n", "   ", " \t\n "} {
		t.Run(strings.NewReplacer(" ", "sp", "\t", "tab", "\n", "nl").Replace("id:"+id), func(t *testing.T) {
			b := newBulkServer(t, respondWith(bulkJSON(okSpecs(2)...)))
			c := b.client(t)
			docs := []catalog.Product{prod("ok"), {ID: id, Name: "x"}}

			_, err := c.Bulk(context.Background(), testIndex, docs)
			if err == nil {
				t.Fatalf("Bulk() with ID %q: error = nil, want error", id)
			}
			if !strings.Contains(err.Error(), "document 1") {
				t.Errorf("error = %q, want the position of the document", err)
			}
			if b.calls() != 0 {
				t.Errorf("%d request(s) sent, want none", b.calls())
			}
		})
	}
}

// The ID is sent exactly as given: only an ID with no content is refused.
func TestBulk_IDWithSurroundingSpacesIsSentUntrimmed(t *testing.T) {
	b := newBulkServer(t, respondWith(bulkJSON(okSpecs(1)...)))
	c := b.client(t)

	if _, err := c.Bulk(context.Background(), testIndex, []catalog.Product{{ID: " 12 ", Name: "x"}}); err != nil {
		t.Fatalf("Bulk() error = %v", err)
	}
	if want := `"_id":" 12 "`; !strings.Contains(b.lastBody(), want) {
		t.Errorf("body = %q, want it to contain %s", b.lastBody(), want)
	}
}
