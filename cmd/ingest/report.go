package main

import "fmt"

// report holds the counters of one run and the facts the writer prints (see
// report_write.go). Item 9 fills the counters through add and the result
// fields directly; Item 10 sets the outcome fields. Item 9 must build it with
// newReport: the zero value is not a started run (its bytes_total is 0, not
// "unknown").
//
// Call protocol of add, once per line read from the line reader:
//
//	line, tooLong, err := lines.next()   // err: stop, the line is not counted
//	var rec record
//	var reason skipReason
//	if !tooLong {
//		rec, reason = hdr.decode(line) // reason is one of the four decode reasons
//	}
//	if rep.add(tooLong, rec, reason) {
//		// the row passed every filter: batch it, count it toward the limit
//	}
//
// add ignores rec and reason for a too-long line, ignores rec when reason is
// not skipNone, and otherwise applies the sampler and the filters. Every line
// lands in exactly one bucket or in passed.
type report struct {
	filt filters

	// Run facts.
	dryRun      bool
	input       string // already redacted by the caller
	limit       int    // --limit, 0 = none
	sampleEvery int    // --sample-every, used by the estimate

	// Progress of the input. bytesTotal is -1 when the size is unknown.
	bytesRead, bytesTotal int64

	// Counters of the lines read. Invariants (see check):
	//   rowsRead == sum(skipped) + passed
	//   valid    == passed + the four filter buckets
	// valid counts the rows that decoded fine AND were sampled: the denominator
	// of the independent pass counts.
	rowsRead  int
	skipped   [numSkipReasons]int // by bucket; skipNone is unused
	valid     int
	passCount [numFilters]int // independent per-filter passes over valid rows
	passed    int

	limitReached bool
	// complete is set by the pipeline when the run ended normally (end of input
	// or limit reached, last batch flushed); only then must passed == indexed +
	// failed hold.
	complete bool

	// Outcome of the indexing (Items 9 and 10).
	indexed, failed int
	failureTypes    map[string]int
	// failedIDs is in order of occurrence. Item 9 must cap the merged list at
	// 10 (maxFailedIDs) as it collects it; the writer caps it again.
	failedIDs   []string
	docCount    int
	hasDocCount bool
	index       string
	published   bool
}

// newReport starts the report of a run. The input is already redacted; the
// size is unknown (-1) until the caller sets bytesTotal.
func newReport(cfg config, input string) *report {
	return &report{
		filt:        newFilters(cfg),
		dryRun:      cfg.DryRun,
		input:       input,
		limit:       cfg.Limit,
		sampleEvery: cfg.SampleEvery,
		bytesTotal:  -1,
	}
}

// add counts one line read and reports whether the row passed every filter. See
// the call protocol on report.
func (r *report) add(tooLong bool, rec record, decoded skipReason) bool {
	r.rowsRead++
	switch {
	case tooLong:
		r.skipped[skipTooLong]++
		return false
	case decoded != skipNone:
		r.skipped[decoded]++
		return false
	}
	reason, pass := r.filt.check(rec)
	if reason == skipNotSampled {
		r.skipped[skipNotSampled]++
		return false
	}
	r.valid++
	for i, ok := range pass {
		if ok {
			r.passCount[i]++
		}
	}
	if reason != skipNone {
		r.skipped[reason]++
		return false
	}
	r.passed++
	return true
}

// check verifies the counting invariants and names the broken one.
func (r *report) check() error {
	sum := r.passed
	for _, n := range r.skipped {
		sum += n
	}
	if r.rowsRead != sum {
		return fmt.Errorf("invariant broken: rows_read=%d but skip buckets + passed = %d", r.rowsRead, sum)
	}
	filtered := r.passed
	for _, reason := range filterReasons {
		filtered += r.skipped[reason]
	}
	if r.valid != filtered {
		return fmt.Errorf("invariant broken: valid=%d but passed + filter buckets = %d", r.valid, filtered)
	}
	if r.complete && !r.dryRun && r.passed != r.indexed+r.failed {
		return fmt.Errorf("invariant broken: passed=%d but indexed=%d + failed=%d = %d",
			r.passed, r.indexed, r.failed, r.indexed+r.failed)
	}
	return nil
}
