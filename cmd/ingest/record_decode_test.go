package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// recStd is the header used by most decode tests: the six required columns in
// the order of recRequired, nothing else.
func recStd(t *testing.T) header {
	t.Helper()
	return recMustHeader(t, recJoin(recRequired...))
}

// recFields builds a standard line from code, name, brands, categories,
// countries and nutriscore.
func recFields(code, name, brands, cats, countries, nutri string) string {
	return recJoin(code, name, brands, cats, countries, nutri)
}

// recOK decodes a standard line and fails unless it decodes.
func recOK(t *testing.T, line string) record {
	t.Helper()
	got, reason := recStd(t).decode([]byte(line))
	if reason != skipNone {
		t.Fatalf("decode(%q) reason = %v, want none", line, reason)
	}
	return got
}

func recWantList(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func TestDecode_FieldCountMustMatchTheHeaderWidth(t *testing.T) {
	ok := recFields("1", "N", "B", "en:c", "en:us", "a")
	tests := []struct {
		name string
		line string
		want skipReason
	}{
		{"exact width", ok, skipNone},
		{"empty line", "", skipMalformed},
		{"one field", "1", skipMalformed},
		{"one field fewer", recJoin("1", "N", "B", "en:c", "en:us"), skipMalformed},
		{"one field more", ok + "\textra", skipMalformed},
		{"trailing tab on a full line", ok + "\t", skipMalformed},
		{"many more", ok + strings.Repeat("\tx", 50), skipMalformed},
		{"only tabs, right count", "\t\t\t\t\t", skipInvalidCode},
		{"only tabs, one more", "\t\t\t\t\t\t", skipMalformed},
		{"only tabs, one fewer", "\t\t\t\t", skipMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := recStd(t).decode([]byte(tt.line))

			if got != tt.want {
				t.Errorf("decode(%q) reason = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestDecode_WiderHeaderCountsItsOwnWidth(t *testing.T) {
	cols := []string{"url", "code", "product_name", "brands", "categories_tags", "countries_tags", "nutriscore_grade", "quantity"}
	h := recMustHeader(t, recJoin(cols...))
	ok := recLineFor(cols)

	if _, r := h.decode([]byte(ok)); r != skipNone {
		t.Errorf("full width line: reason = %v, want none", r)
	}
	// Same field count as the standard header but not as this one.
	if _, r := h.decode([]byte(recFields("1", "N", "B", "en:c", "en:us", "a"))); r != skipMalformed {
		t.Errorf("narrow line: reason = %v, want malformed", r)
	}
	// Last required column is not the last column: a short line must not be
	// decoded just because the needed fields exist.
	short := recJoin("u", "1", "N", "B", "en:c", "en:us", "a")
	if _, r := h.decode([]byte(short)); r != skipMalformed {
		t.Errorf("line missing the trailing extra column: reason = %v, want malformed", r)
	}
}

func TestDecode_QuoteIsLiteralDataInEveryField(t *testing.T) {
	got := recOK(t, recFields(`1"2`, `"Name`, `"B1",B"2`, `en:"c1,en:c2"`, `en:"us`, `"a`))

	if got.code != `1"2` || got.name != `"Name` {
		t.Errorf("code = %q, name = %q", got.code, got.name)
	}
	recWantList(t, "brands", got.brands, []string{`"B1"`, `B"2`})
	recWantList(t, "categories", got.categories, []string{`"c1`, `c2"`})
	recWantList(t, "countries", got.countries, []string{`en:"us`})
	if got.nutriscore != "" {
		t.Errorf("nutriscore = %q, want empty (a quote is not a grade)", got.nutriscore)
	}
}

func TestDecode_LoneQuoteDoesNotSwallowTheNextLine(t *testing.T) {
	h := recStd(t)
	first := recFields("1", `"Lone`, "B", "en:c", "en:us", "a")
	next := recFields("2", "Next", "B2", "en:c2", "en:ca", "b")

	r1, s1 := h.decode([]byte(first))
	r2, s2 := h.decode([]byte(next))

	if s1 != skipNone || r1.code != "1" || r1.name != `"Lone` {
		t.Errorf("first = %+v, %v", r1, s1)
	}
	if s2 != skipNone || r2.code != "2" || r2.name != "Next" {
		t.Errorf("next = %+v, %v", r2, s2)
	}
}

func TestDecode_Code(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		want     string
		wantSkip skipReason
	}{
		{"plain", "3017620422003", "3017620422003", skipNone},
		{"leading zeros kept", "0000012345", "0000012345", skipNone},
		{"ascii spaces trimmed", "  123  ", "123", skipNone},
		{"unicode spaces trimmed", " 123 ", "123", skipNone},
		{"inner space kept", "12 3", "12 3", skipNone},
		{"empty", "", "", skipInvalidCode},
		{"only spaces", "   ", "", skipInvalidCode},
		{"only unicode spaces", "  ", "", skipInvalidCode},
		{"128 bytes accepted", strings.Repeat("1", 128), strings.Repeat("1", 128), skipNone},
		{"129 bytes rejected", strings.Repeat("1", 129), "", skipInvalidCode},
		{"128 bytes plus spaces accepted", " " + strings.Repeat("1", 128) + " ", strings.Repeat("1", 128), skipNone},
		{"128 bytes of 2-byte runes accepted", strings.Repeat("é", 64), strings.Repeat("é", 64), skipNone},
		{"130 bytes of 2-byte runes rejected", strings.Repeat("é", 65), "", skipInvalidCode},
		{"NUL", "1\x002", "", skipInvalidCode},
		{"SOH", "1\x012", "", skipInvalidCode},
		{"US 0x1f", "1\x1f2", "", skipInvalidCode},
		{"newline", "1\n2", "", skipInvalidCode},
		{"carriage return", "1\r2", "", skipInvalidCode},
		{"DEL", "1\x7f2", "", skipInvalidCode},
		{"DEL at the end", "12\x7f", "", skipInvalidCode},
		{"C1 U+0080", "1\u00802", "", skipInvalidCode},
		{"C1 U+0085", "1\u00852", "", skipInvalidCode},
		{"C1 U+009F", "1\u009f2", "", skipInvalidCode},
		{"U+00A0 is a space, not a control", "1 2", "1 2", skipNone},
		{"space 0x20 inside is fine", "1 2", "1 2", skipNone},
		{"tilde 0x7e is fine", "1~2", "1~2", skipNone},
		{"U+0085 at the edge is trimmed like a space", "\u008512", "12", skipNone},
		{"NUL at the edge is not trimmed", "\x0012", "", skipInvalidCode},
		{"invalid UTF-8 is rejected, never replaced", "12\xff3", "", skipInvalidCode},
		{"invalid UTF-8 alone is rejected", "\xff", "", skipInvalidCode},
		{"truncated multi-byte rune is rejected", "12\xc4", "", skipInvalidCode},
		{"long invalid run is rejected", strings.Repeat("\xff", 200), "", skipInvalidCode},
		{"distinct invalid codes never collapse into one id", "\xfe", "", skipInvalidCode},
		{"2-byte rune is not a control (U+0101)", "1ā2", "1ā2", skipNone},
		{"2-byte rune encoding C4 81 is accepted", "1\xc4\x812", "1ā2", skipNone},
		{"U+0085 inside stays rejected", "1\u00852", "", skipInvalidCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := recStd(t).decode([]byte(recFields(tt.code, "N", "B", "en:c", "en:us", "a")))

			if reason != tt.wantSkip {
				t.Fatalf("reason = %v, want %v", reason, tt.wantSkip)
			}
			if got.code != tt.want {
				t.Errorf("code = %q, want %q", got.code, tt.want)
			}
		})
	}
}

func TestDecode_Name(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     string
		wantSkip skipReason
	}{
		{"plain", "Nutella", "Nutella", skipNone},
		{"case and inner spaces kept", " Café  Crème ", "Café  Crème", skipNone},
		{"unicode spaces trimmed", " Name ", "Name", skipNone},
		{"empty", "", "", skipNoName},
		{"only spaces", "   ", "", skipNoName},
		{"only unicode spaces", "   ", "", skipNoName},
		{"quote kept", `"x"`, `"x"`, skipNone},
		{"invalid UTF-8 replaced", "caf\xe9", "caf�", skipNone},
		{"names up to the cap are kept", strings.Repeat("n", maxNameBytes), strings.Repeat("n", maxNameBytes), skipNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := recStd(t).decode([]byte(recFields("1", tt.in, "B", "en:c", "en:us", "a")))

			if reason != tt.wantSkip {
				t.Fatalf("reason = %v, want %v", reason, tt.wantSkip)
			}
			if got.name != tt.want {
				t.Errorf("name = %q, want %q", got.name, tt.want)
			}
		})
	}
}

func TestDecode_ChecksFieldCountThenCodeThenName(t *testing.T) {
	h := recStd(t)
	// Bad code and no name: the code is checked first.
	if _, r := h.decode([]byte(recFields("", "", "B", "en:c", "en:us", "a"))); r != skipInvalidCode {
		t.Errorf("empty code and empty name: reason = %v, want invalid_code", r)
	}
	// Valid code and no name.
	if _, r := h.decode([]byte(recFields("1", " ", "B", "en:c", "en:us", "a"))); r != skipNoName {
		t.Errorf("empty name: reason = %v, want no_name", r)
	}
	// Bad code and the wrong field count: malformed wins.
	if _, r := h.decode([]byte(recJoin("", "", "B"))); r != skipMalformed {
		t.Errorf("wrong width: reason = %v, want malformed", r)
	}
	// A skipped line returns the zero record, not a half-filled one.
	for _, line := range []string{
		recFields("1\x002", "N", "B", "en:c", "en:us", "a"), // invalid code
		recFields("1", "", "B", "en:c", "en:us", "a"),       // no name
		recJoin("1", "N", "B"),                              // malformed
	} {
		if got, _ := h.decode([]byte(line)); !reflect.DeepEqual(got, record{}) {
			t.Errorf("skipped record for %q = %+v, want the zero record", line, got)
		}
	}
}

func TestDecode_Brands(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"one", "Ferrero", []string{"Ferrero"}},
		{"two", "Ferrero,Nutella", []string{"Ferrero", "Nutella"}},
		{"trimmed", " Ferrero , Nutella ", []string{"Ferrero", "Nutella"}},
		{"inner double space kept", "Big Win,  Rite Aid Corporation", []string{"Big Win", "Rite Aid Corporation"}},
		{"empties dropped", ",a,,b, ,", []string{"a", "b"}},
		{"only separators", ", ,,", nil},
		{"empty field", "", nil},
		{"duplicates removed keeping the first position", "b,a,b,c,a", []string{"b", "a", "c"}},
		{"duplicates after trimming", "a, a ,a", []string{"a"}},
		{"dedupe is case sensitive", "a,A,a", []string{"a", "A"}},
		{"case preserved", "FERRERO", []string{"FERRERO"}},
		{"invalid UTF-8 replaced", "B\xffx,C", []string{"B�x", "C"}},
		// "raw" means case and spacing preserved, not byte identical: the
		// dedupe runs after the U+FFFD replacement, so it matches what is stored.
		{"dedupe happens after the replacement", "\xff,\xfe", []string{"�"}},
		{"dedupe after the replacement keeps the first position", "a,\xff,b,\xfe,\xff", []string{"a", "�", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recOK(t, recFields("1", "N", tt.in, "en:c", "en:us", "a"))

			recWantList(t, "brands", got.brands, tt.want)
		})
	}
}

