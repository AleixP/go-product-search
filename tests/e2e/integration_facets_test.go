package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Expectations of the filter and facet subtests, derived by hand from
// testdata/integration.tsv (25 indexed documents; ids are the last three digits
// of the code). Facet values are folded keys. A facet lists the top 10 by count
// descending then key ascending, plus the selected values missing from the top
// 10 (count 0 when nothing matches); the facet of a field ignores the selection
// on that same field and applies the other two (disjunctive facets).
//
// Counts over all 25 documents:
//
//	brands (13 keys; Acme Foods, ACME FOODS and Acmé Foods fold to one):
//	  green valley 4; acme foods 3, crunch mill 3, sweet hearth 3;
//	  aqua peak 2, cocoa crest 2, nonna pantry 2, olive grove 2;
//	  cold peak 1, fizz co 1, leaf house 1, nordic farms 1, zenith bakery 1
//	  The top 10 stops after fizz co: the tie at count 1 is cut by key.
//	categories (12 keys): snacks 10, beverages 8, dairies 7, plant-based-foods 6,
//	  cheeses 5, chocolates 4, biscuits 3, breakfast-cereals 3, spreads 3,
//	  sauces 2, soups 2, frozen-desserts 1
//	  The top 10 stops after sauces: the tie at count 2 is cut by key.
//	grades: a 2, b 3, c 5, d 7, e 8
var (
	allBrands = []facetPair{
		{"green valley", 4}, {"acme foods", 3}, {"crunch mill", 3}, {"sweet hearth", 3},
		{"aqua peak", 2}, {"cocoa crest", 2}, {"nonna pantry", 2}, {"olive grove", 2},
		{"cold peak", 1}, {"fizz co", 1},
	}
	allCategories = []facetPair{
		{"snacks", 10}, {"beverages", 8}, {"dairies", 7}, {"plant-based-foods", 6},
		{"cheeses", 5}, {"chocolates", 4}, {"biscuits", 3}, {"breakfast-cereals", 3},
		{"spreads", 3}, {"sauces", 2},
	}
	allGrades = []facetPair{{"a", 2}, {"b", 3}, {"c", 5}, {"d", 7}, {"e", 8}}
)

// withExtra returns the facet list plus the selected values that fall outside
// the top 10, in the order the API sorts them (they all sort last here).
func withExtra(list []facetPair, extra ...facetPair) []facetPair {
	return append(slices.Clone(list), extra...)
}

// facetCase is one /search request and its full expectation: total, the set of
// ids (never an order) and the three facet lists, exactly and in order.
type facetCase struct {
	name       string
	query      url.Values
	ids        []string // last three digits of the codes
	brands     []facetPair
	categories []facetPair
	grades     []facetPair
}

