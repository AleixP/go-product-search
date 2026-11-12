package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// repText writes the report and returns the text, failing on an error.
func repText(t *testing.T, r *report) string {
	t.Helper()
	var buf bytes.Buffer
	if err := r.write(&buf); err != nil {
		t.Fatalf("write error = %v", err)
	}
	return buf.String()
}

// repValue returns the value of the first line "key=...", and whether the key
// is there.
func repValue(text, key string) (string, bool) {
	for _, l := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// repEstimate builds the report that feeds the estimate.
func repEstimate(passed, k int, read, total int64, limit int) *report {
	return &report{passed: passed, sampleEvery: k, bytesRead: read, bytesTotal: total, limit: limit}
}

// repIngestFull is a complete, consistent ingest report with every filter on.
func repIngestFull() *report {
	cfg := repCfg()
	cfg.SampleEvery = 8
	cfg.Limit = 100000
	r := newReport(cfg, "https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz")
	r.bytesRead, r.bytesTotal = 400_000_000, 1_600_000_000
	r.rowsRead, r.passed, r.limitReached, r.complete = 800045, 100000, true, true
	r.skipped = [numSkipReasons]int{
		skipTooLong: 1, skipMalformed: 2, skipInvalidCode: 3, skipOversized: 4, skipNoName: 5,
		skipNotSampled: 700000, skipCountry: 6, skipBrand: 7, skipCategory: 8, skipNutriscore: 9,
	}
	r.valid = 100030
	r.passCount = [numFilters]int{filterCountry: 100024, filterBrand: 100023, filterCategory: 100022, filterNutriscore: 100021}
	r.indexed, r.failed = 99950, 50
	r.failureTypes = map[string]int{"version_conflict": 20, "mapper_parsing_exception": 30}
	r.failedIDs = []string{"0001", "0002"}
	r.docCount, r.hasDocCount = 99940, true
	r.index, r.published = "products-20261111t120000z", true
	return r
}

// The order of the keys is part of the contract: scripts and the README read
// the report.
const repGoldenIngest = `input=https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz
bytes_read=400000000
bytes_total=1600000000
coverage_pct=25.0
rows_read=800045
too_long=1
malformed=2
invalid_code=3
oversized=4
no_name=5
not_sampled=700000
country=6
brand=7
category=8
nutriscore=9
valid=100030
country_pass=100024
brand_pass=100023
category_pass=100022
nutriscore_pass=100021
passed=100000
limit_reached=true
indexed=99950
failed=50
failure_types=mapper_parsing_exception:30,version_conflict:20
failed_ids=0001,0002
doc_count=99940
duplicates_overwritten=10
index=products-20261111t120000z
published=true
dry_run=false
estimated_passed_full_input_k1=3200000
suggested_sample_every=32
`

const repGoldenDryRun = `input=testdata/products.tsv
bytes_read=1200
bytes_total=1200
coverage_pct=100.0
rows_read=15
too_long=0
malformed=1
invalid_code=1
oversized=0
no_name=0
not_sampled=0
brand=0
nutriscore=3
valid=13
brand_pass=13
nutriscore_pass=10
passed=10
limit_reached=false
indexed=0
failed=0
published=false
dry_run=true
`

func TestReport_Write_GoldenIngest(t *testing.T) {
	r := repIngestFull()
	if err := r.check(); err != nil {
		t.Fatalf("golden report is inconsistent: %v", err)
	}
	if got := repText(t, r); got != repGoldenIngest {
		t.Errorf("report:\n%s\nwant:\n%s", got, repGoldenIngest)
	}
}

func TestReport_Write_GoldenDryRun(t *testing.T) {
	cfg, err := parseConfig([]string{"--dry-run", "--countries", "", "--require", "brand,nutriscore", "--limit", "0", "--sample-every", "1"})
	if err != nil {
		t.Fatal(err)
	}
	r := repFixture(t, cfg)
	r.bytesRead, r.bytesTotal, r.complete = 1200, 1200, true
	if err := r.check(); err != nil {
		t.Fatalf("check() = %v", err)
	}
	if got := repText(t, r); got != repGoldenDryRun {
		t.Errorf("report:\n%s\nwant:\n%s", got, repGoldenDryRun)
	}
}

func TestReport_Write_FilterLinesOnlyWhenEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
		on   []string
	}{
		{"none", config{SampleEvery: 1}, nil},
		{"country", config{SampleEvery: 1, Countries: []string{"en:x"}}, []string{"country"}},
		{"brand", config{SampleEvery: 1, Require: []string{"brand"}}, []string{"brand"}},
		{"category", config{SampleEvery: 1, Require: []string{"category"}}, []string{"category"}},
		{"nutriscore", config{SampleEvery: 1, Require: []string{"nutriscore"}}, []string{"nutriscore"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := repText(t, newReport(tt.cfg, "x"))
			for _, f := range []string{"country", "brand", "category", "nutriscore"} {
				on := false
				for _, w := range tt.on {
					on = on || w == f
				}
				if _, ok := repValue(text, f); ok != on {
					t.Errorf("bucket line %q present = %v, want %v", f, ok, on)
				}
				if _, ok := repValue(text, f+"_pass"); ok != on {
					t.Errorf("pass line %q present = %v, want %v", f+"_pass", ok, on)
				}
			}
			for _, f := range []string{"too_long", "malformed", "invalid_code", "oversized", "no_name", "not_sampled", "valid"} {
				if _, ok := repValue(text, f); !ok {
					t.Errorf("line %q missing: it is always printed", f)
				}
			}
		})
	}
}