func TestDecode_Categories(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"en tags keep the slug", "en:snacks,en:sweet-snacks", []string{"snacks", "sweet-snacks"}},
		{"other languages dropped", "fr:a,en:b,de:c", []string{"b"}},
		{"only non-en", "fr:sans-viande-hache", nil},
		{"empty field", "", nil},
		{"prefix alone dropped", "en:,en:a", []string{"a"}},
		{"prefix with spaces dropped", "en: ,en:a", []string{"a"}},
		{"upper case prefix dropped", "EN:x,En:y,en:z", []string{"z"}},
		{"leading space is trimmed before the prefix check", " en:x", []string{"x"}},
		{"prefix must be at the start", "xen:a,fr:en:b,en:c", []string{"c"}},
		{"prefix stripped once", "en:en:a", []string{"en:a"}},
		{"slug case preserved", "en:Groceries", []string{"Groceries"}},
		{"duplicates removed keeping the first position", "en:b,en:a,en:b,en:c", []string{"b", "a", "c"}},
		{"duplicates after trimming", "en:a, en:a", []string{"a"}},
		{"empties dropped", ",en:a,,", []string{"a"}},
		{"slug is trimmed after the prefix", "en: x", []string{"x"}},
		{"slug is trimmed on both sides after the prefix", "en: \u00a0x \u00a0", []string{"x"}},
		{"duplicates after trimming the slug", "en:x,en: x", []string{"x"}},
		{"no prefix at all", "snacks", nil},
		{"invalid UTF-8 replaced", "en:c\xff,fr:x", []string{"c�"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recOK(t, recFields("1", "N", "B", tt.in, "en:us", "a"))

			recWantList(t, "categories", got.categories, tt.want)
		})
	}
}

