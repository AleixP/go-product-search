package catalog_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

func TestProduct_MarshalJSON_ExactKeysWithoutID(t *testing.T) {
	p := catalog.Product{
		ID:         "3017620422003",
		Name:       "Nutella",
		Brands:     []string{"Ferrero"},
		Categories: []string{"spreads"},
		Nutriscore: "e",
	}

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", raw, err)
	}

	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"brands", "categories", "name", "nutriscore"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("json keys = %v, want %v (body %s)", keys, want, raw)
	}
}

func TestProduct_MarshalJSON_KeysPresentWhenEmpty(t *testing.T) {
	// A zero-value product has nil slices and empty strings; the body must
	// still carry all four keys, so no field may use omitempty.
	raw, err := json.Marshal(catalog.Product{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", raw, err)
	}

	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"brands", "categories", "name", "nutriscore"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("json keys = %v, want %v (body %s)", keys, want, raw)
	}
}
