package main

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// recReference is a naive, strings.Split based decoder of a standard six
// column line. It is written for clarity, not speed, and shares no code with
// the real decoder.
func recReference(line string) (record, skipReason) {
	f := strings.Split(line, "\t")
	if len(f) != 6 {
		return record{}, skipMalformed
	}
	fix := func(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }
	code := strings.TrimSpace(f[0])
	bad := code == "" || len(code) > 128 || !utf8.ValidString(code)
	for _, r := range code {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			bad = true
		}
	}
	if bad {
		return record{}, skipInvalidCode
	}
	if len(f[1]) > maxNameBytes || len(f[2]) > maxListFieldBytes || len(f[3]) > maxListFieldBytes || len(f[4]) > maxListFieldBytes {
		return record{}, skipOversized
	}
	name := fix(strings.TrimSpace(f[1]))
	if name == "" {
		return record{}, skipNoName
	}
	rec := record{code: code, name: name}
	for _, b := range strings.Split(f[2], ",") {
		if b = fix(strings.TrimSpace(b)); b != "" && !slices.Contains(rec.brands, b) {
			rec.brands = append(rec.brands, b)
		}
	}
	for _, c := range strings.Split(f[3], ",") {
		c = strings.TrimSpace(c)
		if !strings.HasPrefix(c, "en:") {
			continue
		}
		if c = fix(strings.TrimSpace(strings.TrimPrefix(c, "en:"))); c != "" && !slices.Contains(rec.categories, c) {
			rec.categories = append(rec.categories, c)
		}
	}
	for _, c := range strings.Split(f[4], ",") {
		if c = fix(strings.TrimSpace(c)); c != "" {
			rec.countries = append(rec.countries, c)
		}
	}
	if g := strings.ToLower(strings.TrimSpace(f[5])); slices.Contains([]string{"a", "b", "c", "d", "e"}, g) {
		rec.nutriscore = g
	}
	return rec, skipNone
}

func recSame(a, b record) bool {
	return a.code == b.code && a.name == b.name && a.nutriscore == b.nutriscore &&
		slices.Equal(a.brands, b.brands) && slices.Equal(a.categories, b.categories) && slices.Equal(a.countries, b.countries)
}

// recTokens is the alphabet of the random lines: separators, prefixes, spaces
// (ASCII and Unicode), quotes, controls, invalid bytes and a few letters.
var recTokens = []string{
	",", ",", "en:", "en:", "fr:", "EN:", " ", " ", "\u00a0", "\"", "a", "b", "c", "A", "e", "f",
	"é", "\xff", "\x00", "\x7f", "\u0085", "\r", "\u0080", "\u009f", "ā", "1", "2", "unknown", "x", "en:a", "en:b", ", ",
}

func recRandomText(r *rand.Rand, maxTokens int) string {
	var b strings.Builder
	for n := r.Intn(maxTokens + 1); n > 0; n-- {
		b.WriteString(recTokens[r.Intn(len(recTokens))])
	}
	return b.String()
}

// recRandomLine returns, half of the time, a line with exactly six fields (so
// the field rules are exercised) and otherwise tabs anywhere.
func recRandomLine(r *rand.Rand) string {
	if r.Intn(2) == 0 {
		f := make([]string, 6)
		for i := range f {
			f[i] = recRandomText(r, 8)
		}
		if r.Intn(2) == 0 {
			f[0] = []string{"1", "123", "0042", " 7 "}[r.Intn(4)]
		}
		if r.Intn(2) == 0 {
			f[1] = []string{"N", " Name ", "Café"}[r.Intn(3)]
		}
		if r.Intn(6) == 0 {
			// Codes around the 128 byte limit, with 1 and 2 byte characters.
			unit := []string{"1", "é", "ā", " "}[r.Intn(4)]
			f[0] = strings.Repeat(unit, 120/len(unit)+r.Intn(12)) + []string{"", "x", "\xff", "\u0085"}[r.Intn(4)]
		}
		if r.Intn(8) == 0 {
			// Fields around the size caps: raw length 1,020 to 1,030 for the
			// name, 16,380 to 16,390 for a list.
			pad := func(n int) string { return strings.Repeat(" ", r.Intn(3)) + strings.Repeat("a", n) }
			f[1] = pad(maxNameBytes - 4 + r.Intn(11))
			if i := 2 + r.Intn(3); r.Intn(2) == 0 {
				f[i] = pad(maxListFieldBytes - 4 + r.Intn(11))
			}
		}
		return recJoin(f...)
	}
	var b strings.Builder
	for n := r.Intn(10); n > 0; n-- {
		b.WriteString(recRandomText(r, 4))
		if r.Intn(2) == 0 {
			b.WriteByte('\t')
		}
	}
	return b.String()
}

func TestDecode_MatchesNaiveReferenceOnRandomLines(t *testing.T) {
	h := recStd(t)
	const cases = 5000
	r := rand.New(rand.NewSource(20261111))
	seen := map[skipReason]int{}
	withLists := 0

	for i := 0; i < cases; i++ {
		line := recRandomLine(r)
		want, wantReason := recReference(line)

		got, reason := h.decode([]byte(line))

		seen[reason]++
		if len(want.brands) > 0 && len(want.categories) > 0 && len(want.countries) > 0 {
			withLists++
		}
		if reason != wantReason || !recSame(got, want) {
			shown := line
			if len(shown) > 300 {
				shown = shown[:300] + "..." // lines near the caps are long
			}
			t.Fatalf("case %d, line %q (%d bytes):\n got  %.300v, %v\n want %.300v, %v", i, shown, len(line), got, reason, want, wantReason)
		}
	}

	// The generator must reach every outcome and non-trivial lists.
	for _, reason := range []skipReason{skipNone, skipMalformed, skipInvalidCode, skipNoName} {
		if seen[reason] < 100 {
			t.Errorf("only %d random lines gave %v; the generator is too narrow", seen[reason], reason)
		}
	}
	if seen[skipOversized] < 20 {
		t.Errorf("only %d random lines gave oversized; the generator never reaches the caps", seen[skipOversized])
	}
	if withLists < 50 {
		t.Errorf("only %d lines with brands, categories and countries", withLists)
	}
}
