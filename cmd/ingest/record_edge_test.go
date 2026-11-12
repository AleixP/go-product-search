package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestDecode_EmptyListsAreNil(t *testing.T) {
	got := recOK(t, recFields("1", "N", "", "fr:x", ", ,", "a"))

	if got.brands != nil || got.categories != nil || got.countries != nil {
		t.Errorf("brands = %#v, categories = %#v, countries = %#v; want nil lists (nothing allocated)",
			got.brands, got.categories, got.countries)
	}
}

// A list field is capped at 16 KiB, so a field made only of commas holds up to
// 16,384 empty items; the list capacity must follow the kept items, not the
// comma count (uncapped it would be 16 B per comma, 256 KiB).
// It reads MemStats.TotalAlloc, so it must not use t.Parallel.
func TestDecode_ListCapacityDoesNotFollowTheCommaCount(t *testing.T) {
	line := recFields("1", "N", strings.Repeat(",", maxListFieldBytes-2)+"a", "en:c", "en:us", "a")
	h := recStd(t)
	b := []byte(line)
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)
	got, reason := h.decode(b)
	runtime.ReadMemStats(&after)

	if reason != skipNone || len(got.brands) != 1 {
		t.Fatalf("decode = %+v, %v", got, reason)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<10 {
		t.Errorf("decode of a line with 16 KiB of commas and one brand allocated %d bytes, want <= 16 KiB", grown)
	}
}

func TestRecord_ProductKeepsOneListWhenTheOtherIsEmpty(t *testing.T) {
	onlyBrands := record{code: "1", name: "N", brands: []string{"B"}}.product()
	onlyCategories := record{code: "1", name: "N", categories: []string{"c"}}.product()

	recWantList(t, "brands of a product without categories", onlyBrands.Brands, []string{"B"})
	if onlyBrands.Categories == nil || len(onlyBrands.Categories) != 0 {
		t.Errorf("Categories = %#v, want a non-nil empty slice", onlyBrands.Categories)
	}
	recWantList(t, "categories of a product without brands", onlyCategories.Categories, []string{"c"})
	if onlyCategories.Brands == nil || len(onlyCategories.Brands) != 0 {
		t.Errorf("Brands = %#v, want a non-nil empty slice", onlyCategories.Brands)
	}
}
