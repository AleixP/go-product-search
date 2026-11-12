package main

import (
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Bounds on what the writer prints, so the report stays small whatever the
// failures look like. Item 3 already caps the failure types at 33 and the
// failed ids at 10.
const (
	maxFailedIDs    = 10
	maxFailureTypes = 64
	maxTextBytes    = 1024 // one free-text value, before quoting
	maxEstimate     = 1 << 62
)

// plainBuckets are the skip buckets that are always printed, in order. The
// filter buckets (country, brand, category, nutriscore) follow, only for the
// enabled filters.
var plainBuckets = [...]skipReason{skipTooLong, skipMalformed, skipInvalidCode, skipOversized, skipNoName, skipNotSampled}

// write prints the report as one "key=value" line per key, in this fixed
// order (a missing line is "not applicable", never reordered):
//
//	input                   already redacted by the caller
//	bytes_read              compressed bytes pulled from the source
//	bytes_total             size of the source, or "unknown" (negative)
//	coverage_pct            only when bytes_total > 0; see below
//	rows_read
//	too_long malformed invalid_code oversized no_name not_sampled
//	country brand category nutriscore   skip buckets, only the enabled filters
//	valid                   rows that decoded and were sampled (documented extra
//	                        key: the denominator of the pass counts)
//	country_pass brand_pass category_pass nutriscore_pass   only the enabled filters
//	passed
//	limit_reached
//	indexed
//	failed
//	failure_types           name:count sorted by name, only when there are some
//	failed_ids              in order of occurrence, at most 10, only when any
//	doc_count               only when known
//	duplicates_overwritten  indexed - doc_count, only when doc_count is known
//	index                   only when set
//	published
//	dry_run                 true or false
//	estimated_passed_full_input_k1, suggested_sample_every   see estimate
//
// This is the order of the accepted proposal's key list; there is no "mode"
// key, dry_run says it. It is pinned by golden tests.
//
// Free text (input, index, failure types, ids) is written as is when it is
// made of [A-Za-z0-9_.:/@+-] only, and as a Go quoted string otherwise, cut at
// 1 KiB first: a line never holds a newline, tab, carriage return or any other
// control character, and a quoted value is unambiguous.
//
// coverage_pct is bytes_read / bytes_total * 100 with one decimal, at most
// 100.0. It is approximate: bytes_read counts the compressed bytes pulled from
// the network or the file, which run ahead of the decoded position by up to
// 64 KiB (the reader's buffer), and gzip hides the exact position.
//
// The whole text is built in memory (a few KB at most, bounded above) and
// written with one Write, so the first write error is the one returned.
func (r *report) write(w io.Writer) error {
	var b strings.Builder
	line := func(key, value string) { b.WriteString(key + "=" + value + "\n") }
	num := func(key string, n int) { line(key, strconv.Itoa(n)) }

	line("input", text(r.input))
	line("bytes_read", strconv.FormatInt(r.bytesRead, 10))
	if r.bytesTotal < 0 {
		line("bytes_total", "unknown")
	} else {
		line("bytes_total", strconv.FormatInt(r.bytesTotal, 10))
	}
	if r.bytesTotal > 0 {
		pct := min(100.0, float64(r.bytesRead)*100/float64(r.bytesTotal))
		line("coverage_pct", strconv.FormatFloat(pct, 'f', 1, 64))
	}
	num("rows_read", r.rowsRead)
	for _, reason := range plainBuckets {
		num(reason.String(), r.skipped[reason])
	}
	for i, reason := range filterReasons {
		if r.filt.enabled(i) {
			num(reason.String(), r.skipped[reason])
		}
	}
	num("valid", r.valid)
	for i, reason := range filterReasons {
		if r.filt.enabled(i) {
			num(reason.String()+"_pass", r.passCount[i])
		}
	}
	num("passed", r.passed)
	line("limit_reached", strconv.FormatBool(r.limitReached))
	num("indexed", r.indexed)
	num("failed", r.failed)
	if len(r.failureTypes) > 0 {
		names := make([]string, 0, len(r.failureTypes))
		for name := range r.failureTypes {
			names = append(names, name)
		}
		slices.Sort(names)
		names = names[:min(len(names), maxFailureTypes)]
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = text(name) + ":" + strconv.Itoa(r.failureTypes[name])
		}
		line("failure_types", strings.Join(parts, ","))
	}
	if len(r.failedIDs) > 0 {
		ids := r.failedIDs[:min(len(r.failedIDs), maxFailedIDs)]
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = text(id)
		}
		line("failed_ids", strings.Join(parts, ","))
	}
	if r.hasDocCount {
		num("doc_count", r.docCount)
		num("duplicates_overwritten", r.indexed-r.docCount)
	}
	if r.index != "" {
		line("index", text(r.index))
	}
	line("published", strconv.FormatBool(r.published))
	line("dry_run", strconv.FormatBool(r.dryRun))
	if est, suggested, ok := r.estimate(); ok {
		line("estimated_passed_full_input_k1", strconv.FormatInt(est, 10))
		line("suggested_sample_every", strconv.FormatInt(suggested, 10))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// estimate projects the run over the whole input. A hash sampler keeps about
// 1 row in k, so the rows that would pass with k = 1 are about passed * k; the
// run only saw the fraction c = bytes_read / bytes_total of the input (capped
// at 1), so the estimate for the full input is passed * k / c, rounded. The
// suggested --sample-every is the k that would fill --limit exactly:
// max(1, floor(estimate / limit)). It is rough: rows are not uniform in size,
// and bytes_read runs ahead of the decoded position (see write). It needs a
// known size, some bytes read and a limit; otherwise ok is false.
func (r *report) estimate() (est, suggested int64, ok bool) {
	if r.bytesTotal <= 0 || r.bytesRead <= 0 || r.limit <= 0 {
		return 0, 0, false
	}
	read := min(r.bytesRead, r.bytesTotal)
	k := max(r.sampleEvery, 1)
	e := math.Round(float64(r.passed) * float64(k) * float64(r.bytesTotal) / float64(read))
	est = maxEstimate // saturate: float to int conversion of a huge value is undefined
	if e < maxEstimate {
		est = int64(e)
	}
	return est, max(1, est/int64(r.limit)), true
}

// text returns s as a report value: as is when it is plain, a Go quoted string
// otherwise (control characters, spaces, '=', ',', quotes, invalid UTF-8,
// empty). It is cut at maxTextBytes before.
func text(s string) string {
	if len(s) > maxTextBytes {
		s = s[:maxTextBytes]
	}
	if s == "" || strings.IndexFunc(s, notPlain) >= 0 {
		return strconv.Quote(s)
	}
	return s
}

// notPlain reports whether r is outside the plain set [A-Za-z0-9_.:/@+-], that
// is, whether a value holding it must be quoted.
func notPlain(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("_.:/@+-", r)
}
