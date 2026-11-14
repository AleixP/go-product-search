package search_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

// t4Bodies holds the bodies the T-4 golden files had before facets existed.
// Body must still produce exactly these once "aggs" is removed.
var t4Bodies = []struct {
	name string
	q    catalog.Query
	want string
}{
	{"match_all_default", catalog.Query{Page: 1, Size: 20},
		`{"from":0,"size":20,"track_total_hits":true,"query":{"match_all":{}}}`},
	{"match_all_page3_size50", catalog.Query{Page: 3, Size: 50},
		`{"from":100,"size":50,"track_total_hits":true,"query":{"match_all":{}}}`},
	{"text_simple", catalog.Query{Text: "chocolate", Page: 1, Size: 20},
		`{"from":0,"size":20,"track_total_hits":true,"query":{"multi_match":{"query":"chocolate","type":"best_fields","fields":["name^3","brands.text^2","categories.text"],"operator":"or","fuzziness":"AUTO","prefix_length":1,"max_expansions":10}}}`},
	{"text_escaping", catalog.Query{Text: `Crème "brûlée" <b> & \ x`, Page: 1, Size: 20},
		`{"from":0,"size":20,"track_total_hits":true,"query":{"multi_match":{"query":"Crème \"brûlée\" <b> & \\ x","type":"best_fields","fields":["name^3","brands.text^2","categories.text"],"operator":"or","fuzziness":"AUTO","prefix_length":1,"max_expansions":10}}}`},
	{"text_last_window", catalog.Query{Text: "tea", Page: 100, Size: 100},
		`{"from":9900,"size":100,"track_total_hits":true,"query":{"multi_match":{"query":"tea","type":"best_fields","fields":["name^3","brands.text^2","categories.text"],"operator":"or","fuzziness":"AUTO","prefix_length":1,"max_expansions":10}}}`},
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, b)
	}
	return m
}

func TestBody_T4BodiesUnchangedExceptAggs(t *testing.T) {
	for _, tt := range t4Bodies {
		t.Run(tt.name, func(t *testing.T) {
			body, err := search.Body(tt.q)
			if err != nil {
				t.Fatalf("Body(%+v) error = %v", tt.q, err)
			}
			got := decode(t, body)
			if _, ok := got["aggs"]; !ok {
				t.Fatalf("Body(%+v) has no aggs", tt.q)
			}
			delete(got, "aggs")
			if _, ok := got["post_filter"]; ok {
				t.Errorf("Body(%+v) has post_filter without filters", tt.q)
			}
			want := decode(t, []byte(tt.want))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Body(%+v) minus aggs = %v, want %v", tt.q, got, want)
			}
		})
	}
}

