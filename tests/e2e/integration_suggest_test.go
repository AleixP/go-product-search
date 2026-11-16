package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// suggestGroup is one expected suggestion: the raw spellings that fold to one
// key (name.norm). The API shows one of them, and which one is not
// deterministic when scores tie, so a suggestion matches the group when its
// text is any of the spellings.
type suggestGroup []string

// Names of testdata/integration.tsv used by the suggest cases (indexed rows
// only; 101 Baguette Tradition, 102 Wasabi Rice Crackers, 103 Plain Rice Cakes
// and 104 Maple Syrup are dropped by the ingest and must never be suggested).
var (
	gDark       = suggestGroup{"Dark Chocolate Bar"}
	gMilkChoc   = suggestGroup{"Milk Chocolate Bar"}
	gWhiteChoc  = suggestGroup{"White Chocolate Chips"}
	gHazelnut   = suggestGroup{"Chocolate Hazelnut Spread"}
	gCreme      = suggestGroup{"Crème Fraîche", "Creme Fraiche", "CREME FRAICHE"}
	gCream      = suggestGroup{"Chestnut Cream"}
	gIceCream   = suggestGroup{"Vanilla Ice Cream"}
	gLongWord   = suggestGroup{"Supercalifragilistic Crunch"}
	suggestCh12 = []suggestGroup{ // every name with a token starting "ch": 12 groups
		gDark, gMilkChoc, gWhiteChoc, gHazelnut,
		{"Cheddar Cheese Slices"}, {"Chili Cheese Crackers"}, {"Chicken Noodle Soup"},
		{"Chickpea Hummus"}, {"Chamomile Herbal Tea"}, gCream, {"Chewy Granola Bars"},
		{"Cherry Cheesecake Yogurt"},
	}
)

// suggestCase is a suggest request whose reply is the exact set of groups.
// Derivation (T-6 rule): every query token is folded, tokens under 2 letters
// are dropped, tokens are cut to 15 letters, and each must equal a prefix (2 to
// 15 letters) of some token of the name; all tokens must match (operator and).
type suggestCase struct {
	name string
	q    string
	want []suggestGroup
}

var suggestCases = []suggestCase{
	{"choc upper case", "CHOC", []suggestGroup{gDark, gMilkChoc, gWhiteChoc, gHazelnut}},
	{"choc lower case", "choc", []suggestGroup{gDark, gMilkChoc, gWhiteChoc, gHazelnut}},
	{"choc with an accent", "Chóc", []suggestGroup{gDark, gMilkChoc, gWhiteChoc, gHazelnut}},
	{"case and accent duplicates come back as one suggestion", "frai", []suggestGroup{gCreme}},
	{"an accented query finds the folded names", "CRÈME", []suggestGroup{gCreme}},
	{"cre reaches cream and creme but not crunch, crackers, crisps", "cre", []suggestGroup{gCream, gCreme, gIceCream}},
	{"two words in one order", "chocolate bar", []suggestGroup{gDark, gMilkChoc}},
	{"two words in the other order", "bar chocolate", []suggestGroup{gDark, gMilkChoc}},
	{"two words need both: dark and milk never share a name", "dark milk", nil},
	{"a prefix on any word, not only the last", "dark choc", []suggestGroup{gDark}},
	{"the second word is a prefix too", "chocolate haz", []suggestGroup{gHazelnut}},
	{"one-letter tail is dropped", "dark chocolate m", []suggestGroup{gDark}},
	{"two words without the tail", "dark chocolate", []suggestGroup{gDark}},
	{"the long word by its full spelling", "supercalifragilistic", []suggestGroup{gLongWord}},
	{"the long word by its first 15 letters", "supercalifragil", []suggestGroup{gLongWord}},
	{"a 17-letter prefix is cut to 15 and found", "supercalifragilis", []suggestGroup{gLongWord}},
	{"a 16th letter that differs is cut away and found", "supercalifragilx", []suggestGroup{gLongWord}},
	{"a 15th letter that differs is not found", "supercalifragix", nil},
	{"a 2-letter prefix of a second word", "su cr", []suggestGroup{gLongWord}},
	{"a name from a dropped row (country)", "baguette", nil},
	{"a name from a dropped row (category)", "wasabi", nil},
	{"a name from a dropped row (brand)", "plain", nil},
	{"a name from a dropped row (nutriscore)", "maple", nil},
	{"no match", "zzqx", nil},
	{"one letter gives an empty list", "m", nil},
	// Known limit (accepted): index-time asciifolding of the suggest field is not observable
	// with this fixture, which has no accent-only name.
}

type suggestReply struct {
	Suggestions []struct {
		Text string `json:"text"`
	} `json:"suggestions"`
	body string
}

func (r suggestReply) texts() []string {
	out := make([]string, len(r.Suggestions))
	for i, s := range r.Suggestions {
		out[i] = s.Text
	}
	return out
}

