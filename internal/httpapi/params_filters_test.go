package httpapi

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

func TestParseSearchParams_Filters(t *testing.T) {
	invalid := func(msg string) *apiError { return invalidParameter(msg) }
	f := func(brands, categories, nutriscore []string) catalog.Query {
		return catalog.Query{Page: 1, Size: 20, Brands: brands, Categories: categories, Nutriscore: nutriscore}
	}
	rep := func(s string, n int) string { return strings.TrimSuffix(strings.Repeat(s, n), "&") }
	e256 := strings.Repeat("%C3%A9", 256)
	e257 := strings.Repeat("%C3%A9", 257)
	const (
		bUTF8    = "brands values must be valid UTF-8"
		bCtl     = "brands values must not contain control characters"
		bLen     = "brands values must be at most 256 characters"
		bMax     = "brands accepts at most 20 values"
		cUTF8    = "categories values must be valid UTF-8"
		cCtl     = "categories values must not contain control characters"
		cLen     = "categories values must be at most 256 characters"
		cMax     = "categories accepts at most 20 values"
		nMax     = "nutriscore accepts at most 20 values"
		nGrade   = "nutriscore values must be one of a, b, c, d, e"
		repeatQ  = "parameter q must appear at most once"
		repeatPg = "parameter page must appear at most once"
	)

	tests := []struct {
		name    string
		raw     string
		want    catalog.Query
		wantErr *apiError
	}{
		// accepted shapes
		{name: "no filters leaves nil slices", raw: "q=a", want: catalog.Query{Text: "a", Page: 1, Size: 20}},
		{name: "brands repeated accepted", raw: "brands=a&brands=b", want: f([]string{"a", "b"}, nil, nil)},
		{name: "categories repeated accepted", raw: "categories=y&categories=x", want: f(nil, []string{"x", "y"}, nil)},
		{name: "nutriscore repeated accepted", raw: "nutriscore=b&nutriscore=a", want: f(nil, nil, []string{"a", "b"})},
		{name: "all fields together", raw: "q=t&page=2&size=5&brands=B&categories=C&nutriscore=e",
			want: catalog.Query{Text: "t", Page: 2, Size: 5, Brands: []string{"B"}, Categories: []string{"C"}, Nutriscore: []string{"e"}}},
		{name: "trim skip empty dedupe sort", raw: "brands=b&brands=+a+&brands=a&brands=", want: f([]string{"a", "b"}, nil, nil)},
		{name: "dedupe is case sensitive", raw: "brands=a&brands=A", want: f([]string{"A", "a"}, nil, nil)},
		{name: "sort is bytewise", raw: "categories=%C3%A9&categories=z&categories=Z", want: f(nil, []string{"Z", "z", "é"}, nil)},
		{name: "only empty brands is nil", raw: "brands=&brands=+&brands", want: f(nil, nil, nil)},
		{name: "only empty categories is nil", raw: "categories=%20", want: f(nil, nil, nil)},
		{name: "only empty nutriscore is nil", raw: "nutriscore=&nutriscore=%20", want: f(nil, nil, nil)},
		{name: "nutriscore trimmed and sorted", raw: "nutriscore=%20e%20&nutriscore=a", want: f(nil, nil, []string{"a", "e"})},
		{name: "nutriscore dedupe", raw: "nutriscore=a&nutriscore=a", want: f(nil, nil, []string{"a"})},
		{name: "C1 NEL is trimmed away", raw: "brands=%C2%85", want: f(nil, nil, nil)},
		// Format characters (Cf: BOM, RLO, ZWSP) are not unicode.IsControl. The
		// spec rejects control characters only, so this pins the spec; rejecting
		// Cf would be a user decision.
		{name: "format characters are not control", raw: "brands=%EF%BB%BFa%E2%80%AEb%E2%80%8Bc", want: f([]string{"\uFEFFa\u202Eb\u200Bc"}, nil, nil)},
		{name: "format characters are not control in categories", raw: "categories=%EF%BB%BFa%E2%80%AEb%E2%80%8Bc", want: f(nil, []string{"\uFEFFa\u202Eb\u200Bc"}, nil)},
		{name: "interior space kept", raw: "brands=a+b", want: f([]string{"a b"}, nil, nil)},

		// counts
		{name: "20 brands accepted", raw: rep("brands=x&", 20), want: f([]string{"x"}, nil, nil)},
		{name: "21 brands refused", raw: rep("brands=x&", 21), wantErr: invalid(bMax)},
		{name: "21 brands refused even if empty", raw: rep("brands=&", 21), wantErr: invalid(bMax)},
		{name: "20 brands plus empty count 21", raw: rep("brands=x&", 20) + "&brands=", wantErr: invalid(bMax)},
		{name: "20 empty brands accepted", raw: rep("brands=&", 20), want: f(nil, nil, nil)},
		{name: "20 categories accepted", raw: rep("categories=x&", 20), want: f(nil, []string{"x"}, nil)},
		{name: "21 categories refused", raw: rep("categories=x&", 21), wantErr: invalid(cMax)},
		{name: "20 nutriscore accepted", raw: rep("nutriscore=a&", 20), want: f(nil, nil, []string{"a"})},
		{name: "21 nutriscore refused", raw: rep("nutriscore=a&", 21), wantErr: invalid(nMax)},
		{name: "21 empty nutriscore refused", raw: rep("nutriscore=&", 21), wantErr: invalid(nMax)},

		// lengths
		{name: "256 runes brand accepted", raw: "brands=" + e256, want: f([]string{strings.Repeat("é", 256)}, nil, nil)},
		{name: "257 runes brand refused", raw: "brands=" + e257, wantErr: invalid(bLen)},
		{name: "256 runes category accepted", raw: "categories=" + e256, want: f(nil, []string{strings.Repeat("é", 256)}, nil)},
		{name: "257 runes category refused", raw: "categories=" + e257, wantErr: invalid(cLen)},
		{name: "257 runes with spaces trimmed to 256", raw: "brands=+" + e256 + "+", want: f([]string{strings.Repeat("é", 256)}, nil, nil)},

		// content
		{name: "brand invalid utf-8", raw: "brands=%FF", wantErr: invalid(bUTF8)},
		{name: "category invalid utf-8", raw: "categories=%FF", wantErr: invalid(cUTF8)},
		{name: "brand nul", raw: "brands=%00", wantErr: invalid(bCtl)},
		{name: "brand tab inside", raw: "brands=a%09b", wantErr: invalid(bCtl)},
		{name: "brand C1 inside", raw: "brands=a%C2%85b", wantErr: invalid(bCtl)},
		{name: "category control", raw: "categories=a%01", wantErr: invalid(cCtl)},
		{name: "brand utf-8 before length", raw: "brands=%FF" + strings.Repeat("a", 300), wantErr: invalid(bUTF8)},
		{name: "brand control before length", raw: "brands=%00" + strings.Repeat("a", 300), wantErr: invalid(bCtl)},
		{name: "invalid utf-8 after valid value", raw: "brands=ok&brands=%FF", wantErr: invalid(bUTF8)},
		{name: "first bad value wins", raw: "brands=%00&brands=%FF", wantErr: invalid(bCtl)},
		{name: "nutriscore upper case", raw: "nutriscore=A", wantErr: invalid(nGrade)},
		{name: "nutriscore f", raw: "nutriscore=f", wantErr: invalid(nGrade)},
		{name: "nutriscore two letters", raw: "nutriscore=ab", wantErr: invalid(nGrade)},
		{name: "nutriscore bad among good", raw: "nutriscore=a&nutriscore=z", wantErr: invalid(nGrade)},
		{name: "nutriscore invalid utf-8", raw: "nutriscore=%FF", wantErr: invalid(nGrade)},

		// repeated key rules unchanged for q, page, size
		{name: "q twice still refused", raw: "q=a&q=b", wantErr: invalid(repeatQ)},
		{name: "page twice still refused", raw: "page=1&page=2", wantErr: invalid(repeatPg)},

		// order of checks
		{name: "unknown sorting first beats brands count", raw: rep("brands=x&", 21) + "&aaa=1", wantErr: invalid("parameter \"aaa\" is not allowed" + allowedList)},
		{name: "brands count sorting first beats unknown", raw: rep("brands=x&", 21) + "&foo=1", wantErr: invalid(bMax)},
		{name: "unknown beats brands value", raw: "brands=%FF&foo=1", wantErr: invalid("parameter \"foo\" is not allowed" + allowedList)},
		{name: "first unknown in sorted order", raw: "zzz=1&bbb=1&brand=1", wantErr: invalid("parameter \"bbb\" is not allowed" + allowedList)},
		{name: "sorted keys: brands count before q repeat", raw: "q=a&q=b&" + rep("brands=x&", 21), wantErr: invalid(bMax)},
		{name: "sorted keys: nutriscore count before q repeat", raw: "q=a&q=b&" + rep("nutriscore=a&", 21), wantErr: invalid(nMax)},
		{name: "sorted keys: brands count before page repeat", raw: rep("brands=x&", 21) + "&page=1&page=1", wantErr: invalid(bMax)},
		{name: "sorted keys: brands count before categories count", raw: rep("categories=x&", 21) + "&" + rep("brands=x&", 21), wantErr: invalid(bMax)},
		{name: "sorted keys: categories count before nutriscore count", raw: rep("nutriscore=a&", 21) + "&" + rep("categories=x&", 21), wantErr: invalid(cMax)},
		{name: "sorted keys: size repeat after nutriscore count", raw: rep("nutriscore=a&", 21) + "&size=1&size=1", wantErr: invalid(nMax)},
		{name: "counts beat q error", raw: "q=%FF&" + rep("brands=x&", 21), wantErr: invalid(bMax)},
		{name: "counts beat filter value error", raw: "brands=%FF&" + rep("categories=x&", 21), wantErr: invalid(cMax)},
		{name: "q beats brands", raw: "q=%FF&brands=%FF", wantErr: invalid(msgUTF8)},
		{name: "page beats brands", raw: "page=0&brands=%FF", wantErr: invalid(msgPage)},
		{name: "size beats brands", raw: "size=0&brands=%FF", wantErr: invalid(msgSize)},
		{name: "window beats brands", raw: "page=501&brands=%FF", wantErr: outOfRangeErr("page must be at most 500 when size is 20")},
		{name: "window beats nutriscore", raw: "page=501&nutriscore=z", wantErr: outOfRangeErr("page must be at most 500 when size is 20")},
		{name: "brands beat categories", raw: "brands=%00&categories=%FF", wantErr: invalid(bCtl)},
		{name: "brands beat nutriscore", raw: "brands=%00&nutriscore=z", wantErr: invalid(bCtl)},
		{name: "categories beat nutriscore", raw: "categories=%00&nutriscore=z", wantErr: invalid(cCtl)},
		{name: "brands first in input order does not matter", raw: "nutriscore=z&categories=%FF&brands=" + strings.Repeat("a", 257), wantErr: invalid(bLen)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				got, err := parseSearchParams(tt.raw)
				if (err == nil) != (tt.wantErr == nil) {
					t.Fatalf("parseSearchParams(%q) err = %v, want %v", tt.raw, err, tt.wantErr)
				}
				if tt.wantErr != nil {
					if *err != *tt.wantErr {
						t.Fatalf("parseSearchParams(%q) err = %+v, want %+v", tt.raw, *err, *tt.wantErr)
					}
					continue
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("parseSearchParams(%q) = %+v, want %+v", tt.raw, got, tt.want)
				}
			}
		})
	}
}