func TestBody_TopLevelKeyOrder(t *testing.T) {
	body, err := search.Body(catalog.Query{Page: 1, Size: 1, Brands: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"from", "size", "track_total_hits", "query", "post_filter", "aggs"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("top-level keys = %v, want %v", keys, want)
	}
}

type obj = map[string]any
type arr = []any

func strs(v ...string) arr {
	a := make(arr, len(v))
	for i, s := range v {
		a[i] = s
	}
	return a
}

func termsClause(field string, v []string) obj { return obj{"terms": obj{field: strs(v...)}} }

func topAgg(field string) obj {
	return obj{"terms": obj{
		"field": field, "size": float64(10), "min_doc_count": float64(1),
		"order": arr{obj{"_count": "desc"}, obj{"_key": "asc"}},
	}}
}

func filtersAgg(field string, v []string) obj {
	f := make(arr, len(v))
	for i, s := range v {
		f[i] = obj{"term": obj{field: s}}
	}
	return obj{"filters": obj{"filters": f}}
}

// TestBody_FilterStructure checks all 8 subsets of filter fields against an
// independently built expectation.
func TestBody_FilterStructure(t *testing.T) {
	names := []string{"brands", "categories", "nutriscore"}
	fields := []string{"brands.norm", "categories.norm", "nutriscore"}
	values := [][]string{{"zeta", "alpha"}, {"c1"}, {"a", "e"}}
	for mask := 0; mask < 8; mask++ {
		q := catalog.Query{Text: "x", Page: 1, Size: 10}
		sel := make([][]string, 3)
		if mask&1 != 0 {
			q.Brands, sel[0] = values[0], values[0]
		}
		if mask&2 != 0 {
			q.Categories, sel[1] = values[1], values[1]
		}
		if mask&4 != 0 {
			q.Nutriscore, sel[2] = values[2], values[2]
		}
		t.Run(fmt.Sprintf("subset_%03b", mask), func(t *testing.T) {
			body, err := search.Body(q)
			if err != nil {
				t.Fatal(err)
			}
			got := decode(t, body)

			other := func(skip int) obj {
				var cl arr
				for i := range names {
					if i != skip && sel[i] != nil {
						cl = append(cl, termsClause(fields[i], sel[i]))
					}
				}
				if cl == nil {
					return obj{"match_all": obj{}}
				}
				return obj{"bool": obj{"filter": cl}}
			}

			pf, hasPF := got["post_filter"]
			if hasPF != (mask != 0) {
				t.Fatalf("post_filter present = %v, want %v", hasPF, mask != 0)
			}
			if hasPF {
				if want := other(-1); !reflect.DeepEqual(pf, want) {
					t.Errorf("post_filter = %v, want %v", pf, want)
				}
			}

			aggs := got["aggs"].(obj)
			if len(aggs) != 3 {
				t.Errorf("aggs has %d keys, want 3", len(aggs))
			}
			for i, name := range names {
				var sub obj
				if name == "nutriscore" {
					sub = obj{"grades": filtersAgg("nutriscore", strings.Split(catalog.NutriscoreGrades, ""))}
				} else {
					sub = obj{"top": topAgg(fields[i])}
					if sel[i] != nil {
						sub["selected"] = filtersAgg(fields[i], sel[i])
					}
				}
				want := obj{"filter": other(i), "aggs": sub}
				if !reflect.DeepEqual(aggs[name], want) {
					t.Errorf("aggs.%s = %v, want %v", name, aggs[name], want)
				}
			}
			if containsKey(got, "include") {
				t.Error("body contains an include key")
			}
		})
	}
}

func containsKey(v any, key string) bool {
	switch x := v.(type) {
	case obj:
		for k, c := range x {
			if k == key || containsKey(c, key) {
				return true
			}
		}
	case arr:
		for _, c := range x {
			if containsKey(c, key) {
				return true
			}
		}
	}
	return false
}

func TestBody_SpecialCharactersRoundTrip(t *testing.T) {
	v := `a"b\c<d&e`
	q := catalog.Query{Page: 1, Size: 1, Brands: []string{v}, Categories: []string{v}}
	body, err := search.Body(q)
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, body)
	pf := got["post_filter"].(obj)["bool"].(obj)["filter"].(arr)
	for i, f := range []string{"brands.norm", "categories.norm"} {
		if want := termsClause(f, []string{v}); !reflect.DeepEqual(pf[i], want) {
			t.Errorf("post_filter[%d] = %v, want %v", i, pf[i], want)
		}
	}
	aggs := got["aggs"].(obj)
	for name, f := range map[string]string{"brands": "brands.norm", "categories": "categories.norm"} {
		sel := aggs[name].(obj)["aggs"].(obj)["selected"]
		if want := filtersAgg(f, []string{v}); !reflect.DeepEqual(sel, want) {
			t.Errorf("aggs.%s.selected = %v, want %v", name, sel, want)
		}
	}
}

func TestBody_DoesNotModifyQuery(t *testing.T) {
	q := catalog.Query{
		Text: "x", Page: 2, Size: 5,
		Brands:     []string{"b", "a"},
		Categories: []string{"c"},
		Nutriscore: []string{"e", "a"},
	}
	before := catalog.Query{
		Text: q.Text, Page: q.Page, Size: q.Size,
		Brands:     append([]string(nil), q.Brands...),
		Categories: append([]string(nil), q.Categories...),
		Nutriscore: append([]string(nil), q.Nutriscore...),
	}
	if _, err := search.Body(q); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(q, before) {
		t.Errorf("Body modified the query: %+v, want %+v", q, before)
	}
}
