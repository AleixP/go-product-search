package search_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// assertGolden indents body like the golden files (2 spaces, trailing newline)
// and compares it to testdata/file; with -update it rewrites the file first.
func assertGolden(t *testing.T, file string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	buf.WriteByte('\n')
	path := filepath.Join("testdata", file)
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("body differs from %s\ngot:\n%s\nwant:\n%s", path, buf.Bytes(), want)
	}
}

func TestBody_Golden(t *testing.T) {
	tests := []struct {
		file string
		q    catalog.Query
	}{
		{"match_all_default.json", catalog.Query{Text: "", Page: 1, Size: 20}},
		{"match_all_page3_size50.json", catalog.Query{Text: "", Page: 3, Size: 50}},
		{"text_simple.json", catalog.Query{Text: "chocolate", Page: 1, Size: 20}},
		{"text_escaping.json", catalog.Query{Text: `Crème "brûlée" <b> & \ x`, Page: 1, Size: 20}},
		{"text_last_window.json", catalog.Query{Text: "tea", Page: 100, Size: 100}},
		{"filter_brands.json", catalog.Query{Page: 1, Size: 20, Brands: []string{"acme", "bo"}}},
		{"filter_categories_text.json", catalog.Query{Text: "tea", Page: 1, Size: 20, Categories: []string{"teas"}}},
		{"filter_nutriscore.json", catalog.Query{Page: 1, Size: 20, Nutriscore: []string{"a", "b"}}},
		{"filter_all.json", catalog.Query{
			Text: "chocolate", Page: 2, Size: 20,
			Brands: []string{"nestle"}, Categories: []string{"snacks", "sweets"}, Nutriscore: []string{"e"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			body, err := search.Body(tt.q)
			if err != nil {
				t.Fatalf("Body(%+v) error = %v", tt.q, err)
			}
			assertGolden(t, tt.file, body)
		})
	}
}

func TestBody_FromIsPageOffset(t *testing.T) {
	tests := []struct {
		page, size, wantFrom int
	}{
		{1, 1, 0},
		{1, 100, 0},
		{2, 10, 10},
		{3, 50, 100},
		{7, 13, 78},
		{100, 100, 9900},
		{10000, 1, 9999},
	}
	for _, tt := range tests {
		body, err := search.Body(catalog.Query{Text: "x", Page: tt.page, Size: tt.size})
		if err != nil {
			t.Fatalf("Body(page %d, size %d) error = %v", tt.page, tt.size, err)
		}
		var got struct {
			From int `json:"from"`
			Size int `json:"size"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("Body(page %d, size %d) is not valid JSON: %v", tt.page, tt.size, err)
		}
		if got.From != tt.wantFrom || got.Size != tt.size {
			t.Errorf("Body(page %d, size %d): from=%d size=%d, want from=%d size=%d",
				tt.page, tt.size, got.From, got.Size, tt.wantFrom, tt.size)
		}
	}
}
