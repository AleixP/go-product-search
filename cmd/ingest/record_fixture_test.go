package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// recFixtureRow is the expected outcome of one data row of
// testdata/products.tsv, in file order.
type recFixtureRow struct {
	code       string
	skip       skipReason
	name       string
	brands     []string
	categories []string
	nutriscore string
	countries  []string
}

// The rows are real Open Food Facts rows trimmed to a few columns, except the
// three synthetic ones (codes 9990000000001, 9990000000002 and the empty one).
// The url column of the real rows was rebuilt as
// https://world.openfoodfacts.org/product/<code>, which differs from the
// official https://world-en.openfoodfacts.org/product/<code>; the other values
// are the real ones.
var recFixtureWant = []recFixtureRow{
	{code: "0046872021545", name: "Smoked Mussels In Cottonseed Oil", brands: []string{"Family"}, categories: []string{"undefined"}, nutriscore: "a", countries: []string{"en:united-states"}},
	// Synthetic: the name starts with a lone quote; the next row must still decode.
	{code: "9990000000001", name: `"Lone quote product`, brands: []string{"Synthetic Brand"}, categories: []string{"snacks"}, nutriscore: "c", countries: []string{"en:united-states"}},
	{code: "0862418000269", name: "Rawpothecary, Probiotic Water", brands: []string{"Rawpothecary Inc."}, categories: []string{"beverages"}, nutriscore: "b", countries: []string{"en:united-states"}},
	{code: "0026395783168", name: "Organic aloe water beverage", brands: []string{"Lodc Inc."}, categories: []string{"plant-based-foods-and-beverages", "beverages", "plant-based-beverages"}, nutriscore: "c", countries: []string{"en:united-states"}},
	{code: "0041757001803", name: "Light snack cheese count", brands: []string{"Bel Brands Usa"}, categories: []string{"dairies", "fermented-foods", "fermented-milk-products", "cheeses"}, nutriscore: "d", countries: []string{"en:united-states"}},
	{code: "0099482439903", name: "Whole milk", brands: []string{"365 Everyday Value"}, categories: []string{"dairies", "milks"}, nutriscore: "e", countries: []string{"en:united-states"}},
	// Two brands separated by a comma and a double space.
	{code: "0011822442411", name: "Apricots", brands: []string{"Big Win", "Rite Aid Corporation"}, categories: []string{"undefined"}, nutriscore: "d", countries: []string{"en:united-states"}},
	// Synthetic: 7 fields instead of 8.
	{code: "", skip: skipMalformed},
	// Grade "unknown" and no categories at all.
	{code: "0028400017824", name: "Onion flavored rings", brands: []string{"Frito-Lay Company"}, nutriscore: "", countries: []string{"en:united-states"}},
	// Grade "not-applicable", another single country.
	{code: "0748927023008", name: "Serious Mass", brands: []string{"Optimum Nutrition"}, categories: []string{"dietary-supplements", "bodybuilding-supplements"}, nutriscore: "", countries: []string{"en:france"}},
	// Synthetic: empty code.
	{code: "", skip: skipInvalidCode},
	// Non-ASCII name, Canada, grade "unknown", no categories.
	{code: "0055742502008", name: "Thon pâle émietté", brands: []string{"Compliments"}, nutriscore: "", countries: []string{"en:canada"}},
	// Mixed en: and fr: categories (the fr: one is dropped), 8-digit code with zeros.
	{code: "00895743", name: "Minestrone Soup With Bacon", brands: []string{"Marks & Spencer"}, categories: []string{"meals", "soups", "refrigerated-foods", "microwave-meals", "minestrone-soups", "refrigerated-meals", "refrigerated-soups"}, nutriscore: "b", countries: []string{"en:france"}},
	// Several countries.
	{code: "0016000502642", name: "Nature Valley Crunchy Oats 'n Honey", brands: []string{"Nature Valley"}, categories: []string{"snacks", "sweet-snacks", "bars", "cereal-bars"}, nutriscore: "d", countries: []string{"en:france", "en:germany", "en:switzerland", "en:united-kingdom"}},
	// Only non-en categories, two countries, two brands.
	{code: "0060822800028", name: "Mexican Veggie Ground Round", brands: []string{"The Hain Celestial Group  Inc.", "Yves Veggie Cuisine"}, nutriscore: "a", countries: []string{"en:united-states", "en:world"}},
}