func TestDecode_CountriesKeepTheirPrefix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"one", "en:united-states", []string{"en:united-states"}},
		{"several", "en:france,en:germany,en:world", []string{"en:france", "en:germany", "en:world"}},
		{"trimmed", " en:france , en:spain", []string{"en:france", "en:spain"}},
		{"empties dropped", ",en:a, ,", []string{"en:a"}},
		{"empty field", "", nil},
		{"case preserved", "EN:France", []string{"EN:France"}},
		{"other prefixes kept as they are", "fr:france", []string{"fr:france"}},
		{"not deduplicated", "en:a,en:a", []string{"en:a", "en:a"}},
		{"invalid UTF-8 replaced", "en:a\xfe", []string{"en:a�"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recOK(t, recFields("1", "N", "B", "en:c", tt.in, "a"))

			recWantList(t, "countries", got.countries, tt.want)
		})
	}
}

func TestDecode_Nutriscore(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"a", "a"}, {"b", "b"}, {"c", "c"}, {"d", "d"}, {"e", "e"},
		{"A", "a"}, {"B", "b"}, {"C", "c"}, {"D", "d"}, {"E", "e"},
		{" b ", "b"}, {" c ", "c"},
		{"unknown", ""}, {"not-applicable", ""}, {"", ""}, {" ", ""},
		{"f", ""}, {"F", ""}, {"`", ""}, {"@", ""}, {"z", ""},
		{"ab", ""}, {"aa", ""}, {"a1", ""}, {"a b", ""}, {"A+garbage", ""},
		{"1", ""}, {"ａ", ""}, {"é", ""},
	}
	for _, tt := range tests {
		got := recOK(t, recFields("1", "N", "B", "en:c", "en:us", tt.in))

		if got.nutriscore != tt.want {
			t.Errorf("nutriscore(%q) = %q, want %q", tt.in, got.nutriscore, tt.want)
		}
	}
}