// The run mode is the dry_run key (the proposal's name), not a "mode" key.
func TestReport_Write_NoModeKey(t *testing.T) {
	for _, dry := range []bool{true, false} {
		if _, ok := repValue(repText(t, &report{dryRun: dry}), "mode"); ok {
			t.Errorf("dryRun=%v: report prints a mode key", dry)
		}
	}
}

func TestReport_Write_ModeAndFlags(t *testing.T) {
	r := &report{dryRun: true, limitReached: true, published: true}
	text := repText(t, r)
	for k, want := range map[string]string{"dry_run": "true", "limit_reached": "true", "published": "true"} {
		if got, _ := repValue(text, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// The two flags are independent.
	text = repText(t, &report{limitReached: true})
	if lr, _ := repValue(text, "limit_reached"); lr != "true" {
		t.Errorf("limit_reached = %q, want true", lr)
	}
	if pub, _ := repValue(text, "published"); pub != "false" {
		t.Errorf("published = %q, want false", pub)
	}
	text = repText(t, &report{published: true})
	if lr, _ := repValue(text, "limit_reached"); lr != "false" {
		t.Errorf("limit_reached = %q, want false", lr)
	}
	if pub, _ := repValue(text, "published"); pub != "true" {
		t.Errorf("published = %q, want true", pub)
	}
	text = repText(t, &report{})
	for k, want := range map[string]string{"dry_run": "false", "limit_reached": "false", "published": "false"} {
		if got, _ := repValue(text, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestReport_Write_Coverage(t *testing.T) {
	tests := []struct {
		name        string
		read, total int64
		want        string // "" = no coverage_pct line
		wantTotal   string
	}{
		{"quarter", 1, 4, "25.0", "4"},
		{"third", 1, 3, "33.3", "3"},
		{"two thirds rounds up", 2, 3, "66.7", "3"},
		{"eighth", 1, 8, "12.5", "8"},
		{"full", 5, 5, "100.0", "5"},
		{"read ahead is capped", 6, 5, "100.0", "5"},
		{"nothing read yet", 0, 100, "0.0", "100"},
		{"unknown total", 100, -1, "", "unknown"},
		{"empty file", 0, 0, "", "0"},
		{"empty file with bytes", 7, 0, "", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := repText(t, &report{bytesRead: tt.read, bytesTotal: tt.total})
			got, ok := repValue(text, "coverage_pct")
			if ok != (tt.want != "") || got != tt.want {
				t.Errorf("coverage_pct = %q (present %v), want %q", got, ok, tt.want)
			}
			if got, _ := repValue(text, "bytes_total"); got != tt.wantTotal {
				t.Errorf("bytes_total = %q, want %q", got, tt.wantTotal)
			}
			if got, _ := repValue(text, "bytes_read"); got != strconv.FormatInt(tt.read, 10) {
				t.Errorf("bytes_read = %q, want %d", got, tt.read)
			}
		})
	}
}

func TestReport_Write_Estimate(t *testing.T) {
	tests := []struct {
		name         string
		r            *report
		est, suggest string // "" = line not printed
	}{
		{"documented example", repEstimate(100000, 8, 25, 100, 100000), "3200000", "32"},
		{"floor not ceil", repEstimate(799999, 4, 10, 10, 100000), "3199996", "31"},
		{"never below one", repEstimate(10, 1, 5, 5, 100), "10", "1"},
		{"no rows passed", repEstimate(0, 8, 5, 10, 100), "0", "1"},
		{"coverage divides", repEstimate(100, 2, 1, 3, 10), "600", "60"},
		{"estimate rounds up at half", repEstimate(1, 1, 2, 3, 1), "2", "2"},
		{"estimate rounds down below half", repEstimate(1, 1, 5, 7, 1), "1", "1"},
		{"read ahead capped at full coverage", repEstimate(100, 3, 12, 10, 10), "300", "30"},
		{"zero sample_every counts as one", repEstimate(100, 0, 1, 2, 10), "200", "20"},
		{"limit 0", repEstimate(100, 8, 1, 4, 0), "", ""},
		{"unknown size", repEstimate(100, 8, 1, -1, 100), "", ""},
		{"zero size", repEstimate(100, 8, 1, 0, 100), "", ""},
		{"nothing read", repEstimate(100, 8, 0, 100, 100), "", ""},
		{"saturates instead of overflowing", repEstimate(1<<40, 1<<30, 1, 1<<40, 7), "4611686018427387904", "658812288346769700"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := repText(t, tt.r)
			est, ok := repValue(text, "estimated_passed_full_input_k1")
			if ok != (tt.est != "") || est != tt.est {
				t.Errorf("estimated_passed_full_input_k1 = %q (present %v), want %q", est, ok, tt.est)
			}
			sug, ok := repValue(text, "suggested_sample_every")
			if ok != (tt.suggest != "") || sug != tt.suggest {
				t.Errorf("suggested_sample_every = %q (present %v), want %q", sug, ok, tt.suggest)
			}
		})
	}
}

func TestReport_Write_DocCountAndDuplicates(t *testing.T) {
	tests := []struct {
		name             string
		indexed, count   int
		known            bool
		wantCount, wantD string
	}{
		{"unknown", 5, 5, false, "", ""},
		{"duplicates", 10, 8, true, "8", "2"},
		{"none", 10, 10, true, "10", "0"},
		{"more than indexed", 5, 8, true, "8", "-3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := repText(t, &report{indexed: tt.indexed, docCount: tt.count, hasDocCount: tt.known})
			c, cok := repValue(text, "doc_count")
			d, dok := repValue(text, "duplicates_overwritten")
			if cok != tt.known || dok != tt.known || c != tt.wantCount || d != tt.wantD {
				t.Errorf("doc_count=%q(%v) duplicates_overwritten=%q(%v), want %q %q (present %v)",
					c, cok, d, dok, tt.wantCount, tt.wantD, tt.known)
			}
		})
	}
}

func TestReport_Write_IndexOnlyWhenSet(t *testing.T) {
	if _, ok := repValue(repText(t, &report{}), "index"); ok {
		t.Error("index line printed for an empty index name")
	}
	if got, _ := repValue(repText(t, &report{index: "products-1"}), "index"); got != "products-1" {
		t.Errorf("index = %q, want products-1", got)
	}
}

func TestReport_Write_FailureTypesSortedAndCapped(t *testing.T) {
	r := &report{failureTypes: map[string]int{"zeta": 1, "alpha": 2, "mid": 3}}
	if got, _ := repValue(repText(t, r), "failure_types"); got != "alpha:2,mid:3,zeta:1" {
		t.Errorf("failure_types = %q, want sorted keys", got)
	}
	if _, ok := repValue(repText(t, &report{failureTypes: map[string]int{}}), "failure_types"); ok {
		t.Error("failure_types printed for an empty map")
	}
	many := map[string]int{}
	for i := 0; i < 100; i++ {
		many[fmt.Sprintf("t%03d", i)] = i
	}
	got, _ := repValue(repText(t, &report{failureTypes: many}), "failure_types")
	parts := strings.Split(got, ",")
	if len(parts) != 64 || parts[0] != "t000:0" || parts[63] != "t063:63" {
		t.Errorf("failure_types has %d entries (first %q), want the first 64 sorted", len(parts), parts[0])
	}
}

func TestReport_Write_FailedIDsInOrderAndCapped(t *testing.T) {
	r := &report{failedIDs: []string{"z", "a", "m"}}
	if got, _ := repValue(repText(t, r), "failed_ids"); got != "z,a,m" {
		t.Errorf("failed_ids = %q, want occurrence order z,a,m", got)
	}
	if _, ok := repValue(repText(t, &report{}), "failed_ids"); ok {
		t.Error("failed_ids printed for no ids")
	}
	var ids []string
	for i := 0; i < 15; i++ {
		ids = append(ids, fmt.Sprintf("id%02d", i))
	}
	got, _ := repValue(repText(t, &report{failedIDs: ids}), "failed_ids")
	if want := strings.Join(ids[:10], ","); got != want {
		t.Errorf("failed_ids = %q, want the first 10: %q", got, want)
	}
}

// Hostile text must never break the one-line-per-key format.
func TestReport_Write_HostileText(t *testing.T) {
	hostile := []string{
		"a\nb", "a\rb", "a\tb", "a\x00b", "a b", "a\u0085b", "a b", "a=b", "a,b", `a"b`, `a\b`,
		"\xff\xfe", "x\x1b[31mred", "", strings.Repeat("é", 5000),
	}
	r := repIngestFull()
	r.input = strings.Join(hostile, "|")
	r.index = hostile[0]
	r.failedIDs = hostile
	r.failureTypes = map[string]int{}
	for _, h := range hostile {
		r.failureTypes[h] = 1
	}
	text := repText(t, r)
	for _, bad := range []string{"\r", "\t", "\x00", " ", "\u0085", "\x1b"} {
		if strings.Contains(text, bad) {
			t.Errorf("report contains a raw %q", bad)
		}
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if want := len(strings.Split(strings.TrimSuffix(repGoldenIngest, "\n"), "\n")); len(lines) != want {
		t.Errorf("hostile report has %d lines, want %d (one per key)", len(lines), want)
	}
	for _, l := range lines {
		k, _, ok := strings.Cut(l, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \"\\,") {
			t.Errorf("line %q is not key=value", l)
		}
	}
	for _, key := range []string{"input", "index", "failed_ids", "failure_types"} {
		if v, _ := repValue(text, key); len(v) > 60000 {
			t.Errorf("%s is %d bytes: not bounded", key, len(v))
		}
	}
}

func TestReport_Write_TextQuoting(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://h/p/a_b-c.csv.gz", "https://h/p/a_b-c.csv.gz"},
		{"data/my file.csv", `"data/my file.csv"`},
		{"a\nb", `"a\nb"`},
		{"a\tb", `"a\tb"`},
		{"a\rb", `"a\rb"`},
		{"a=b", `"a=b"`},
		{"a,b", `"a,b"`},
		{`a"b`, `"a\"b"`},
		{"", `""`},
		{"\xff", `"\xff"`},
		{"azAZ09_.:/@+-", "azAZ09_.:/@+-"}, // every plain character, boundaries included
		{"a b", `"a b"`},
		{"{", `"{"`},   // just after 'z'
		{"`", "\"`\""}, // just before 'a'
		{"[", `"["`},   // just after 'Z'
		{"@", "@"},
		{"A", "A"},
		{":", ":"},
		{"/", "/"},
		{"\x7f", `"\x7f"`},
		{"é", `"é"`},
		{"%", `"%"`},
		{`a\b`, `"a\\b"`},
	}
	for _, tt := range tests {
		got, _ := repValue(repText(t, &report{input: tt.in}), "input")
		if got != tt.want {
			t.Errorf("input %q written as %s, want %s", tt.in, got, tt.want)
		}
	}
	long := strings.Repeat("a", 5000)
	if got, _ := repValue(repText(t, &report{input: long}), "input"); got != long[:1024] {
		t.Errorf("long input written with %d bytes, want 1024", len(got))
	}
}

type repFailWriter struct{ err error }

func (w repFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestReport_Write_ReturnsWriteError(t *testing.T) {
	boom := errors.New("disk full")
	err := repIngestFull().write(repFailWriter{boom})
	if !errors.Is(err, boom) {
		t.Errorf("write error = %v, want %v", err, boom)
	}
}

// A zero-value report is not a started run: Item 9 must build the report with
// newReport, which sets bytesTotal to -1 (unknown). The zero value prints
// bytes_total=0, which is also what an empty source prints.
func TestReport_Write_ZeroValue(t *testing.T) {
	var r report
	text := repText(t, &r)
	if !strings.HasPrefix(text, "input=\"\"\nbytes_read=0\nbytes_total=0\n") {
		t.Errorf("zero report starts with %q", text[:min(60, len(text))])
	}
	for _, key := range []string{"coverage_pct", "estimated_passed_full_input_k1", "suggested_sample_every", "doc_count", "index"} {
		if _, ok := repValue(text, key); ok {
			t.Errorf("zero report prints %s", key)
		}
	}
	if got, _ := repValue(newReportText(t), "bytes_total"); got != "unknown" {
		t.Errorf("newReport bytes_total = %q, want unknown", got)
	}
}

func newReportText(t *testing.T) string {
	t.Helper()
	return repText(t, newReport(repCfg(), "x"))
}
