package search_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

func TestSuggestBody_Golden(t *testing.T) {
	tests := []struct {
		file string
		q    catalog.SuggestQuery
	}{
		{"suggest_simple.json", catalog.SuggestQuery{Text: "choc", Size: 10}},
		{"suggest_multiword.json", catalog.SuggestQuery{Text: "dark choc", Size: 5}},
		{"suggest_escaping.json", catalog.SuggestQuery{Text: `Crème "brûlée" <b> & \ x`, Size: 20}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			body, err := search.SuggestBody(tt.q)
			if err != nil {
				t.Fatalf("SuggestBody(%+v) error = %v", tt.q, err)
			}
			assertGolden(t, tt.file, body)
		})
	}
}

// topLevelKeys returns the object keys of raw JSON in document order.
func topLevelKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("body does not start with an object: %v %v", tok, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestSuggestBody_Structure(t *testing.T) {
	q := catalog.SuggestQuery{Text: "dark choc", Size: 7}
	body, err := search.SuggestBody(q)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"size", "track_total_hits", "_source", "query", "collapse", "sort"}
	if got := topLevelKeys(t, body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("top-level keys = %v, want %v", got, want)
	}
	for _, banned := range []string{"fuzziness", "aggs", "post_filter", "from"} {
		if bytes.Contains(body, []byte(`"`+banned+`"`)) {
			t.Errorf("body contains %q: %s", banned, body)
		}
	}
	var doc struct {
		Size   int      `json:"size"`
		Source []string `json:"_source"`
		Query  struct {
			Match map[string]struct {
				Query    string `json:"query"`
				Operator string `json:"operator"`
			} `json:"match"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Size != 7 {
		t.Errorf("size = %d, want 7", doc.Size)
	}
	if len(doc.Source) != 1 || doc.Source[0] != "name" {
		t.Errorf("_source = %v, want [name]", doc.Source)
	}
	m, ok := doc.Query.Match["name.autocomplete"]
	if !ok || len(doc.Query.Match) != 1 {
		t.Fatalf("match = %v, want only name.autocomplete", doc.Query.Match)
	}
	if m.Operator != "and" {
		t.Errorf("operator = %q, want and", m.Operator)
	}
	if m.Query != q.Text {
		t.Errorf("query = %q, want %q", m.Query, q.Text)
	}
}

func TestSuggestBody_TextStaysAString(t *testing.T) {
	text := `x","collapse":{},"size":999,"sort":[]}`
	body, err := search.SuggestBody(catalog.SuggestQuery{Text: text, Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"size", "track_total_hits", "_source", "query", "collapse", "sort"}
	if got := topLevelKeys(t, body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("top-level keys = %v, want %v", got, want)
	}
	var doc struct {
		Size  int `json:"size"`
		Query struct {
			Match map[string]struct {
				Query string `json:"query"`
			} `json:"match"`
		} `json:"query"`
		Collapse struct {
			Field string `json:"field"`
		} `json:"collapse"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Query.Match["name.autocomplete"].Query; got != text {
		t.Errorf("query = %q, want %q", got, text)
	}
	if doc.Size != 3 || doc.Collapse.Field != "name.norm" {
		t.Errorf("size = %d, collapse = %q: text leaked into the DSL", doc.Size, doc.Collapse.Field)
	}
}

func TestSuggestBody_DeterministicAndQueryUntouched(t *testing.T) {
	q := catalog.SuggestQuery{Text: "dark choc", Size: 5}
	orig := q
	first, err := search.SuggestBody(q)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		got, err := search.SuggestBody(q)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("call %d differs:\n%s\nvs\n%s", i, got, first)
		}
	}
	if q != orig {
		t.Errorf("query modified: %+v, want %+v", q, orig)
	}
}
