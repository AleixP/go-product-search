package search_test

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

// kv is one bucket (key, doc_count) of a fake engine response.
type kv struct {
	k string
	n int
}

func buckets(bs ...kv) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = fmt.Sprintf(`{"key":%q,"doc_count":%d}`, b.k, b.n)
	}
	return `{"buckets":[` + strings.Join(parts, ",") + `]}`
}

// counts builds an anonymous filters response: keys are not named.
func counts(ns ...int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprintf(`{"doc_count":%d}`, n)
	}
	return `{"buckets":[` + strings.Join(parts, ",") + `]}`
}

func termsFacet(top, selected string) string {
	s := `{"doc_count":99,"top":` + top
	if selected != "" {
		s += `,"selected":` + selected
	}
	return s + `}`
}

func gradesFacet(grades string) string {
	return `{"doc_count":99,"grades":` + grades + `}`
}

func response(brands, categories, nutriscore string) json.RawMessage {
	return json.RawMessage(`{"brands":` + brands + `,"categories":` + categories + `,"nutriscore":` + nutriscore + `}`)
}

var zeroGrades = gradesFacet(counts(0, 0, 0, 0, 0))

func fv(v string, n int) catalog.FacetValue { return catalog.FacetValue{Value: v, Count: n} }

func TestParseFacets_Brands(t *testing.T) {
	tests := []struct {
		name string
		q    catalog.Query
		top  string
		sel  string
		want []catalog.FacetValue
	}{
		{"top only keeps order", catalog.Query{},
			buckets(kv{"acme", 5}, kv{"bo", 3}), "",
			[]catalog.FacetValue{fv("acme", 5), fv("bo", 3)}},
		{"selected already in top is not duplicated", catalog.Query{Brands: []string{"bo"}},
			buckets(kv{"acme", 5}, kv{"bo", 3}), counts(3),
			[]catalog.FacetValue{fv("acme", 5), fv("bo", 3)}},
		{"in top the top entry wins over the selected count", catalog.Query{Brands: []string{"bo"}},
			buckets(kv{"acme", 5}, kv{"bo", 3}), counts(7),
			[]catalog.FacetValue{fv("acme", 5), fv("bo", 3)}},
		{"selected outside top is appended with its count", catalog.Query{Brands: []string{"zed"}},
			buckets(kv{"acme", 5}, kv{"bo", 3}), counts(4),
			[]catalog.FacetValue{fv("acme", 5), fv("zed", 4), fv("bo", 3)}},
		{"selected with count zero is kept last", catalog.Query{Brands: []string{"none"}},
			buckets(kv{"acme", 5}), counts(0),
			[]catalog.FacetValue{fv("acme", 5), fv("none", 0)}},
		{"ties ordered by value ascending bytewise", catalog.Query{Brands: []string{"b", "a"}},
			buckets(kv{"c", 2}, kv{"B", 2}), counts(2, 2),
			[]catalog.FacetValue{fv("B", 2), fv("a", 2), fv("b", 2), fv("c", 2)}},
		{"several appended values sorted into place", catalog.Query{Brands: []string{"m", "x", "y"}},
			buckets(kv{"acme", 9}, kv{"bo", 1}), counts(5, 1, 0),
			[]catalog.FacetValue{fv("acme", 9), fv("m", 5), fv("bo", 1), fv("x", 1), fv("y", 0)}},
		{"empty top and selected values", catalog.Query{Brands: []string{"a"}},
			buckets(), counts(0),
			[]catalog.FacetValue{fv("a", 0)}},
		{"duplicate selected value is listed once, first count", catalog.Query{Brands: []string{"a", "a"}},
			buckets(), counts(2, 9),
			[]catalog.FacetValue{fv("a", 2)}},
		{"duplicate top key keeps the first", catalog.Query{},
			buckets(kv{"a", 3}, kv{"a", 2}), "",
			[]catalog.FacetValue{fv("a", 3)}},
		{"count zero in top is allowed", catalog.Query{},
			buckets(kv{"a", 0}), "",
			[]catalog.FacetValue{fv("a", 0)}},
		{"max int32 count is accepted", catalog.Query{},
			buckets(kv{"a", math.MaxInt32}), "",
			[]catalog.FacetValue{fv("a", math.MaxInt32)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.Brands = append([]string(nil), tt.q.Brands...)
			if len(tt.q.Brands) == 0 {
				tt.q.Brands = nil
			}
			got, err := search.ParseFacets(response(termsFacet(tt.top, tt.sel), termsFacet(buckets(), ""), zeroGrades), tt.q)
			if err != nil {
				t.Fatalf("ParseFacets error = %v", err)
			}
			if !reflect.DeepEqual(got.Brands, tt.want) {
				t.Errorf("Brands = %v, want %v", got.Brands, tt.want)
			}
		})
	}
}