func outOfRangeErr(msg string) *apiError { return pageOutOfRange(msg) }

func TestParseSearchParams_UnknownKeyNamed(t *testing.T) {
	named := func(key string) string { return "parameter \"" + key + "\" is not allowed" + allowedList }
	tests := []struct {
		name string
		key  string // raw, as sent
		want string
	}{
		{"brand", "brand", named("brand")},
		{"brands brackets", "brands[]", named("brands[]")},
		{"brackets encoded", "brands%5B%5D", named("brands[]")},
		{"dots dashes underscores", "a.b-c_d", named("a.b-c_d")},
		{"32 runes", strings.Repeat("k", 32), named(strings.Repeat("k", 32))},
		{"33 runes", strings.Repeat("k", 33), msgUnknown},
		{"e acute", "%C3%A9", msgUnknown},
		{"double quote", "%22", msgUnknown},
		{"backslash", "%5C", msgUnknown},
		{"less than", "%3C", msgUnknown},
		{"space", "a+b", msgUnknown},
		{"nul", "%00", msgUnknown},
		{"invalid utf-8", "%FF", msgUnknown},
		{"empty key", "", msgUnknown},
		{"kelvin sign folds to k", "%E2%84%AA", msgUnknown},
		{"long s", "%C5%BF", msgUnknown},
		{"dotless i", "%C4%B1", msgUnknown},
		{"newline", "a%0A", msgUnknown},
		{"colon", "a%3Ab", msgUnknown},
		{"slash", "a%2Fb", msgUnknown},
		{"at sign", "%40", msgUnknown},
		{"backtick", "%60", msgUnknown},
		{"brace", "%7B", msgUnknown},
		{"digits", "0123456789", named("0123456789")},
		{"upper", "ABCXYZ", named("ABCXYZ")},
		{"boundary chars", "AZaz09", named("AZaz09")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.key + "=1"
			for i := 0; i < 20; i++ {
				_, err := parseSearchParams(raw)
				if err == nil {
					t.Fatalf("parseSearchParams(%q) accepted", raw)
				}
				if *err != *invalidParameter(tt.want) {
					t.Fatalf("parseSearchParams(%q) = %+v, want message %q", raw, *err, tt.want)
				}
			}
		})
	}
}

