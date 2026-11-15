package opensearch

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// mappingDoc is the part of mapping.json the tests pin down.
type mappingDoc struct {
	Settings struct {
		Index map[string]any `json:"index"`
		Anlys struct {
			Filter   map[string]map[string]any `json:"filter"`
			Analyzer map[string]struct {
				Type      string   `json:"type"`
				Tokenizer string   `json:"tokenizer"`
				Filter    []string `json:"filter"`
			} `json:"analyzer"`
			Normalizer map[string]map[string]any `json:"normalizer"`
		} `json:"analysis"`
	} `json:"settings"`
	Mappings struct {
		Dynamic    string                    `json:"dynamic"`
		Properties map[string]map[string]any `json:"properties"`
	} `json:"mappings"`
}

func loadMapping(t *testing.T) mappingDoc {
	t.Helper()
	var m mappingDoc
	if err := json.Unmarshal(mappingJSON, &m); err != nil {
		t.Fatalf("mapping.json is not valid JSON: %v", err)
	}
	return m
}

// sub returns the sub-object at the given path of a decoded JSON object.
func sub(t *testing.T, obj map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := obj
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("missing object %q in path %v", p, path)
		}
		cur = next
	}
	return cur
}

func TestMapping_IndexSettings(t *testing.T) {
	idx := loadMapping(t).Settings.Index
	for key, want := range map[string]any{
		"number_of_shards":   float64(1),
		"number_of_replicas": float64(0),
		"refresh_interval":   "-1",
	} {
		t.Run(key, func(t *testing.T) {
			if got := idx[key]; got != want {
				t.Errorf("%s = %v, want %v", key, got, want)
			}
		})
	}
}

