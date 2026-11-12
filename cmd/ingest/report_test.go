package main

import (
	"fmt"
	"testing"
)

// repCfg is the default config with sampling off.
func repCfg() config { return filtCfg() }

// repLine is one line of input as the pipeline sees it.
type repLine struct {
	name    string
	tooLong bool
	rec     record
	reason  skipReason // result of decode
	want    skipReason // expected bucket, skipNone = passed
}

func repGoodWith(mut func(r *record)) record {
	r := filtGood()
	mut(&r)
	return r
}

func TestReport_Add_EveryLineLandsInOneBucket(t *testing.T) {
	cfg := repCfg()
	cfg.SampleEvery = 8
	r := newReport(cfg, "x")
	keep := func(mut func(r *record)) record { // a sampled code (mix64(fnv1a64) mod 8 = 0)
		rec := repGoodWith(mut)
		rec.code = "3017620422003"
		return rec
	}
	lines := []repLine{
		{name: "too long", tooLong: true, want: skipTooLong},
		{name: "too long ignores decode result", tooLong: true, reason: skipMalformed, want: skipTooLong},
		{name: "malformed", reason: skipMalformed, want: skipMalformed},
		{name: "invalid code", reason: skipInvalidCode, want: skipInvalidCode},
		{name: "oversized", reason: skipOversized, want: skipOversized},
		{name: "no name", reason: skipNoName, want: skipNoName},
		{name: "decode reason ignores the record", reason: skipMalformed, rec: keep(func(*record) {}), want: skipMalformed},
		{name: "not sampled", rec: repGoodWith(func(r *record) { r.code = "code-1" }), want: skipNotSampled},
		{name: "country", rec: keep(func(r *record) { r.countries = nil }), want: skipCountry},
		{name: "brand", rec: keep(func(r *record) { r.brands = nil }), want: skipBrand},
		{name: "category", rec: keep(func(r *record) { r.categories = nil }), want: skipCategory},
		{name: "nutriscore", rec: keep(func(r *record) { r.nutriscore = "" }), want: skipNutriscore},
		{name: "passed", rec: keep(func(*record) {}), want: skipNone},
		{name: "passed again", rec: keep(func(*record) {}), want: skipNone},
	}
	wantSkipped := [numSkipReasons]int{}
	wantPassed := 0
	for i, l := range lines {
		got := r.add(l.tooLong, l.rec, l.reason)
		if got != (l.want == skipNone) {
			t.Errorf("%s: add returned %v, want %v", l.name, got, l.want == skipNone)
		}
		if l.want == skipNone {
			wantPassed++
		} else {
			wantSkipped[l.want]++
		}
		if r.rowsRead != i+1 {
			t.Errorf("%s: rowsRead = %d, want %d", l.name, r.rowsRead, i+1)
		}
		if r.skipped != wantSkipped {
			t.Errorf("%s: skipped = %v, want %v", l.name, r.skipped, wantSkipped)
		}
		if r.passed != wantPassed {
			t.Errorf("%s: passed = %d, want %d", l.name, r.passed, wantPassed)
		}
		if err := r.check(); err != nil {
			t.Errorf("%s: check() = %v", l.name, err)
		}
	}
	// valid = rows that decoded and were sampled: 4 filter buckets + 2 passed.
	if r.valid != 6 {
		t.Errorf("valid = %d, want 6", r.valid)
	}
}

// The independent pass counts differ from the sequential buckets: a row
// rejected as "country" still counts as a brand, category and nutriscore pass.
func TestReport_Add_IndependentCountsDifferFromBuckets(t *testing.T) {
	r := newReport(repCfg(), "x")
	r.add(false, repGoodWith(func(r *record) { r.countries = []string{"en:france"} }), skipNone) // country only
	r.add(false, repGoodWith(func(r *record) { r.countries = nil; r.brands = nil }), skipNone)   // country + brand
	r.add(false, repGoodWith(func(r *record) { r.nutriscore = "" }), skipNone)                   // nutriscore only
	r.add(false, filtGood(), skipNone)                                                           // passes

	if r.valid != 4 {
		t.Fatalf("valid = %d, want 4", r.valid)
	}
	wantBuckets := map[skipReason]int{skipCountry: 2, skipNutriscore: 1, skipBrand: 0, skipCategory: 0}
	for reason, n := range wantBuckets {
		if r.skipped[reason] != n {
			t.Errorf("bucket %v = %d, want %d", reason, r.skipped[reason], n)
		}
	}
	// Independent: country passes 2 (rows 3, 4), brand 3 (rows 1, 3, 4),
	// category 4, nutriscore 3 (rows 1, 2, 4).
	want := [numFilters]int{filterCountry: 2, filterBrand: 3, filterCategory: 4, filterNutriscore: 3}
	if r.passCount != want {
		t.Errorf("passCount = %v, want %v", r.passCount, want)
	}
	if r.passed != 1 {
		t.Errorf("passed = %d, want 1", r.passed)
	}
}

