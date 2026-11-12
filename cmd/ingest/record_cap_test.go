package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// listCases name the three list fields of a standard line (brands, categories,
// countries) and put a field value in the right place.
var recListCases = []struct {
	name string
	line func(field string) string
}{
	{"brands", func(f string) string { return recFields("1", "N", f, "en:c", "en:us", "a") }},
	{"categories_tags", func(f string) string { return recFields("1", "N", "B", f, "en:us", "a") }},
	{"countries_tags", func(f string) string { return recFields("1", "N", "B", "en:c", f, "a") }},
}

// The caps are part of the contract (4x and about 8x the real maxima): a change
// must be deliberate.
func TestCaps_AreOneKiBAndSixteenKiB(t *testing.T) {
	if maxNameBytes != 1024 || maxListFieldBytes != 16384 {
		t.Errorf("maxNameBytes = %d, maxListFieldBytes = %d, want 1024 and 16384", maxNameBytes, maxListFieldBytes)
	}
}

func TestDecode_NameCap(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want skipReason
	}{
		{"exactly the cap", strings.Repeat("n", maxNameBytes), skipNone},
		{"one byte over", strings.Repeat("n", maxNameBytes+1), skipOversized},
		{"the cap counts raw bytes, not trimmed ones", strings.Repeat("n", maxNameBytes) + " ", skipOversized},
		{"raw leading space counts too", " " + strings.Repeat("n", maxNameBytes), skipOversized},
		{"spaces only over the cap are oversized, not no_name", strings.Repeat(" ", maxNameBytes+1), skipOversized},
		{"spaces only within the cap are no_name", strings.Repeat(" ", maxNameBytes), skipNoName},
		{"a 4 MiB name", strings.Repeat("n", 4<<20), skipOversized},
		{"the cap counts bytes: 512 two-byte runes fit", strings.Repeat("é", maxNameBytes/2), skipNone},
		{"the cap counts bytes: 513 two-byte runes do not", strings.Repeat("é", maxNameBytes/2+1), skipOversized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := recStd(t).decode([]byte(recFields("1", tt.in, "B", "en:c", "en:us", "a")))

			if reason != tt.want {
				t.Fatalf("reason = %v, want %v", reason, tt.want)
			}
			if reason != skipNone && got.code != "" {
				t.Errorf("skipped record = %+v, want the zero record", got)
			}
		})
	}
}

func TestDecode_ListFieldCap(t *testing.T) {
	for _, lc := range recListCases {
		prefix := ""
		if lc.name == "categories_tags" {
			prefix = "en:"
		}
		tests := []struct {
			name string
			in   string
			want skipReason
		}{
			{"exactly the cap", prefix + strings.Repeat("a", maxListFieldBytes-len(prefix)), skipNone},
			{"one byte over", prefix + strings.Repeat("a", maxListFieldBytes-len(prefix)+1), skipOversized},
			{"the cap counts raw bytes, not trimmed ones", prefix + strings.Repeat("a", maxListFieldBytes-len(prefix)) + " ", skipOversized},
			{"only separators over the cap", strings.Repeat(",", maxListFieldBytes+1), skipOversized},
			{"only separators within the cap", strings.Repeat(",", maxListFieldBytes), skipNone},
			{"a 4 MiB field", strings.Repeat("a,", 2<<20), skipOversized},
			{"the cap counts bytes: 8,192 two-byte runes fit", prefix + strings.Repeat("é", (maxListFieldBytes-len(prefix))/2), skipNone},
			{"the cap counts bytes: 8,193 two-byte runes do not", prefix + strings.Repeat("é", (maxListFieldBytes-len(prefix))/2+1), skipOversized},
		}
		for _, tt := range tests {
			t.Run(lc.name+"/"+tt.name, func(t *testing.T) {
				got, reason := recStd(t).decode([]byte(lc.line(tt.in)))

				if reason != tt.want {
					t.Fatalf("reason = %v, want %v", reason, tt.want)
				}
				if reason == skipOversized && got.code != "" {
					t.Errorf("skipped record = %+v, want the zero record", got)
				}
			})
		}
	}
}