func vals(kv ...string) url.Values {
	v := url.Values{"size": {"100"}}
	for i := 0; i < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

var acmeCase = facetCase{
	ids: []string{"013", "014", "015"}, // 013 Acme Foods, 014 ACME FOODS, 015 Acmé Foods
	// brands facet ignores the brands filter: all 25 documents
	brands:     allBrands,
	categories: []facetPair{{"dairies", 3}, {"cheeses", 2}, {"beverages", 1}}, // 013 d+c, 014 d+b, 015 d+c
	grades:     []facetPair{{"a", 0}, {"b", 1}, {"c", 1}, {"d", 0}, {"e", 1}},
}

var facetCases = []facetCase{
	{
		name: "no filters", query: vals(),
		ids: []string{"001", "002", "003", "004", "005", "006", "007", "008", "009", "010", "011", "012", "013",
			"014", "015", "016", "017", "018", "019", "020", "021", "022", "023", "024", "025"},
		brands: allBrands, categories: allCategories, grades: allGrades,
	},
	{
		name: "brands one value", query: vals("brands", "Green Valley"),
		ids:    []string{"005", "007", "012", "019"},
		brands: allBrands,
		// 005 dairies cheeses snacks; 007 soups; 012 dairies cheeses beverages breakfast-cereals; 019 dairies beverages
		categories: []facetPair{{"dairies", 3}, {"beverages", 2}, {"cheeses", 2}, {"breakfast-cereals", 1}, {"snacks", 1}, {"soups", 1}},
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 1}, {"d", 3}, {"e", 0}}, // 005 c; 007, 012, 019 d
	},
	{
		// zenith bakery has count 1 and sits outside the top 10 of the brands facet
		name: "brands two values are an OR and a selected value outside the top 10 is listed", query: vals("brands", "Cocoa Crest", "brands", "Zenith Bakery"),
		ids:        []string{"001", "002", "006"},
		brands:     withExtra(allBrands, facetPair{"zenith bakery", 1}),
		categories: []facetPair{{"snacks", 3}, {"chocolates", 2}, {"biscuits", 1}, {"cheeses", 1}}, // 001, 002 chocolates snacks; 006 snacks biscuits cheeses
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 1}, {"d", 1}, {"e", 1}},                  // 001 c, 006 d, 002 e
	},
	{
		// raw order is Z before n (upper case sorts first), folded order is nordic before zenith:
		// both values have count 1 and sit outside the top 10, so their order is decided by the
		// final stable sort in termsFacetValues (internal/opensearch/search/facets.go): count
		// descending, then Value ascending. Input order, including normalize's sort, is irrelevant
		name: "two selected values outside the top 10 are ordered by folded key", query: vals("brands", "ZENITH BAKERY", "brands", "nordic farms"),
		ids:        []string{"006", "007"},
		brands:     withExtra(allBrands, facetPair{"nordic farms", 1}, facetPair{"zenith bakery", 1}),
		categories: []facetPair{{"biscuits", 1}, {"cheeses", 1}, {"snacks", 1}, {"soups", 1}}, // 006 snacks biscuits cheeses; 007 soups
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 0}, {"d", 2}, {"e", 0}},             // 006 d, 007 d
	},
	withName(acmeCase, "brands in the folded spelling", vals("brands", "acme foods")),
	withName(acmeCase, "brands in the stored spelling", vals("brands", "Acme Foods")),
	withName(acmeCase, "brands in a raw upper case and accent spelling", vals("brands", "ACMÉ FOODS")),
	// Known limit (accepted): the categories.norm normalizer is not observable with this
	// fixture, which has only lowercase ASCII category tags and ingest does not lowercase them.
	{
		name: "categories one value", query: vals("categories", "cheeses"),
		ids: []string{"005", "006", "012", "013", "015"},
		// 005 and 012 green valley, 006 zenith bakery, 013 and 015 acme foods
		brands:     []facetPair{{"acme foods", 2}, {"green valley", 2}, {"zenith bakery", 1}},
		categories: allCategories,
		grades:     []facetPair{{"a", 0}, {"b", 1}, {"c", 1}, {"d", 2}, {"e", 1}}, // 013 b; 005 c; 006, 012 d; 015 e
	},
	{
		name: "categories in a raw upper case spelling", query: vals("categories", "CHEESES"),
		ids:        []string{"005", "006", "012", "013", "015"},
		brands:     []facetPair{{"acme foods", 2}, {"green valley", 2}, {"zenith bakery", 1}},
		categories: allCategories,
		grades:     []facetPair{{"a", 0}, {"b", 1}, {"c", 1}, {"d", 2}, {"e", 1}},
	},
	{
		// soups ties with sauces at count 2 and loses the tie at place 10 (key asc)
		name: "categories value cut by the tie-break at place 10 is listed with its count", query: vals("categories", "soups"),
		ids:        []string{"007", "023"},
		brands:     []facetPair{{"green valley", 1}, {"nonna pantry", 1}, {"nordic farms", 1}}, // 007 nordic farms + green valley; 023 nonna pantry
		categories: withExtra(allCategories, facetPair{"soups", 2}),
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 0}, {"d", 1}, {"e", 1}}, // 007 d, 023 e
	},
	{
		name: "categories value with the lowest count is listed with its count", query: vals("categories", "frozen-desserts"),
		ids:        []string{"025"},
		brands:     []facetPair{{"cold peak", 1}},
		categories: withExtra(allCategories, facetPair{"frozen-desserts", 1}),
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 0}, {"d", 0}, {"e", 1}},
	},
	{
		name: "nutriscore one value keeps all grades in its own facet", query: vals("nutriscore", "a"),
		ids:        []string{"009", "018"},
		brands:     []facetPair{{"leaf house", 1}, {"olive grove", 1}}, // 009, 018
		categories: []facetPair{{"beverages", 2}, {"plant-based-foods", 2}},
		grades:     allGrades,
	},
	{
		name: "nutriscore two values are an OR", query: vals("nutriscore", "a", "nutriscore", "b"),
		ids: []string{"008", "009", "013", "017", "018"},
		// 008 olive grove, 009 leaf house, 013 acme foods, 017 aqua peak, 018 olive grove
		brands: []facetPair{{"olive grove", 2}, {"acme foods", 1}, {"aqua peak", 1}, {"leaf house", 1}},
		// 008 spreads plant snacks; 009, 017, 018 beverages plant; 013 dairies cheeses
		categories: []facetPair{{"plant-based-foods", 4}, {"beverages", 3}, {"cheeses", 1}, {"dairies", 1}, {"snacks", 1}, {"spreads", 1}},
		grades:     allGrades,
	},
	{
		// brands is applied to the nutriscore facet and nutriscore to the brands facet
		name: "brands and nutriscore are an AND", query: vals("brands", "Green Valley", "nutriscore", "d"),
		ids: []string{"007", "012", "019"},
		// documents graded d: 003 sweet hearth, 006 zenith bakery, 007 nordic farms + green valley,
		// 011 crunch mill, 012 and 019 green valley, 022 nonna pantry
		brands:     []facetPair{{"green valley", 3}, {"crunch mill", 1}, {"nonna pantry", 1}, {"nordic farms", 1}, {"sweet hearth", 1}, {"zenith bakery", 1}},
		categories: []facetPair{{"beverages", 2}, {"dairies", 2}, {"breakfast-cereals", 1}, {"cheeses", 1}, {"soups", 1}}, // 007, 012, 019
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 1}, {"d", 3}, {"e", 0}},                                         // green valley: 005 c; 007, 012, 019 d
	},
	{
		// zenith bakery (006) has no soups: the AND is empty, and each selected
		// value is listed with count 0 inside the facet of the other field
		name: "an AND with no match lists both selected values with count 0", query: vals("brands", "Zenith Bakery", "categories", "soups"),
		ids:        nil,
		brands:     []facetPair{{"green valley", 1}, {"nonna pantry", 1}, {"nordic farms", 1}, {"zenith bakery", 0}}, // soups: 007, 023
		categories: []facetPair{{"biscuits", 1}, {"cheeses", 1}, {"snacks", 1}, {"soups", 0}},                        // zenith bakery: 006
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 0}, {"d", 0}, {"e", 0}},
	},
	{
		name: "a brand that matches nothing gives 200, total 0 and the value with count 0", query: vals("brands", "Nonexistent Brand"),
		ids:        nil,
		brands:     withExtra(allBrands, facetPair{"nonexistent brand", 0}),
		categories: nil,
		grades:     []facetPair{{"a", 0}, {"b", 0}, {"c", 0}, {"d", 0}, {"e", 0}},
	},
}