// Unsampled rows and decode failures never reach the independent counts.
func TestReport_Add_PassCountsOnlyOverValidSampledRows(t *testing.T) {
	cfg := repCfg()
	cfg.SampleEvery = 8
	r := newReport(cfg, "x")
	r.add(false, repGoodWith(func(r *record) { r.code = "code-1" }), skipNone) // unsampled, would pass everything
	r.add(true, record{}, skipNone)
	r.add(false, filtGood(), skipMalformed)
	if r.passCount != [numFilters]int{} || r.valid != 0 {
		t.Errorf("passCount = %v valid = %d, want zeros", r.passCount, r.valid)
	}
}

func TestReport_Add_DisabledFiltersCountNothing(t *testing.T) {
	r := newReport(config{SampleEvery: 1, Require: []string{"brand"}}, "x")
	r.add(false, filtGood(), skipNone)
	want := [numFilters]int{filterBrand: 1}
	if r.passCount != want {
		t.Errorf("passCount = %v, want %v", r.passCount, want)
	}
	// Everything off: a bare row passes and nothing is counted per filter.
	r = newReport(config{SampleEvery: 1}, "x")
	if !r.add(false, record{code: "1", name: "n"}, skipNone) {
		t.Error("bare row did not pass with every filter off")
	}
	if r.passCount != [numFilters]int{} || r.valid != 1 || r.passed != 1 {
		t.Errorf("passCount = %v valid = %d passed = %d", r.passCount, r.valid, r.passed)
	}
}

func TestNewReport_Defaults(t *testing.T) {
	cfg := repCfg()
	cfg.DryRun = true
	cfg.Limit = 77
	cfg.SampleEvery = 5
	r := newReport(cfg, "https://h/p")
	if !r.dryRun || r.limit != 77 || r.sampleEvery != 5 || r.input != "https://h/p" {
		t.Errorf("newReport = %+v", r)
	}
	if r.bytesTotal != -1 {
		t.Errorf("bytesTotal = %d, want -1 (unknown) until set", r.bytesTotal)
	}
	if r.hasDocCount {
		t.Error("hasDocCount is true on a new report")
	}
}

func TestReport_Check_Invariants(t *testing.T) {
	good := func() *report {
		r := newReport(repCfg(), "x")
		r.rowsRead = 20
		r.skipped[skipTooLong] = 1
		r.skipped[skipMalformed] = 2
		r.skipped[skipInvalidCode] = 3
		r.skipped[skipOversized] = 1
		r.skipped[skipNoName] = 2
		r.skipped[skipNotSampled] = 4
		r.skipped[skipCountry] = 1
		r.skipped[skipBrand] = 1
		r.skipped[skipCategory] = 1
		r.skipped[skipNutriscore] = 1
		r.valid = 4 + 3 // 4 filter buckets + 3 passed
		r.passed = 3
		r.indexed, r.failed = 2, 1
		return r
	}
	if err := good().check(); err != nil {
		t.Fatalf("consistent report: check() = %v", err)
	}
	g := good()
	g.complete = true
	if err := g.check(); err != nil {
		t.Fatalf("consistent complete report: check() = %v", err)
	}

	rows := func(rowsRead, sum int) string {
		return fmt.Sprintf("invariant broken: rows_read=%d but skip buckets + passed = %d", rowsRead, sum)
	}
	tests := []struct {
		name string
		mut  func(r *report)
		want string // the exact error text
	}{
		{"rows_read too high", func(r *report) { r.rowsRead++ }, rows(21, 20)},
		{"rows_read too low", func(r *report) { r.rowsRead-- }, rows(19, 20)},
		{"passed too high", func(r *report) { r.passed++ }, rows(20, 21)},
		{"too_long bucket lost", func(r *report) { r.skipped[skipTooLong] = 0 }, rows(20, 19)},
		{"malformed bucket lost", func(r *report) { r.skipped[skipMalformed] = 0 }, rows(20, 18)},
		{"invalid_code bucket lost", func(r *report) { r.skipped[skipInvalidCode] = 0 }, rows(20, 17)},
		{"oversized bucket lost", func(r *report) { r.skipped[skipOversized] = 0 }, rows(20, 19)},
		{"no_name bucket lost", func(r *report) { r.skipped[skipNoName] = 0 }, rows(20, 18)},
		{"not_sampled bucket lost", func(r *report) { r.skipped[skipNotSampled] = 0 }, rows(20, 16)},
		{"country bucket lost", func(r *report) { r.skipped[skipCountry] = 0 }, rows(20, 19)},
		{"brand bucket lost", func(r *report) { r.skipped[skipBrand] = 0 }, rows(20, 19)},
		{"category bucket lost", func(r *report) { r.skipped[skipCategory] = 0 }, rows(20, 19)},
		{"nutriscore bucket lost", func(r *report) { r.skipped[skipNutriscore] = 0 }, rows(20, 19)},
		{"valid too high", func(r *report) { r.valid++ }, "invariant broken: valid=8 but passed + filter buckets = 7"},
		{"valid too low", func(r *report) { r.valid-- }, "invariant broken: valid=6 but passed + filter buckets = 7"},
		{"indexed + failed above passed", func(r *report) { r.complete = true; r.indexed++ }, "invariant broken: passed=3 but indexed=3 + failed=1 = 4"},
		{"indexed + failed below passed", func(r *report) { r.complete = true; r.failed-- }, "invariant broken: passed=3 but indexed=2 + failed=0 = 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := good()
			tt.mut(r)
			err := r.check()
			if err == nil {
				t.Fatal("check() = nil on a broken report")
			}
			if err.Error() != tt.want {
				t.Errorf("check() = %q, want %q", err, tt.want)
			}
		})
	}
}