func TestParseFacets_CategoriesUseCategoryValues(t *testing.T) {
	q := catalog.Query{Brands: []string{"b1"}, Categories: []string{"teas"}}
	got, err := search.ParseFacets(response(
		termsFacet(buckets(kv{"acme", 1}), counts(0)),
		termsFacet(buckets(kv{"snacks", 4}), counts(2)),
		zeroGrades), q)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if want := []catalog.FacetValue{fv("acme", 1), fv("b1", 0)}; !reflect.DeepEqual(got.Brands, want) {
		t.Errorf("Brands = %v, want %v", got.Brands, want)
	}
	if want := []catalog.FacetValue{fv("snacks", 4), fv("teas", 2)}; !reflect.DeepEqual(got.Categories, want) {
		t.Errorf("Categories = %v, want %v", got.Categories, want)
	}
}

func TestParseFacets_Nutriscore(t *testing.T) {
	got, err := search.ParseFacets(response(termsFacet(buckets(), ""), termsFacet(buckets(), ""),
		gradesFacet(counts(1, 9, 0, 4, 0))), catalog.Query{Nutriscore: []string{"b"}})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	want := []catalog.FacetValue{fv("a", 1), fv("b", 9), fv("c", 0), fv("d", 4), fv("e", 0)}
	if !reflect.DeepEqual(got.Nutriscore, want) {
		t.Errorf("Nutriscore = %v, want %v (grade order, not count order)", got.Nutriscore, want)
	}
}

func TestParseFacets_ListsNeverNil(t *testing.T) {
	got, err := search.ParseFacets(response(termsFacet(buckets(), ""), termsFacet(buckets(), ""), zeroGrades), catalog.Query{})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if got.Brands == nil || got.Categories == nil || got.Nutriscore == nil {
		t.Fatalf("nil list in %#v", got)
	}
	if len(got.Brands) != 0 || len(got.Categories) != 0 || len(got.Nutriscore) != 5 {
		t.Errorf("lengths = %d/%d/%d, want 0/0/5", len(got.Brands), len(got.Categories), len(got.Nutriscore))
	}
}

func TestParseFacets_MaxEntries(t *testing.T) {
	var top []kv
	for i := 0; i < 10; i++ {
		top = append(top, kv{fmt.Sprintf("t%02d", i), 100 - i})
	}
	var vals []string
	var sel []int
	for i := 0; i < 20; i++ {
		vals = append(vals, fmt.Sprintf("v%02d", i))
		sel = append(sel, i)
	}
	got, err := search.ParseFacets(response(termsFacet(buckets(top...), counts(sel...)), termsFacet(buckets(), ""), zeroGrades),
		catalog.Query{Brands: vals})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(got.Brands) != 30 {
		t.Fatalf("len = %d, want 30", len(got.Brands))
	}
	for i := 1; i < len(got.Brands); i++ {
		a, b := got.Brands[i-1], got.Brands[i]
		if a.Count < b.Count || (a.Count == b.Count && a.Value >= b.Value) {
			t.Fatalf("not sorted at %d: %v then %v", i, a, b)
		}
	}
}