// getSuggest sends GET /suggest?<query> and decodes the 200 reply; the list
// must be [] and never null.
func getSuggest(t *testing.T, client *http.Client, base, query string) suggestReply {
	t.Helper()
	got := do(t, client, http.MethodGet, base+"/suggest?"+query)
	if got.status != http.StatusOK {
		t.Fatalf("GET /suggest?%s status = %d, want 200\nbody: %s", query, got.status, got.body)
	}
	var r suggestReply
	if err := json.Unmarshal([]byte(got.body), &r); err != nil {
		t.Fatalf("GET /suggest?%s: bad JSON: %v\nbody: %s", query, err, got.body)
	}
	if r.Suggestions == nil {
		t.Fatalf("GET /suggest?%s: suggestions is null, want a list\nbody: %s", query, got.body)
	}
	r.body = got.body
	return r
}

// groupOf returns the index of the group holding text, or -1.
func groupOf(groups []suggestGroup, text string) int {
	return slices.IndexFunc(groups, func(g suggestGroup) bool { return slices.Contains(g, text) })
}

// checkGroups fails unless texts holds exactly one spelling of each wanted
// group and nothing else.
func checkGroups(t *testing.T, label string, texts []string, want []suggestGroup) {
	t.Helper()
	seen := make([]int, len(want))
	for _, s := range texts {
		i := groupOf(want, s)
		if i < 0 {
			t.Errorf("%s: unexpected suggestion %q in %q", label, s, texts)
			continue
		}
		seen[i]++
	}
	for i, n := range seen {
		if n != 1 {
			t.Errorf("%s: group %q shown %d times, want 1 (got %q)", label, want[i], n, texts)
		}
	}
}

// checkSubset fails unless texts has exactly n suggestions, each from a
// different group of the pool.
func checkSubset(t *testing.T, label string, texts []string, n int, pool []suggestGroup) {
	t.Helper()
	if len(texts) != n {
		t.Errorf("%s: %d suggestions %q, want %d", label, len(texts), texts, n)
	}
	used := map[int]bool{}
	for _, s := range texts {
		i := groupOf(pool, s)
		switch {
		case i < 0:
			t.Errorf("%s: unexpected suggestion %q", label, s)
		case used[i]:
			t.Errorf("%s: group %q shown twice", label, pool[i])
		default:
			used[i] = true
		}
	}
}

// suggestIntegrationSubtests runs the suggest cases against the real engine.
func suggestIntegrationSubtests(t *testing.T, client *http.Client, base string) {
	t.Run("suggest", func(t *testing.T) {
		for _, tc := range suggestCases {
			t.Run(tc.name, func(t *testing.T) {
				r := getSuggest(t, client, base, "size=20&q="+url.QueryEscape(tc.q))
				checkGroups(t, "q="+tc.q, r.texts(), tc.want)
			})
		}
		t.Run("a 2-letter prefix gives min(10, groups) with the default size", func(t *testing.T) {
			// ch matches 12 groups; the default size is 10.
			checkSubset(t, "ch", getSuggest(t, client, base, "q=ch").texts(), 10, suggestCh12)
		})
		t.Run("size 11 and size 12 bound the 12 groups", func(t *testing.T) {
			checkSubset(t, "ch size=11", getSuggest(t, client, base, "q=ch&size=11").texts(), 11, suggestCh12)
			checkSubset(t, "ch size=12", getSuggest(t, client, base, "q=ch&size=12").texts(), 12, suggestCh12)
		})
		t.Run("size 1 gives exactly one", func(t *testing.T) {
			checkSubset(t, "ch size=1", getSuggest(t, client, base, "q=ch&size=1").texts(), 1, suggestCh12)
		})
		t.Run("size 20 gives all the groups", func(t *testing.T) {
			checkGroups(t, "ch size=20", getSuggest(t, client, base, "q=ch&size=20").texts(), suggestCh12)
		})
		t.Run("a size above the groups does not pad or repeat", func(t *testing.T) {
			checkGroups(t, "cre size=20", getSuggest(t, client, base, "q=cre&size=20").texts(), []suggestGroup{gCream, gCreme, gIceCream})
		})
		t.Run("case and accent variants give byte-identical bodies", func(t *testing.T) {
			for _, set := range [][]string{{"CHOC", "choc", "Chóc"}, {"CRÈME", "creme", "Creme", "crème"}} {
				first := getSuggest(t, client, base, "q="+url.QueryEscape(set[0])).body
				for _, q := range set[1:] {
					if got := getSuggest(t, client, base, "q="+url.QueryEscape(q)).body; got != first {
						t.Errorf("q=%q body %s differs from q=%q body %s", q, got, set[0], first)
					}
				}
			}
		})
		t.Run("trailing one-letter word gives the same body", func(t *testing.T) {
			a := getSuggest(t, client, base, "q="+url.QueryEscape("dark chocolate m")).body
			b := getSuggest(t, client, base, "q="+url.QueryEscape("dark chocolate")).body
			if a != b || !strings.Contains(a, "Dark Chocolate Bar") {
				t.Errorf("bodies differ or miss the name: %s vs %s", a, b)
			}
		})
	})
}