// passed == indexed + failed only holds on a normal completion of an ingest.
func TestReport_Check_IngestInvariantOnlyWhenCompleteAndNotDryRun(t *testing.T) {
	r := newReport(repCfg(), "x")
	r.rowsRead, r.passed, r.valid = 5, 5, 5
	r.indexed, r.failed = 1, 1
	if err := r.check(); err != nil { // interrupted run: not complete
		t.Errorf("incomplete run: check() = %v", err)
	}
	r.complete = true
	if err := r.check(); err == nil {
		t.Error("complete ingest with passed != indexed + failed: check() = nil")
	}
	r.dryRun = true
	r.indexed, r.failed = 0, 0
	if err := r.check(); err != nil { // a dry run indexes nothing
		t.Errorf("complete dry run: check() = %v", err)
	}
}

func TestReport_ZeroValue(t *testing.T) {
	var r report
	if err := r.check(); err != nil {
		t.Errorf("zero report: check() = %v", err)
	}
	if !r.add(false, record{code: "x"}, skipNone) { // zero filters: sampling and filters all off
		t.Error("zero report: a plain row did not pass")
	}
	if r.rowsRead != 1 || r.passed != 1 || r.valid != 1 {
		t.Errorf("zero report after one row: %+v", r)
	}
}

// Fixture rows (testdata/products.tsv), counted by hand: 15 rows. The short
// row is malformed, the row without a code is invalid_code; four rows are
// outside the United States (country); two have no English category
// (category); seven pass.
func TestReport_Fixture_UnitedStatesSampleEveryOne(t *testing.T) {
	cfg, err := parseConfig([]string{"--countries", "en:united-states", "--sample-every", "1"})
	if err != nil {
		t.Fatal(err)
	}
	r := repFixture(t, cfg)
	want := map[skipReason]int{
		skipTooLong: 0, skipMalformed: 1, skipInvalidCode: 1, skipOversized: 0, skipNoName: 0,
		skipNotSampled: 0, skipCountry: 4, skipBrand: 0, skipCategory: 2, skipNutriscore: 0,
	}
	for reason, n := range want {
		if r.skipped[reason] != n {
			t.Errorf("bucket %v = %d, want %d", reason, r.skipped[reason], n)
		}
	}
	if r.rowsRead != 15 || r.passed != 7 || r.valid != 13 {
		t.Errorf("rows_read=%d passed=%d valid=%d, want 15 7 13", r.rowsRead, r.passed, r.valid)
	}
	// Independent over the 13 valid rows.
	wantPass := [numFilters]int{filterCountry: 9, filterBrand: 13, filterCategory: 10, filterNutriscore: 10}
	if r.passCount != wantPass {
		t.Errorf("passCount = %v, want %v", r.passCount, wantPass)
	}
	if err := r.check(); err != nil {
		t.Errorf("check() = %v", err)
	}
}

func TestReport_Fixture_AllFiltersOff(t *testing.T) {
	cfg, err := parseConfig([]string{"--countries", "", "--require", "", "--sample-every", "1"})
	if err != nil {
		t.Fatal(err)
	}
	r := repFixture(t, cfg)
	if r.passed != 13 || r.valid != 13 || r.rowsRead != 15 {
		t.Errorf("passed=%d valid=%d rows_read=%d, want 13 13 15", r.passed, r.valid, r.rowsRead)
	}
	if err := r.check(); err != nil {
		t.Errorf("check() = %v", err)
	}
}

// repFixture runs the fixture through the real decoder and the report.
func repFixture(t *testing.T, cfg config) *report {
	t.Helper()
	lines := recFixtureLines(t)
	h, err := parseHeader(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	r := newReport(cfg, "testdata/products.tsv")
	for _, l := range lines[1:] {
		rec, reason := h.decode(l)
		r.add(false, rec, reason)
	}
	return r
}
