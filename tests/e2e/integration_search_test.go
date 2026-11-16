package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// docIDs expands the last three digits of fixture codes (9991000000NNN) into
// full ids, so the expectation tables stay readable.
func docIDs(suffixes ...string) []string {
	ids := make([]string, len(suffixes))
	for i, s := range suffixes {
		ids[i] = "9991000000" + s
	}
	return ids
}

type facetPair struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// searchCase is one text search and its expectation. The expectations are
// hand-derived from testdata/integration.tsv: the query is analyzed like the
// field (lower case, accents folded, standard tokenizer, so plant-based-foods
// gives plant, based and foods) and fuzziness AUTO allows 0 edits up to 2
// characters, 1 edit for 3 to 5 and 2 edits from 6, never in the first
// character (prefix_length 1). The ids are the set of matches, never an order.
type searchCase struct {
	name string
	q    string
	ids  []string // last three digits of the codes
	why  string
}

var searchCases = []searchCase{
	{
		name: "a 2-edit typo of a long word is reached by fuzziness", q: "chcolat", ids: []string{"001", "002", "003", "004"},
		why: "chocolate is 2 edits from chcolat (insert o, append e); 7 letters allow 2; no other term is within 2",
	},
	{
		name: "a typo in the second letter is reached", q: "cxocolate", ids: []string{"001", "002", "003", "004"},
		why: "chocolate is 1 edit away; prefix_length 1 keeps only the first letter exact, so the second may differ",
	},
	{
		name: "a name word", q: "yogurt", ids: []string{"012"},
		why: "only Cherry Cheesecake Yogurt has a term starting with y",
	},
	{
		name: "a word of a brand only", q: "cocoa", ids: []string{"001", "002"},
		why: "Cocoa Crest is a brand; no name or category holds the word",
	},
	{
		name: "a word inside a hyphenated category", q: "based", ids: []string{"008", "009", "017", "018", "020", "022"},
		why: "plant-based-foods is split into plant, based, foods",
	},
	{
		name: "a typo of one name word is reached by fuzziness", q: "choclate", ids: []string{"001", "002", "003", "004"},
		why: "chocolate is 1 edit away (the 8 letters allow 2); chocolates, the category of the same four rows, is 2 edits away",
	},
	{
		name: "a typo in the first letter is not reached", q: "xhocolate", ids: nil,
		why: "prefix_length 1 keeps the first letter exact",
	},
	{
		name: "fuzziness crosses fields", q: "milk", ids: []string{"002", "011", "016", "018", "019", "024"},
		why: "milk is in the names of 002, 018, 019; mill (the brand Crunch Mill) is 1 edit away: 011, 016, 024",
	},
	{
		name: "an accent-free query finds the accented rows (fuzziness alone would too)", q: "creme", ids: []string{"013", "014", "015"},
		why: "Crème Fraîche, Creme Fraiche and CREME FRAICHE all fold to creme fraiche; crème is also 1 edit from creme, so this does not prove folding alone",
	},
	{
		name: "an accented query finds the same rows (does not prove folding alone)", q: "Crème", ids: []string{"013", "014", "015"},
		why: "crème is 1 edit from creme even unfolded; only Crèmé below proves folding",
	},
	{
		name: "accents in several places are folded (the case that proves folding)", q: "Crèmé", ids: []string{"013", "014", "015"},
		why: "folded to creme before the fuzzy match; unfolded it would be 2 edits from creme (014, 015) and 1 from crème (013)",
	},
	{
		name: "an accent-free brand word finds the accented brand (fuzziness alone would too)", q: "acme", ids: []string{"013", "014", "015"},
		why: "Acme Foods, ACME FOODS and Acmé Foods fold to acme foods; acmé is 1 edit from acme, so this does not prove folding alone",
	},
	{
		name: "two words are an OR", q: "tomato soup", ids: []string{"007", "022", "023"},
		why: "tomato: 022, 023; soup also reaches soups (1 edit): 007, 023",
	},
	{
		name: "a word of a row the ingest dropped", q: "baguette", ids: nil,
		why: "Baguette Tradition has only en:france and is not indexed",
	},
	{
		name: "a word of another dropped row", q: "wasabi", ids: nil,
		why: "Wasabi Rice Crackers has only fr: categories and is not indexed",
	},
}

// searchIntegrationSubtests runs the text search cases against the real engine.
func searchIntegrationSubtests(t *testing.T, client *http.Client, base string) {
	t.Run("text search", func(t *testing.T) {
		for _, tc := range searchCases {
			t.Run(tc.name, func(t *testing.T) {
				r := getSearch(t, client, base, "size=100&q="+url.QueryEscape(tc.q))
				want := docIDs(tc.ids...)
				slices.Sort(want)
				got := itemIDs(r.Items)
				slices.Sort(got)
				if r.Total != len(want) || !slices.Equal(got, want) {
					t.Errorf("q=%q: total %d, ids %v; want total %d, ids %v (%s)", tc.q, r.Total, got, len(want), want, tc.why)
				}
			})
		}
		t.Run("a query that matches nothing has empty lists and zero grades", func(t *testing.T) {
			r := getSearch(t, client, base, "q=zzqx")
			if r.Total != 0 || r.Items == nil || len(r.Items) != 0 {
				t.Errorf("q=zzqx: total %d, items %v; want 0 and []\n%s", r.Total, r.Items, r.body)
			}
			wantFacets(t, r, nil, nil, []facetPair{{"a", 0}, {"b", 0}, {"c", 0}, {"d", 0}, {"e", 0}})
		})
		t.Run("the facets follow the matches of q", func(t *testing.T) {
			// tomato soup matches 007 (Nordic Farms + Green Valley, soups, d),
			// 022 (Nonna Pantry, sauces + plant-based-foods, d) and 023 (Nonna
			// Pantry, soups + sauces, e). Order: count desc, then key asc.
			r := getSearch(t, client, base, "q="+url.QueryEscape("tomato soup"))
			wantFacets(t, r,
				[]facetPair{{"nonna pantry", 2}, {"green valley", 1}, {"nordic farms", 1}},
				[]facetPair{{"sauces", 2}, {"soups", 2}, {"plant-based-foods", 1}},
				[]facetPair{{"a", 0}, {"b", 0}, {"c", 0}, {"d", 2}, {"e", 1}})
		})
	})
}

// wantFacets compares the three facet lists exactly, in order. A nil brands or
// categories list means the empty list, which must be [] and not null.
func wantFacets(t *testing.T, r searchReply, brands, categories, grades []facetPair) {
	t.Helper()
	for _, c := range []struct {
		key  string
		want []facetPair
	}{{"brands", brands}, {"categories", categories}, {"nutriscore", grades}} {
		raw := r.Facets[c.key]
		var got []facetPair
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("facet %s: bad JSON %s: %v", c.key, raw, err)
			continue
		}
		if c.want == nil && string(raw) != "[]" {
			t.Errorf("facet %s = %s, want []", c.key, raw)
		}
		if !slices.Equal(got, c.want) && len(got)+len(c.want) > 0 {
			t.Errorf("facet %s = %v, want %v", c.key, got, c.want)
		}
	}
}