// recFixtureLines reads every line of the fixture with the real line reader.
func recFixtureLines(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("../../testdata/products.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lr := newLineReader(f, 0)
	var lines [][]byte
	for {
		line, tooLong, err := lr.next()
		if err != nil {
			break
		}
		if tooLong {
			t.Fatal("fixture line too long")
		}
		lines = append(lines, slices.Clone(line)) // the reader reuses its buffer
	}
	return lines
}

func TestFixture_EveryRowDecodesAsExpected(t *testing.T) {
	lines := recFixtureLines(t)
	if len(lines) != len(recFixtureWant)+1 {
		t.Fatalf("fixture has %d lines, want a header and %d rows", len(lines), len(recFixtureWant))
	}
	h, err := parseHeader(lines[0])
	if err != nil {
		t.Fatalf("parseHeader(fixture header) error = %v", err)
	}
	// The fixture header is not in the real export order.
	if h.cols == [numCols]int{0, 10, 18, 22, 40, 58} {
		t.Error("fixture header has the real column positions; it must be reordered to prove lookup by name")
	}
	if h.width != 8 {
		t.Errorf("fixture header width = %d, want 8 (6 required + 2 unrelated)", h.width)
	}

	for i, want := range recFixtureWant {
		got, reason := h.decode(lines[i+1])

		if reason != want.skip {
			t.Errorf("row %d (%q): reason = %v, want %v", i+1, want.code, reason, want.skip)
			continue
		}
		if want.skip != skipNone {
			continue
		}
		if got.code != want.code || got.name != want.name || got.nutriscore != want.nutriscore {
			t.Errorf("row %d: code, name, nutriscore = %q, %q, %q; want %q, %q, %q",
				i+1, got.code, got.name, got.nutriscore, want.code, want.name, want.nutriscore)
		}
		recWantList(t, "row "+want.code+" brands", got.brands, want.brands)
		recWantList(t, "row "+want.code+" categories", got.categories, want.categories)
		recWantList(t, "row "+want.code+" countries", got.countries, want.countries)
	}
}

func TestFixture_CountsPerSkipReason(t *testing.T) {
	lines := recFixtureLines(t)
	h, err := parseHeader(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	counts := map[skipReason]int{}

	for _, l := range lines[1:] {
		_, reason := h.decode(l)
		counts[reason]++
	}

	want := map[skipReason]int{skipNone: 13, skipMalformed: 1, skipInvalidCode: 1}
	for _, r := range []skipReason{skipNone, skipMalformed, skipInvalidCode, skipNoName} {
		if counts[r] != want[r] {
			t.Errorf("rows with reason %v = %d, want %d", r, counts[r], want[r])
		}
	}
}

func TestFixture_LoneQuoteRowAndTheNextOneDecodeOnTheirOwn(t *testing.T) {
	lines := recFixtureLines(t)
	h, err := parseHeader(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	var idx int
	for i, l := range lines {
		if strings.Contains(string(l), "\t\"Lone quote") {
			idx = i
		}
	}
	if idx == 0 || idx+1 >= len(lines) {
		t.Fatal("lone quote row not found in the fixture")
	}

	quoted, r1 := h.decode(lines[idx])
	next, r2 := h.decode(lines[idx+1])

	if r1 != skipNone || quoted.name != `"Lone quote product` {
		t.Errorf("lone quote row = %+v, %v", quoted, r1)
	}
	if r2 != skipNone || next.code != "0862418000269" {
		t.Errorf("row after the lone quote = %+v, %v; want code 0862418000269", next, r2)
	}
}

func TestFixture_HasNoCarriageReturnAndEndsWithANewline(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/products.tsv")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\r") || !strings.HasSuffix(string(raw), "\n") {
		t.Error("fixture must use \\n line ends and end with a newline")
	}
}

func TestFixture_LicenseNoticeNamesTheSourceAndTheLicences(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(c) Open Food Facts contributors",
		"https://opendatacommons.org/licenses/odbl/1-0/",
		"https://opendatacommons.org/licenses/dbcl/1-0/",
		"https://world.openfoodfacts.org/terms-of-use",
		"https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz",
		"separate from the licence of the project code",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("testdata/LICENSE lacks %q", want)
		}
	}
}