func TestParseFacets_Errors(t *testing.T) {
	okB := termsFacet(buckets(), "")
	okSel := termsFacet(buckets(), counts(0))
	q1 := catalog.Query{Brands: []string{"a"}}
	tests := []struct {
		name string
		aggs json.RawMessage
		q    catalog.Query
		want string
	}{
		{"nil", nil, catalog.Query{}, "missing aggregations"},
		{"empty", json.RawMessage(""), catalog.Query{}, "missing aggregations"},
		{"null", json.RawMessage("null"), catalog.Query{}, "missing aggregations"},
		{"null with spaces", json.RawMessage(" null "), catalog.Query{}, "missing aggregations"},
		{"brands absent", json.RawMessage(`{"categories":` + okB + `,"nutriscore":` + zeroGrades + `}`), catalog.Query{}, "missing aggregation brands"},
		{"categories absent", json.RawMessage(`{"brands":` + okB + `,"nutriscore":` + zeroGrades + `}`), catalog.Query{}, "missing aggregation categories"},
		{"nutriscore absent", json.RawMessage(`{"brands":` + okB + `,"categories":` + okB + `}`), catalog.Query{}, "missing aggregation nutriscore"},
		{"brands null", response("null", okB, zeroGrades), catalog.Query{}, "missing aggregation brands"},
		{"empty object", json.RawMessage(`{}`), catalog.Query{}, "missing aggregation brands"},
		{"brands without top", response(`{"doc_count":1}`, okB, zeroGrades), catalog.Query{}, "missing aggregation brands"},
		{"nutriscore without grades", response(okB, okB, `{"doc_count":1}`), catalog.Query{}, "missing aggregation nutriscore"},
		{"selected too few", response(termsFacet(buckets(), counts(1)), okB, zeroGrades),
			catalog.Query{Brands: []string{"a", "b"}}, "aggregation brands: 1 selected buckets for 2 values"},
		{"selected too many", response(okB, termsFacet(buckets(), counts(1, 2)), zeroGrades),
			catalog.Query{Categories: []string{"a"}}, "aggregation categories: 2 selected buckets for 1 values"},
		{"selected missing though values", response(okB, okB, zeroGrades), q1, "aggregation brands: 0 selected buckets for 1 values"},
		{"selected present without values", response(termsFacet(buckets(), counts(1)), okB, zeroGrades),
			catalog.Query{}, "aggregation brands: 1 selected buckets for 0 values"},
		{"grades too few", response(okB, okB, gradesFacet(counts(1, 2, 3, 4))), catalog.Query{},
			"aggregation nutriscore: 4 grade buckets for 5 grades"},
		{"grades too many", response(okB, okB, gradesFacet(counts(1, 2, 3, 4, 5, 6))), catalog.Query{},
			"aggregation nutriscore: 6 grade buckets for 5 grades"},
		{"not json", json.RawMessage(`{`), catalog.Query{}, "decode aggregations"},
		{"array", json.RawMessage(`[]`), catalog.Query{}, "decode aggregations"},
		{"string", json.RawMessage(`"x"`), catalog.Query{}, "decode aggregations"},
		{"buckets as object", response(termsFacet(`{"buckets":{"a":{"doc_count":1}}}`, ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"selected buckets as object", response(termsFacet(buckets(), `{"buckets":{"a":{"doc_count":1}}}`), okB, zeroGrades), q1, "decode aggregations"},
		{"grades buckets as object", response(okB, okB, gradesFacet(`{"buckets":{"a":{"doc_count":1}}}`)), catalog.Query{}, "decode aggregations"},
		{"key not a string", response(termsFacet(`{"buckets":[{"key":5,"doc_count":1}]}`, ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"doc_count not a number", response(termsFacet(`{"buckets":[{"key":"a","doc_count":"1"}]}`, ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"fractional doc_count", response(termsFacet(`{"buckets":[{"key":"a","doc_count":1.5}]}`, ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"missing doc_count", response(termsFacet(`{"buckets":[{"key":"a"}]}`, ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"null bucket", response(termsFacet(`{"buckets":[null]}`, ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"negative doc_count in top", response(termsFacet(buckets(kv{"a", -1}), ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"negative doc_count in selected", response(termsFacet(buckets(), counts(-1)), okB, zeroGrades), q1, "decode aggregations"},
		{"negative doc_count in grades", response(okB, okB, gradesFacet(counts(0, 0, -3, 0, 0))), catalog.Query{}, "decode aggregations"},
		{"huge doc_count", response(termsFacet(buckets(kv{"a", math.MaxInt32 + 1}), ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"doc_count overflowing int64", response(termsFacet(`{"buckets":[{"key":"a","doc_count":99999999999999999999}]}`, ""), okB, zeroGrades), catalog.Query{}, "decode aggregations"},
		{"huge doc_count in selected", response(termsFacet(buckets(), counts(math.MaxInt32+1)), okB, zeroGrades), q1, "decode aggregations"},
		{"selected ok control", response(okSel, okB, zeroGrades), q1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := search.ParseFacets(tt.aggs, tt.q)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("no error, want %q", tt.want)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestParseFacets_ErrorsNeverLeakResponseText(t *testing.T) {
	const marker = "SECRET-MARKER-7"
	okB := termsFacet(buckets(), "")
	q := catalog.Query{Brands: []string{"a"}}
	bad := []json.RawMessage{
		json.RawMessage(`{"` + marker + `":` + `{`),
		json.RawMessage(`"` + marker + `"`),
		response(termsFacet(`{"buckets":[{"key":"`+marker+`","doc_count":"x"}]}`, ""), okB, zeroGrades),
		response(termsFacet(`{"buckets":{"`+marker+`":{"doc_count":1}}}`, ""), okB, zeroGrades),
		response(termsFacet(buckets(kv{marker, -1}), ""), okB, zeroGrades),
		response(termsFacet(buckets(kv{marker, math.MaxInt32 + 1}), ""), okB, zeroGrades),
		response(termsFacet(buckets(kv{marker, 1}), counts(1, 2)), okB, zeroGrades),
		response(okB, okB, gradesFacet(`{"buckets":[{"key":"`+marker+`","doc_count":1}]}`)),
		json.RawMessage(`{"brands":{"top":{"buckets":[]}},"categories":{"top":{"buckets":[]}},"nutriscore":{"` + marker + `":1}}`),
		json.RawMessage(`{"` + marker + `":1}`),
	}
	for i, b := range bad {
		_, err := search.ParseFacets(b, q)
		if err == nil {
			t.Fatalf("case %d: no error for %s", i, b)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("case %d: error %q leaks response text", i, err)
		}
	}
}

// TestParseFacets_ReadsWhatBodyWrites pins the contract between Body and
// ParseFacets: every path ParseFacets decodes exists in the body Body emits.
func TestParseFacets_ReadsWhatBodyWrites(t *testing.T) {
	q := catalog.Query{Page: 1, Size: 20, Brands: []string{"acme", "bo"}, Categories: []string{"teas"}, Nutriscore: []string{"a"}}
	raw, err := search.Body(q)
	if err != nil {
		t.Fatalf("Body error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	dig := func(path ...string) any {
		t.Helper()
		var cur any = body
		for _, p := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("path %v: %q parent is not an object", path, p)
			}
			if cur, ok = m[p]; !ok {
				t.Fatalf("path %v: key %q missing in Body output", path, p)
			}
		}
		return cur
	}
	selectedLen := func(path ...string) int {
		return len(dig(append(path, "filters", "filters")...).([]any))
	}
	for _, f := range []string{"brands", "categories"} {
		dig("aggs", f, "aggs", "top", "terms")
		dig("aggs", f, "aggs", "selected")
	}
	if n := selectedLen("aggs", "brands", "aggs", "selected"); n != 2 {
		t.Errorf("brands selected clauses = %d, want 2", n)
	}
	if n := selectedLen("aggs", "categories", "aggs", "selected"); n != 1 {
		t.Errorf("categories selected clauses = %d, want 1", n)
	}
	if n := selectedLen("aggs", "nutriscore", "aggs", "grades"); n != 5 {
		t.Errorf("grades clauses = %d, want 5", n)
	}

	// A response shaped as the engine answers Body(q) decodes cleanly.
	got, err := search.ParseFacets(response(
		termsFacet(buckets(kv{"acme", 3}), counts(3, 1)),
		termsFacet(buckets(kv{"snacks", 2}), counts(0)),
		gradesFacet(counts(1, 1, 1, 1, 1))), q)
	if err != nil {
		t.Fatalf("ParseFacets error = %v", err)
	}
	if want := []catalog.FacetValue{fv("acme", 3), fv("bo", 1)}; !reflect.DeepEqual(got.Brands, want) {
		t.Errorf("Brands = %v, want %v", got.Brands, want)
	}
	if want := []catalog.FacetValue{fv("snacks", 2), fv("teas", 0)}; !reflect.DeepEqual(got.Categories, want) {
		t.Errorf("Categories = %v, want %v", got.Categories, want)
	}
}

func manyBuckets(n int) string {
	bs := make([]kv, n)
	for i := range bs {
		bs[i] = kv{fmt.Sprintf("k%02d", i), 100 - i}
	}
	return buckets(bs...)
}

func keys(vs []catalog.FacetValue) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Value
	}
	return out
}

func parseBrands(t *testing.T, top, sel string, q catalog.Query) []catalog.FacetValue {
	t.Helper()
	got, err := search.ParseFacets(response(termsFacet(top, sel), termsFacet(buckets(), ""), zeroGrades), q)
	if err != nil {
		t.Fatalf("ParseFacets error = %v", err)
	}
	return got.Brands
}

func TestParseFacets_TopBucketsAreCappedAtFacetSize(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want int
	}{
		{"fifty top buckets", 50, 10},
		{"exactly ten are accepted", 10, 10},
		{"eleventh is dropped", 11, 10},
		{"nine are kept", 9, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBrands(t, manyBuckets(tt.n), "", catalog.Query{})
			if len(got) != tt.want {
				t.Fatalf("len(Brands) = %d, want %d (%v)", len(got), tt.want, keys(got))
			}
			for i, v := range got {
				if want := fv(fmt.Sprintf("k%02d", i), 100-i); v != want {
					t.Errorf("Brands[%d] = %v, want %v", i, v, want)
				}
			}
		})
	}
}

func TestParseFacets_SelectedFromDroppedTailIsStillMerged(t *testing.T) {
	q := catalog.Query{Brands: []string{"k15", "k03"}}
	got := parseBrands(t, manyBuckets(20), counts(7, 1), q)
	if len(got) != 11 {
		t.Fatalf("len(Brands) = %d, want 11 (%v)", len(got), keys(got))
	}
	// k03 is in the kept head so its top count wins; k15 was cut from top and is
	// appended with its selected count.
	if want := fv("k15", 7); got[10] != want {
		t.Errorf("Brands[10] = %v, want %v", got[10], want)
	}
	if want := fv("k03", 97); got[3] != want {
		t.Errorf("Brands[3] = %v, want %v", got[3], want)
	}
}

func TestParseFacets_SelectedValuesMergeByExactKey(t *testing.T) {
	t.Run("case-different selected value is a different key", func(t *testing.T) {
		got := parseBrands(t, buckets(kv{"a", 5}), counts(1), catalog.Query{Brands: []string{"A"}})
		want := []catalog.FacetValue{fv("a", 5), fv("A", 1)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Brands = %v, want %v", got, want)
		}
	})
	t.Run("selected values differing only in case are both kept", func(t *testing.T) {
		got := parseBrands(t, buckets(), counts(2, 3), catalog.Query{Brands: []string{"A", "a"}})
		want := []catalog.FacetValue{fv("a", 3), fv("A", 2)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Brands = %v, want %v", got, want)
		}
	})
	t.Run("top keys differing only in case are both kept", func(t *testing.T) {
		got := parseBrands(t, buckets(kv{"a", 3}, kv{"A", 2}), "", catalog.Query{})
		want := []catalog.FacetValue{fv("a", 3), fv("A", 2)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Brands = %v, want %v", got, want)
		}
	})
}

// encoding/json matches struct keys case-insensitively, so a response key
// "Top" is accepted. The decoder is deliberately left as is.
func TestParseFacets_ResponseKeysMatchCaseInsensitively(t *testing.T) {
	brands := `{"doc_count":99,"Top":` + buckets(kv{"a", 1}) + `}`
	got, err := search.ParseFacets(response(brands, termsFacet(buckets(), ""), zeroGrades), catalog.Query{})
	if err != nil {
		t.Fatalf("ParseFacets error = %v", err)
	}
	if want := []catalog.FacetValue{fv("a", 1)}; !reflect.DeepEqual(got.Brands, want) {
		t.Errorf("Brands = %v, want %v", got.Brands, want)
	}
}