// The first unknown key in sorted order is named, even when a later key sorts
// before it in input order. "%FF" decodes to byte 0xFF, which sorts last, so
// the safe key is named.
func TestParseSearchParams_UnknownKeyFirstInSortedOrder(t *testing.T) {
	for i := 0; i < 20; i++ {
		_, err := parseSearchParams("zzz=1&%FF=1&mmm=1")
		if err == nil || err.message != "parameter \"mmm\" is not allowed"+allowedList {
			t.Fatalf("got %+v", err)
		}
	}
}

func TestParseSearchParams_SlicesAreFresh(t *testing.T) {
	got, err := parseSearchParams("brands=a&brands=b&categories=c&nutriscore=a")
	if err != nil {
		t.Fatal(err)
	}
	got.Brands[0] = "mutated"
	got.Categories[0] = "mutated"
	got.Nutriscore[0] = "mutated"
	again, _ := parseSearchParams("brands=a&brands=b&categories=c&nutriscore=a")
	if again.Brands[0] != "a" || again.Categories[0] != "c" || again.Nutriscore[0] != "a" {
		t.Errorf("results share state: %+v", again)
	}
}

// Decision 17: the parse result never carries an empty value to the searcher.
func TestParseSearchParams_NeverEmptyValues(t *testing.T) {
	raws := []string{
		"brands=&brands=a&brands=+", "categories=%20&categories=b&categories", "nutriscore=&nutriscore=a&nutriscore=%20",
		"brands=%C2%85&brands=a", "brands=%E2%80%A8&brands=a",
	}
	for _, raw := range raws {
		got, err := parseSearchParams(raw)
		if err != nil {
			t.Fatalf("parseSearchParams(%q): %+v", raw, *err)
		}
		for _, list := range [][]string{got.Brands, got.Categories, got.Nutriscore} {
			if list != nil && len(list) == 0 {
				t.Errorf("parseSearchParams(%q) returned an empty non-nil slice", raw)
			}
			for _, v := range list {
				if v == "" {
					t.Errorf("parseSearchParams(%q) returned an empty value: %+v", raw, got)
				}
			}
		}
	}
}

func TestParseSearchParams_FilterValuesDecoded(t *testing.T) {
	got, err := parseSearchParams("brands=" + url.QueryEscape("Nestlé & Co"))
	if err != nil || !reflect.DeepEqual(got.Brands, []string{"Nestlé & Co"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}