func TestDecode_NutriscoreEverySingleByte(t *testing.T) {
	for b := 0; b < 256; b++ {
		in := string([]byte{byte(b)})
		if b == '\t' {
			continue // a tab is the separator
		}
		want := ""
		switch {
		case b >= 'a' && b <= 'e':
			want = string(rune(b))
		case b >= 'A' && b <= 'E':
			want = string(rune(b + 'a' - 'A'))
		}

		got := recOK(t, recFields("1", "N", "B", "en:c", "en:us", in))

		if got.nutriscore != want {
			t.Errorf("nutriscore(byte 0x%02x) = %q, want %q", b, got.nutriscore, want)
		}
	}
}

func TestDecode_DoesNotAliasTheInputLine(t *testing.T) {
	line := []byte(recFields("12345", "Some Name", "B1,B2", "en:c1,en:c2", "en:us,en:ca", "b"))
	got, reason := recStd(t).decode(line)
	if reason != skipNone {
		t.Fatalf("reason = %v", reason)
	}
	want := record{
		code: "12345", name: "Some Name", brands: []string{"B1", "B2"},
		categories: []string{"c1", "c2"}, nutriscore: "b", countries: []string{"en:us", "en:ca"},
	}

	for i := range line {
		line[i] = 'Z' // the line reader reuses its buffer on the next call
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("record changed after the input was overwritten: %+v, want %+v", got, want)
	}
}

func TestRecord_ProductHasNonNilSlices(t *testing.T) {
	p := record{code: "1", name: "N"}.product()

	if p.Brands == nil || p.Categories == nil {
		t.Fatalf("Brands = %#v, Categories = %#v, want non-nil empty slices", p.Brands, p.Categories)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, want := range []string{`"brands":[]`, `"categories":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("JSON %s lacks %s", raw, want)
		}
	}
}

func TestRecord_ProductCarriesTheDocumentFields(t *testing.T) {
	r := record{
		code: "0001", name: "Name", brands: []string{"B1", "B2"},
		categories: []string{"c1"}, nutriscore: "c", countries: []string{"en:us"},
	}

	p := r.product()

	if p.ID != "0001" || p.Name != "Name" || p.Nutriscore != "c" {
		t.Errorf("product = %+v", p)
	}
	recWantList(t, "Brands", p.Brands, r.brands)
	recWantList(t, "Categories", p.Categories, r.categories)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 4 {
		t.Errorf("JSON %s has %d keys, want exactly name, brands, categories, nutriscore", raw, len(keys))
	}
	for _, k := range []string{"name", "brands", "categories", "nutriscore"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("JSON %s lacks key %q", raw, k)
		}
	}
	if strings.Contains(string(raw), "united") || strings.Contains(string(raw), "en:us") {
		t.Errorf("JSON %s carries countries, which are not part of the document", raw)
	}
}