// withName copies a case with another name and query.
func withName(c facetCase, name string, q url.Values) facetCase {
	c.name, c.query = name, q
	return c
}

// roundTrip lists every value of the three facets with its count over all 25
// documents, including the 3 brands and 2 categories the top 10 cuts off.
var roundTrip = []struct {
	field string
	count map[string]int
}{
	{"brands", map[string]int{
		"green valley": 4, "acme foods": 3, "crunch mill": 3, "sweet hearth": 3, "aqua peak": 2, "cocoa crest": 2,
		"nonna pantry": 2, "olive grove": 2, "cold peak": 1, "fizz co": 1, "leaf house": 1, "nordic farms": 1, "zenith bakery": 1,
	}},
	{"categories", map[string]int{
		"snacks": 10, "beverages": 8, "dairies": 7, "plant-based-foods": 6, "cheeses": 5, "chocolates": 4,
		"biscuits": 3, "breakfast-cereals": 3, "spreads": 3, "sauces": 2, "soups": 2, "frozen-desserts": 1,
	}},
	{"nutriscore", map[string]int{"a": 2, "b": 3, "c": 5, "d": 7, "e": 8}},
}

// facetsIntegrationSubtests runs the filter and facet cases against the real engine.
func facetsIntegrationSubtests(t *testing.T, client *http.Client, base string) {
	t.Run("filters and facets", func(t *testing.T) {
		for _, tc := range facetCases {
			t.Run(tc.name, func(t *testing.T) {
				r := getSearch(t, client, base, tc.query.Encode())
				want := docIDs(tc.ids...)
				slices.Sort(want)
				got := itemIDs(r.Items)
				slices.Sort(got)
				if r.Total != len(want) || !slices.Equal(got, want) {
					t.Errorf("%s: total %d, ids %v; want total %d, ids %v", tc.query.Encode(), r.Total, got, len(want), want)
				}
				wantFacets(t, r, tc.brands, tc.categories, tc.grades)
			})
		}

		t.Run("a raw spelling gives the same body as the folded key", func(t *testing.T) {
			folded := getSearch(t, client, base, vals("brands", "acme foods").Encode()).body
			for _, raw := range []string{"ACMÉ FOODS", "Acmé Foods", "aCmE fOoDs"} {
				if got := getSearch(t, client, base, vals("brands", raw).Encode()).body; got != folded {
					t.Errorf("brands=%q body differs from brands=acme foods:\n%s\n%s", raw, got, folded)
				}
			}
		})

		t.Run("every facet value filters to exactly its count", func(t *testing.T) {
			for _, f := range roundTrip {
				for value, n := range f.count {
					r := getSearch(t, client, base, vals(f.field, value).Encode())
					if r.Total != n || len(r.Items) != n {
						t.Errorf("%s=%q: total %d, items %d; want %d", f.field, value, r.Total, len(r.Items), n)
					}
				}
			}
		})

		t.Run("the same value in the facet and the filter is one entry", func(t *testing.T) {
			r := getSearch(t, client, base, vals("categories", "snacks", "categories", "SNACKS").Encode())
			if r.Total != 10 || strings.Count(string(r.Facets["categories"]), `"snacks"`) != 1 {
				t.Errorf("total %d, categories %s; want 10 and one snacks entry", r.Total, r.Facets["categories"])
			}
		})
	})
}
