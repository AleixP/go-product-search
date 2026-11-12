package opensearch

import (
	"encoding/json"
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

func TestMapping_Fields(t *testing.T) {
	props := loadMapping(t).Mappings.Properties

	t.Run("name", func(t *testing.T) {
		name := props["name"]
		if name["type"] != "text" || name["analyzer"] != "folded" {
			t.Errorf("name = %v, want text with analyzer folded", name)
		}
	})
	t.Run("name.autocomplete", func(t *testing.T) {
		ac := sub(t, props["name"], "fields", "autocomplete")
		if ac["type"] != "text" || ac["analyzer"] != "autocomplete" || ac["search_analyzer"] != "folded" {
			t.Errorf("name.autocomplete = %v, want text, analyzer autocomplete, search_analyzer folded", ac)
		}
	})
	for _, field := range []string{"brands", "categories"} {
		t.Run(field, func(t *testing.T) {
			p := props[field]
			if p["type"] != "keyword" || p["ignore_above"] != float64(256) {
				t.Errorf("%s = %v, want keyword with ignore_above 256", field, p)
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