func TestDecode_OversizedIsCheckedAfterFieldCountAndCodeBeforeName(t *testing.T) {
	h := recStd(t)
	bigName := strings.Repeat("n", maxNameBytes+1)
	bigList := strings.Repeat("a", maxListFieldBytes+1)
	tests := []struct {
		name string
		line string
		want skipReason
	}{
		{"oversized name and invalid code", recFields("", bigName, "B", "en:c", "en:us", "a"), skipInvalidCode},
		{"oversized name and a control in the code", recFields("1\x002", bigName, "B", "en:c", "en:us", "a"), skipInvalidCode},
		{"oversized brands and invalid code", recFields("\xff", "N", bigList, "en:c", "en:us", "a"), skipInvalidCode},
		{"oversized categories and a long code", recFields(strings.Repeat("1", 129), "N", "B", bigList, "en:us", "a"), skipInvalidCode},
		{"oversized countries and invalid code", recFields(" ", "N", "B", "en:c", bigList, "a"), skipInvalidCode},
		{"oversized name and the wrong field count", recJoin("1", bigName, "B"), skipMalformed},
		{"oversized name and one field more", recFields("1", bigName, "B", "en:c", "en:us", "a") + "\tx", skipMalformed},
		{"oversized brands and an empty name", recFields("1", "", bigList, "en:c", "en:us", "a"), skipOversized},
		{"oversized countries and a blank name", recFields("1", "  ", "B", "en:c", bigList, "a"), skipOversized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := h.decode([]byte(tt.line)); got != tt.want {
				t.Errorf("reason = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecode_RowAfterAnOversizedRowDecodesNormally(t *testing.T) {
	h := recStd(t)
	big := recFields("1", strings.Repeat("n", 4*maxNameBytes), "B", "en:c", "en:us", "a")
	next := recFields("2", "Next", "B2", "en:c2", "en:ca", "b")

	if _, r := h.decode([]byte(big)); r != skipOversized {
		t.Fatalf("big row: reason = %v, want oversized", r)
	}
	got, r := h.decode([]byte(next))

	if r != skipNone || got.code != "2" || got.name != "Next" {
		t.Errorf("next row = %+v, %v", got, r)
	}
}

// An oversized row must cost nothing: the check runs on the raw field lengths,
// before any copy, trim or split, so a huge (16 MiB, far over the 1 MiB line limit) row is skipped with no
// allocation at all (the line reader's buffer is the only memory in play).
func TestDecode_OversizedRowAllocatesNothing(t *testing.T) {
	h := recStd(t)
	const lineBytes = 16 << 20
	lines := []struct {
		name  string
		build func() string
	}{
		{"name", func() string { return recFields("1", strings.Repeat("x", lineBytes-100), "B", "en:c", "en:us", "a") }},
		{"brands", func() string { return recFields("1", "N", strings.Repeat("x", lineBytes-100), "en:c", "en:us", "a") }},
		{"categories_tags", func() string { return recFields("1", "N", "B", strings.Repeat("x", lineBytes-100), "en:us", "a") }},
		{"countries_tags", func() string { return recFields("1", "N", "B", "en:c", strings.Repeat("x", lineBytes-100), "a") }},
		{"many commas", func() string {
			return recFields("1", "N", strings.Repeat(",a", (lineBytes-100)/2), "en:c", "en:us", "a")
		}},
	}
	for _, tc := range lines {
		t.Run(tc.name, func(t *testing.T) {
			b := []byte(tc.build())
			if _, r := h.decode(b); r != skipOversized {
				t.Fatalf("reason = %v, want oversized", r)
			}

			allocs := testing.AllocsPerRun(5, func() { _, _ = h.decode(b) })

			if allocs != 0 {
				t.Errorf("decode of a %d byte oversized line allocates %v times, want 0", len(b), allocs)
			}
		})
	}
}

// The largest record that gets through the caps, with every byte a control
// character that JSON writes as a six-byte escape, must stay far below the
// 2 MiB batch cap of the pipeline: name 1 KiB and two indexed lists of 16 KiB
// give at most 6 x (1 KiB + 2 x 16 KiB) = 198 KiB, plus the keys and quotes.
func TestRecord_ProductOfTheLargestAcceptedRowIsBounded(t *testing.T) {
	ctl := func(n int) string { return strings.Repeat("\x01", n) }
	line := recFields("1", ctl(maxNameBytes), ctl(maxListFieldBytes), "en:"+ctl(maxListFieldBytes-3), ctl(maxListFieldBytes), "a")

	got := recOK(t, line)
	body, err := json.Marshal(got.product())

	if err != nil {
		t.Fatal(err)
	}
	const wantAtLeast, limit = 190 << 10, 400 << 10
	if len(body) < wantAtLeast {
		t.Errorf("JSON is %d bytes: the fixture no longer reaches the caps (want >= %d)", len(body), wantAtLeast)
	}
	if len(body) >= limit {
		t.Errorf("JSON of the largest accepted record is %d bytes, want < %d", len(body), limit)
	}
}
