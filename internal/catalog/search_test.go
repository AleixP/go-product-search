package catalog_test

import (
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

func TestNutriscoreGrades_Value(t *testing.T) {
	if catalog.NutriscoreGrades != "abcde" {
		t.Errorf("NutriscoreGrades = %q, want %q", catalog.NutriscoreGrades, "abcde")
	}
}