func TestMapping_Analysis(t *testing.T) {
	a := loadMapping(t).Settings.Anlys

	t.Run("filter autocomplete_edge", func(t *testing.T) {
		want := map[string]any{"type": "edge_ngram", "min_gram": float64(2), "max_gram": float64(15)}
		if got := a.Filter["autocomplete_edge"]; !reflect.DeepEqual(got, want) {
			t.Errorf("autocomplete_edge = %v, want %v", got, want)
		}
	})
	t.Run("filter autocomplete_min_length", func(t *testing.T) {
		want := map[string]any{"type": "length", "min": float64(2)}
		if got := a.Filter["autocomplete_min_length"]; !reflect.DeepEqual(got, want) {
			t.Errorf("autocomplete_min_length = %v, want %v", got, want)
		}
	})
	t.Run("filter autocomplete_truncate", func(t *testing.T) {
		want := map[string]any{"type": "truncate", "length": float64(15)}
		if got := a.Filter["autocomplete_truncate"]; !reflect.DeepEqual(got, want) {
			t.Errorf("autocomplete_truncate = %v, want %v", got, want)
		}
	})
	t.Run("filter names", func(t *testing.T) {
		got := slices.Sorted(maps.Keys(a.Filter))
		if want := []string{"autocomplete_edge", "autocomplete_min_length", "autocomplete_truncate"}; !slices.Equal(got, want) {
			t.Errorf("filters = %v, want exactly %v", got, want)
		}
	})
	t.Run("analyzer autocomplete_search", func(t *testing.T) {
		// Generic map so that an extra key (char_filter, ...) is caught; the order of
		// the filters matters: the length and truncate filters run on folded tokens.
		var raw struct {
			Settings struct {
				Analysis struct {
					Analyzer map[string]map[string]any `json:"analyzer"`
				} `json:"analysis"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(mappingJSON, &raw); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"type": "custom", "tokenizer": "standard",
			"filter": []any{"lowercase", "asciifolding", "autocomplete_min_length", "autocomplete_truncate"},
		}
		if got := raw.Settings.Analysis.Analyzer["autocomplete_search"]; !reflect.DeepEqual(got, want) {
			t.Errorf("autocomplete_search = %v, want %v", got, want)
		}
		got := slices.Sorted(maps.Keys(raw.Settings.Analysis.Analyzer))
		if want := []string{"autocomplete", "autocomplete_search", "folded"}; !slices.Equal(got, want) {
			t.Errorf("analyzers = %v, want exactly %v", got, want)
		}
	})
	for name, wantFilters := range map[string][]string{
		"folded":       {"lowercase", "asciifolding"},
		"autocomplete": {"lowercase", "asciifolding", "autocomplete_edge"},
	} {
		t.Run("analyzer "+name, func(t *testing.T) {
			got := a.Analyzer[name]
			if got.Type != "custom" || got.Tokenizer != "standard" || !reflect.DeepEqual(got.Filter, wantFilters) {
				t.Errorf("analyzer %s = %+v, want custom/standard/%v", name, got, wantFilters)
			}
		})
	}
}

// TestMapping_AutocompleteCoupling pins the contract between the index-time
// edge n-grams and the query-time analyzer: a query token shorter than min_gram
// or longer than max_gram could never match an indexed gram.
func TestMapping_AutocompleteCoupling(t *testing.T) {
	f := loadMapping(t).Settings.Anlys.Filter
	edge, minLen, trunc := f["autocomplete_edge"], f["autocomplete_min_length"], f["autocomplete_truncate"]
	if minLen["min"] != edge["min_gram"] {
		t.Errorf("autocomplete_min_length.min = %v, want autocomplete_edge.min_gram %v", minLen["min"], edge["min_gram"])
	}
	if trunc["length"] != edge["max_gram"] {
		t.Errorf("autocomplete_truncate.length = %v, want autocomplete_edge.max_gram %v", trunc["length"], edge["max_gram"])
	}
	if edge["min_gram"] == nil || edge["max_gram"] == nil {
		t.Errorf("autocomplete_edge = %v, want min_gram and max_gram set", edge)
	}
}

func TestMapping_Normalizer(t *testing.T) {
	n := loadMapping(t).Settings.Anlys.Normalizer
	if len(n) != 1 {
		t.Fatalf("normalizers = %v, want only folded_keyword", n)
	}
	got, ok := n["folded_keyword"]
	if !ok {
		t.Fatalf("normalizer folded_keyword is not defined: %v", n)
	}
	// Decoded as a generic map so that an extra key (tokenizer, description, ...) is caught.
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, []string{"char_filter", "filter", "type"}) {
		t.Errorf("normalizer keys = %v, want exactly [char_filter filter type]", keys)
	}
	if got["type"] != "custom" {
		t.Errorf("type = %v, want custom", got["type"])
	}
	// An empty, non-nil char_filter is part of the pinned definition.
	if cf, ok := got["char_filter"].([]any); !ok || cf == nil || len(cf) != 0 {
		t.Errorf("char_filter = %#v, want an empty list", got["char_filter"])
	}
	want := []any{"lowercase", "asciifolding"}
	if !reflect.DeepEqual(got["filter"], want) {
		t.Errorf("filter = %v, want %v (same order as the folded analyzer)", got["filter"], want)
	}
}

// TestMapping_TopLevelKeys pins the document to exactly settings and mappings.
// CreateIndex sends mapping.json verbatim, so a top-level "aliases" key would
// attach the new index to the live "products" alias before the swap.
func TestMapping_TopLevelKeys(t *testing.T) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(mappingJSON, &doc); err != nil {
		t.Fatalf("mapping.json is not valid JSON: %v", err)
	}
	if got := slices.Sorted(maps.Keys(doc)); !slices.Equal(got, []string{"mappings", "settings"}) {
		t.Errorf("top-level keys = %v, want exactly [mappings settings]", got)
	}
}

func TestMapping_Fields(t *testing.T) {
	props := loadMapping(t).Mappings.Properties

	t.Run("name", func(t *testing.T) {
		name := props["name"]
		if got := slices.Sorted(maps.Keys(name)); !slices.Equal(got, []string{"analyzer", "fields", "type"}) {
			t.Errorf("name keys = %v, want exactly [analyzer fields type]", got)
		}
		if name["type"] != "text" || name["analyzer"] != "folded" {
			t.Errorf("name = %v, want text with analyzer folded", name)
		}
	})
	t.Run("name fields", func(t *testing.T) {
		got := slices.Sorted(maps.Keys(sub(t, props["name"], "fields")))
		if want := []string{"autocomplete", "norm"}; !slices.Equal(got, want) {
			t.Errorf("name fields = %v, want exactly %v", got, want)
		}
	})
	t.Run("name.autocomplete", func(t *testing.T) {
		ac := sub(t, props["name"], "fields", "autocomplete")
		want := map[string]any{"type": "text", "analyzer": "autocomplete", "search_analyzer": "autocomplete_search"}
		if !reflect.DeepEqual(ac, want) {
			t.Errorf("name.autocomplete = %v, want %v", ac, want)
		}
	})
	t.Run("name.norm", func(t *testing.T) {
		norm := sub(t, props["name"], "fields", "norm")
		want := map[string]any{"type": "keyword", "normalizer": "folded_keyword", "ignore_above": float64(1024)}
		if !reflect.DeepEqual(norm, want) {
			t.Errorf("name.norm = %v, want %v", norm, want)
		}
	})
	for _, field := range []string{"brands", "categories"} {
		t.Run(field, func(t *testing.T) {
			p := props[field]
			if p["type"] != "keyword" || p["ignore_above"] != float64(256) {
				t.Errorf("%s = %v, want keyword with ignore_above 256", field, p)
			}
		})
		t.Run(field+" keys", func(t *testing.T) {
			got := slices.Sorted(maps.Keys(props[field]))
			if want := []string{"fields", "ignore_above", "type"}; !slices.Equal(got, want) {
				t.Errorf("%s keys = %v, want exactly %v", field, got, want)
			}
		})
		t.Run(field+" fields", func(t *testing.T) {
			got := slices.Sorted(maps.Keys(sub(t, props[field], "fields")))
			if want := []string{"norm", "text"}; !slices.Equal(got, want) {
				t.Errorf("%s fields = %v, want %v", field, got, want)
			}
		})
		t.Run(field+".norm", func(t *testing.T) {
			norm := sub(t, props[field], "fields", "norm")
			want := map[string]any{"type": "keyword", "normalizer": "folded_keyword", "ignore_above": float64(256)}
			if !reflect.DeepEqual(norm, want) {
				t.Errorf("%s.norm = %v, want %v", field, norm, want)
			}
		})
		t.Run(field+".text", func(t *testing.T) {
			txt := sub(t, props[field], "fields", "text")
			if txt["type"] != "text" || txt["analyzer"] != "folded" {
				t.Errorf("%s.text = %v, want text with analyzer folded", field, txt)
			}
		})
	}

	t.Run("nutriscore", func(t *testing.T) {
		p := props["nutriscore"]
		if got := slices.Sorted(maps.Keys(p)); !slices.Equal(got, []string{"type"}) {
			t.Errorf("nutriscore keys = %v, want exactly [type]", got)
		}
		if _, has := p["fields"]; has {
			t.Errorf("nutriscore = %v, want no fields", p)
		}
		if _, has := p["ignore_above"]; p["type"] != "keyword" || has {
			t.Errorf("nutriscore = %v, want keyword without ignore_above", p)
		}
	})
}

func TestMapping_IsStrictAndMatchesProduct(t *testing.T) {
	m := loadMapping(t)
	if m.Mappings.Dynamic != "strict" {
		t.Errorf("dynamic = %q, want strict", m.Mappings.Dynamic)
	}

	var want []string
	for f := range reflect.TypeFor[catalog.Product]().Fields() {
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			want = append(want, tag)
		}
	}
	slices.Sort(want)
	got := slices.Sorted(maps.Keys(m.Mappings.Properties))
	if !slices.Equal(got, want) {
		t.Errorf("mapping properties = %v, want Product JSON tags %v", got, want)
	}
}

// diffJSON returns the paths where two decoded JSON values differ.
func diffJSON(path string, got, want any) []string {
	gm, gok := got.(map[string]any)
	wm, wok := want.(map[string]any)
	if !gok || !wok {
		if reflect.DeepEqual(got, want) {
			return nil
		}
		return []string{fmt.Sprintf("%s: got %v, want %v", path, got, want)}
	}
	var out []string
	for k, w := range wm {
		g, ok := gm[k]
		if !ok {
			out = append(out, fmt.Sprintf("%s.%s: missing, want %v", path, k, w))
			continue
		}
		out = append(out, diffJSON(path+"."+k, g, w)...)
	}
	for k, g := range gm {
		if _, ok := wm[k]; !ok {
			out = append(out, fmt.Sprintf("%s.%s: unexpected %v", path, k, g))
		}
	}
	slices.Sort(out)
	return out
}

// TestMapping_GoldenDocument is a deliberate golden pin of the whole index
// definition: every analyzer, filter, normalizer, setting and field. Any
// intended mapping change must update this literal in the same commit, which
// is the point: a mapping change forces a re-ingest, so it must be conscious.
func TestMapping_GoldenDocument(t *testing.T) {
	text := func(analyzer string) map[string]any {
		return map[string]any{"type": "text", "analyzer": analyzer}
	}
	subfields := map[string]any{
		"text": text("folded"),
		"norm": map[string]any{"type": "keyword", "normalizer": "folded_keyword", "ignore_above": float64(256)},
	}
	want := map[string]any{
		"settings": map[string]any{
			"index": map[string]any{
				"number_of_shards":   float64(1),
				"number_of_replicas": float64(0),
				"refresh_interval":   "-1",
			},
			"analysis": map[string]any{
				"filter": map[string]any{
					"autocomplete_edge":       map[string]any{"type": "edge_ngram", "min_gram": float64(2), "max_gram": float64(15)},
					"autocomplete_min_length": map[string]any{"type": "length", "min": float64(2)},
					"autocomplete_truncate":   map[string]any{"type": "truncate", "length": float64(15)},
				},
				"analyzer": map[string]any{
					"folded": map[string]any{
						"type": "custom", "tokenizer": "standard",
						"filter": []any{"lowercase", "asciifolding"},
					},
					"autocomplete": map[string]any{
						"type": "custom", "tokenizer": "standard",
						"filter": []any{"lowercase", "asciifolding", "autocomplete_edge"},
					},
					"autocomplete_search": map[string]any{
						"type": "custom", "tokenizer": "standard",
						"filter": []any{"lowercase", "asciifolding", "autocomplete_min_length", "autocomplete_truncate"},
					},
				},
				"normalizer": map[string]any{
					"folded_keyword": map[string]any{
						"type": "custom", "char_filter": []any{},
						"filter": []any{"lowercase", "asciifolding"},
					},
				},
			},
		},
		"mappings": map[string]any{
			"dynamic": "strict",
			"properties": map[string]any{
				"name": map[string]any{
					"type": "text", "analyzer": "folded",
					"fields": map[string]any{
						"autocomplete": map[string]any{"type": "text", "analyzer": "autocomplete", "search_analyzer": "autocomplete_search"},
						"norm":         map[string]any{"type": "keyword", "normalizer": "folded_keyword", "ignore_above": float64(1024)},
					},
				},
				"brands": map[string]any{
					"type": "keyword", "ignore_above": float64(256), "fields": subfields,
				},
				"categories": map[string]any{
					"type": "keyword", "ignore_above": float64(256), "fields": subfields,
				},
				"nutriscore": map[string]any{"type": "keyword"},
			},
		},
	}

	var got map[string]any
	if err := json.Unmarshal(mappingJSON, &got); err != nil {
		t.Fatalf("mapping.json is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapping.json differs from the golden document:\n%s", strings.Join(diffJSON("$", got, want), "\n"))
	}
}
