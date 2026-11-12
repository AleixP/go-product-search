package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// recRealHeader and recRealLine imitate the real export: 211 columns, the
// required ones at their real positions (code 0, product_name 10, brands 18,
// categories_tags 22, countries_tags 40, nutriscore_grade 58) and about 3 KB
// per row.
func recRealHeader() []byte {
	cols := make([]string, 211)
	for i := range cols {
		cols[i] = fmt.Sprintf("col_%03d", i)
	}
	cols[0], cols[10], cols[18], cols[22], cols[40], cols[58] =
		"code", "product_name", "brands", "categories_tags", "countries_tags", "nutriscore_grade"
	return []byte(recJoin(cols...))
}

func recRealLine() []byte {
	fields := make([]string, 211)
	for i := range fields {
		fields[i] = strings.Repeat("v", 14) // 211 x 14 + separators is about 3.1 KB
	}
	fields[0] = "3017620422003"
	fields[10] = "Nutella Hazelnut Spread"
	fields[18] = "Ferrero, Nutella"
	fields[22] = "en:breakfasts,en:spreads,en:sweet-spreads,en:hazelnut-spreads,en:chocolate-spreads,en:cocoa-and-hazelnuts-spreads,fr:pates-a-tartiner,en:nutella"
	fields[40] = "en:united-states"
	fields[58] = "e"
	return []byte(recJoin(fields...))
}

func TestDecode_RealWidthLineIsDecodedCorrectly(t *testing.T) {
	h, err := parseHeader(recRealHeader())
	if err != nil {
		t.Fatal(err)
	}
	line := recRealLine()
	if len(line) < 3000 || len(line) > 3500 || h.width != 211 {
		t.Fatalf("line is %d bytes, header width %d; want about 3.1 KB and 211", len(line), h.width)
	}

	got, reason := h.decode(line)

	if reason != skipNone || got.code != "3017620422003" || got.nutriscore != "e" {
		t.Fatalf("decode = %+v, %v", got, reason)
	}
	recWantList(t, "brands", got.brands, []string{"Ferrero", "Nutella"})
	recWantList(t, "categories", got.categories, []string{"breakfasts", "spreads", "sweet-spreads", "hazelnut-spreads", "chocolate-spreads", "cocoa-and-hazelnuts-spreads", "nutella"})
	recWantList(t, "countries", got.countries, []string{"en:united-states"})
}

// The only allocations of a decode are the copies it returns: a string for the
// code and one for the name, a slice and one string per kept item for each of
// the three lists (the one-letter grade is free). For the line above that is
// 1 + 1 + (1+2) + (1+7) + (1+1) = 15; nothing is allocated per column, for
// dropped items or for temporary slices.
func TestDecode_AllocatesOnlyTheRecord(t *testing.T) {
	h, err := parseHeader(recRealHeader())
	if err != nil {
		t.Fatal(err)
	}
	line := recRealLine()

	allocs := testing.AllocsPerRun(200, func() { _, _ = h.decode(line) })

	const maxAllocs = 15
	if allocs > maxAllocs {
		t.Errorf("decode allocates %v times per line, want <= %d", allocs, maxAllocs)
	}
}

func TestDecode_AllocatedBytesAreFarBelowTheLineSize(t *testing.T) {
	h, err := parseHeader(recRealHeader())
	if err != nil {
		t.Fatal(err)
	}
	line := recRealLine()
	const runs = 1000
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		_, _ = h.decode(line)
	}
	runtime.ReadMemStats(&after)

	perLine := (after.TotalAlloc - before.TotalAlloc) / runs
	// The returned strings and slices need about 0.5 KB; 211 string headers
	// alone would be 3.4 KB.
	const maxBytes = 1024
	if perLine > maxBytes {
		t.Errorf("decode allocates %d bytes per line, want <= %d (the line is %d bytes)", perLine, maxBytes, len(line))
	}
}

func BenchmarkDecode211Columns(b *testing.B) {
	h, err := parseHeader(recRealHeader())
	if err != nil {
		b.Fatal(err)
	}
	line := recRealLine()
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, reason := h.decode(line); reason != skipNone {
			b.Fatal(reason)
		}
	}
}
