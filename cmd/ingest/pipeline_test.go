package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

func TestPipeline_2500RowsGiveBatchesOf1000_1000_500(t *testing.T) {
	plNoLeak(t)
	idx := &plIndexer{}
	rep, err := plRun(context.Background(), t, &plLines{n: 2500}, plCfg(t), idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := idx.sizes(), []int{1000, 1000, 500}; !plEqualInts(got, want) {
		t.Errorf("batch sizes = %v, want %v", got, want)
	}
	// Every id exactly once, in order inside each batch.
	seen := map[string]bool{}
	for _, b := range idx.copies {
		for i, p := range b {
			if seen[p.ID] {
				t.Fatalf("id %s sent twice", p.ID)
			}
			seen[p.ID] = true
			if i > 0 && p.ID <= b[i-1].ID {
				t.Fatalf("batch out of order at %s", p.ID)
			}
		}
	}
	if len(seen) != 2500 {
		t.Errorf("distinct ids = %d, want 2500", len(seen))
	}
	for _, ix := range idx.indices {
		if ix != "products-test" {
			t.Errorf("Bulk index = %q, want products-test", ix)
		}
	}
	if rep.rowsRead != 2500 || rep.passed != 2500 || rep.indexed != 2500 || rep.failed != 0 {
		t.Errorf("rows_read=%d passed=%d indexed=%d failed=%d, want 2500 each and 0", rep.rowsRead, rep.passed, rep.indexed, rep.failed)
	}
	if !rep.complete || rep.limitReached {
		t.Errorf("complete=%v limitReached=%v, want true false", rep.complete, rep.limitReached)
	}
	if err := rep.check(); err != nil {
		t.Error(err)
	}
	if idx.maxCur > workers {
		t.Errorf("max concurrent Bulk = %d, want <= %d", idx.maxCur, workers)
	}
}

func TestPipeline_BatchesAreNotOverwrittenWhileInFlight(t *testing.T) {
	idx := &plIndexer{}
	if _, err := plRun(context.Background(), t, &plLines{n: 4500}, plCfg(t), idx, nil); err != nil {
		t.Fatal(err)
	}
	for i := range idx.batches {
		for j := range idx.batches[i] {
			if idx.batches[i][j].ID != idx.copies[i][j].ID {
				t.Fatalf("batch %d doc %d changed after Bulk received it: the slice was reused", i, j)
			}
		}
	}
}

func TestPipeline_ByteCapFlushesEarlyWithBigRecords(t *testing.T) {
	big := strings.Repeat("b", 16000)
	row := func(i int) []byte {
		return fmt.Appendf(nil, "c%07d\tName\t%s\ten:%s\ten:united-states\ta", i, big, big[:15990])
	}
	idx := &plIndexer{}
	rep, err := plRun(context.Background(), t, &plLines{n: 40, line: row}, plCfg(t), idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cost := docCost(idx.copies[0][0])
	perBatch := batchBytes / cost
	if perBatch < 2 || perBatch >= 40 {
		t.Fatalf("test data: %d records per batch", perBatch)
	}
	sizes := idx.sizes()
	if sizes[0] != perBatch {
		t.Errorf("first batch = %d documents, want %d (cost %d each)", sizes[0], perBatch, cost)
	}
	total := 0
	for _, n := range sizes {
		total += n
		if n > perBatch {
			t.Errorf("batch of %d documents is over the byte cap (%d fit)", n, perBatch)
		}
	}
	if total != 40 || rep.indexed != 40 {
		t.Errorf("sent %d, indexed %d, want 40", total, rep.indexed)
	}
}

func TestPipeline_LimitStopsExactlyAndDoesNotReadPast(t *testing.T) {
	plNoLeak(t)
	// Even lines pass, odd lines are blank (malformed): the 100th pass is line 198.
	src := &plLines{n: 5000, line: func(i int) []byte {
		if i%2 == 1 {
			return []byte{}
		}
		return plGood(i)
	}}
	idx := &plIndexer{}
	rep, err := plRun(context.Background(), t, src, plCfg(t, "--limit", "100"), idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := src.calls.Load(); got != 199 {
		t.Errorf("next called %d times, want 199 (stop at the 100th passed row)", got)
	}
	if rep.passed != 100 || rep.rowsRead != 199 || rep.indexed != 100 {
		t.Errorf("passed=%d rows_read=%d indexed=%d, want 100 199 100", rep.passed, rep.rowsRead, rep.indexed)
	}
	if !rep.limitReached || !rep.complete {
		t.Errorf("limitReached=%v complete=%v, want true true", rep.limitReached, rep.complete)
	}
	if got := idx.sizes(); !plEqualInts(got, []int{100}) {
		t.Errorf("batches = %v, want the last batch flushed ([100])", got)
	}
	if err := rep.check(); err != nil {
		t.Error(err)
	}
}

func TestPipeline_LimitEqualToInputSizeNeverReadsEOF(t *testing.T) {
	src := &plLines{n: 100}
	rep, err := plRun(context.Background(), t, src, plCfg(t, "--limit", "100"), &plIndexer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := src.calls.Load(); got != 100 {
		t.Errorf("next called %d times, want 100", got)
	}
	if !rep.limitReached {
		t.Error("limitReached = false, want true")
	}
}

func TestPipeline_LimitAboveInputIsNotReached(t *testing.T) {
	rep, err := plRun(context.Background(), t, &plLines{n: 50}, plCfg(t, "--limit", "100"), &plIndexer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.limitReached || !rep.complete || rep.passed != 50 {
		t.Errorf("limitReached=%v complete=%v passed=%d, want false true 50", rep.limitReached, rep.complete, rep.passed)
	}
}

func TestPipeline_LimitZeroReadsEverything(t *testing.T) {
	src := &plLines{n: 2500}
	rep, err := plRun(context.Background(), t, src, plCfg(t, "--limit", "0"), &plIndexer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.rowsRead != 2500 || src.calls.Load() != 2501 || rep.limitReached {
		t.Errorf("rows_read=%d next calls=%d limitReached=%v, want 2500 2501 false", rep.rowsRead, src.calls.Load(), rep.limitReached)
	}
}

func TestPipeline_DryRunNeverCallsBulkAndKeepsTheCounts(t *testing.T) {
	plNoLeak(t)
	rep, err := plRun(context.Background(), t, &plLines{n: 2500}, plCfg(t, "--dry-run", "--limit", "1200"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.passed != 1200 || rep.rowsRead != 1200 || rep.indexed != 0 || rep.failed != 0 {
		t.Errorf("passed=%d rows_read=%d indexed=%d failed=%d, want 1200 1200 0 0", rep.passed, rep.rowsRead, rep.indexed, rep.failed)
	}
	if !rep.limitReached || !rep.complete {
		t.Errorf("limitReached=%v complete=%v, want true true", rep.limitReached, rep.complete)
	}
	if err := rep.check(); err != nil {
		t.Error(err)
	}
}

func TestPipeline_DryRunWithoutLimitReadsAll(t *testing.T) {
	rep, err := plRun(context.Background(), t, &plLines{n: 2500}, plCfg(t, "--dry-run"), nil, nil)
	if err != nil || rep.passed != 2500 || !rep.complete || rep.limitReached {
		t.Errorf("err=%v passed=%d complete=%v limitReached=%v", err, rep.passed, rep.complete, rep.limitReached)
	}
}

func TestPipeline_ItemFailuresAreSummedAndDoNotStopTheRun(t *testing.T) {
	plNoLeak(t)
	idx := &plIndexer{hook: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		return catalog.BulkResult{
			Indexed: len(docs) - 3, Failed: 3,
			FailureTypes: map[string]int{"mapper_parsing_exception": 3},
			FailedIDs:    []string{docs[0].ID},
		}, nil
	}}
	rep, err := plRun(context.Background(), t, &plLines{n: 2500}, plCfg(t), idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if idx.calls != 3 {
		t.Errorf("Bulk calls = %d, want 3: a failed item must not stop the run", idx.calls)
	}
	if rep.indexed != 2500-9 || rep.failed != 9 {
		t.Errorf("indexed=%d failed=%d, want %d 9", rep.indexed, rep.failed, 2500-9)
	}
	if rep.failureTypes["mapper_parsing_exception"] != 9 {
		t.Errorf("failureTypes = %v, want 9 mapper_parsing_exception", rep.failureTypes)
	}
	if len(rep.failedIDs) != 3 {
		t.Errorf("failedIDs = %v, want 3 ids", rep.failedIDs)
	}
	if !rep.complete || rep.check() != nil {
		t.Errorf("complete=%v check=%v", rep.complete, rep.check())
	}
}

func TestPipeline_BulkErrorIsFatalStopsReadingAndKeepsTheCounts(t *testing.T) {
	plNoLeak(t)
	boom := errors.New("boom")
	ctxOfFailure := make(chan context.Context, 1)
	inNext := make(chan struct{}) // closed once the producer is inside next for line 2000
	idx := &plIndexer{hook: func(ctx context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		if docs[0].ID == "c0001000" { // batch 2
			ctxOfFailure <- ctx
			// Fail only once the producer is inside next: otherwise, on a loaded
			// machine, the cancel can land before its check and line 2000 is never read.
			select {
			case <-inNext:
			case <-time.After(plWait):
				t.Error("producer never reached line 2000")
			}
			// A result that comes with an error is not counted.
			return catalog.BulkResult{Indexed: 999, Failed: 1, FailureTypes: map[string]int{"x": 1}, FailedIDs: []string{"z"}}, boom
		}
		return catalog.BulkResult{Indexed: len(docs)}, nil
	}}
	src := &plLines{n: 5000}
	// next is called for line 2000 right after batch 2 was flushed; hold it until
	// the group is cancelled, so the stop is deterministic: that one line is
	// still read (the check runs before next), the one after it is not.
	src.onNext = func(i int) {
		if i != 2000 {
			return
		}
		close(inNext)
		select {
		case ctx := <-ctxOfFailure:
			select {
			case <-ctx.Done():
			case <-time.After(plWait):
				t.Error("group context was not cancelled by the Bulk error")
			}
		case <-time.After(plWait):
			t.Error("batch 2 never reached Bulk")
		}
	}
	rep, err := plRun(context.Background(), t, src, plCfg(t), idx, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the Bulk error", err)
	}
	if !strings.Contains(err.Error(), "batch 2") {
		t.Errorf("error %q does not name the batch", err)
	}
	if got := src.reads.Load(); got != 2001 {
		t.Errorf("lines read = %d, want 2001: the producer must stop at the next line", got)
	}
	if rep.complete {
		t.Error("complete = true after a fatal error")
	}
	if rep.passed != 2001 || rep.rowsRead != 2001 || rep.indexed != 1000 {
		t.Errorf("passed=%d rows_read=%d indexed=%d, want 2001 2001 1000 (counts so far)", rep.passed, rep.rowsRead, rep.indexed)
	}
	if idx.calls != 2 {
		t.Errorf("Bulk calls = %d, want 2", idx.calls)
	}
	if rep.failed != 0 || rep.failureTypes != nil || rep.failedIDs != nil {
		t.Errorf("failed=%d types=%v ids=%v: the result of a failed request must be ignored", rep.failed, rep.failureTypes, rep.failedIDs)
	}
}

func TestPipeline_BackpressureAndAtMostTwoParallelBulkCalls(t *testing.T) {
	plNoLeak(t)
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	idx := &plIndexer{hook: func(ctx context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return catalog.BulkResult{}, ctx.Err()
		}
		return catalog.BulkResult{Indexed: len(docs)}, nil
	}}
	// With two batches in flight the producer must block on the third flush,
	// after reading exactly 3,000 lines.
	beyond := make(chan struct{})
	src := &plLines{n: 10000}
	src.onNext = func(i int) {
		if i == 3000 {
			close(beyond)
		}
	}
	type result struct {
		rep *report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := plRun(context.Background(), t, src, plCfg(t), idx, nil)
		done <- result{rep, err}
	}()
	plWaitCh(t, arrived, "first Bulk call")
	plWaitCh(t, arrived, "second Bulk call: batches must run in parallel")
	select {
	case <-beyond:
		t.Fatal("producer read past the third batch while two were in flight: no backpressure")
	case <-time.After(100 * time.Millisecond): // negative check: it cannot fail a correct run
	}
	if got := src.reads.Load(); got != 3000 {
		t.Errorf("lines read while blocked = %d, want 3000", got)
	}
	close(release)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if idx.maxCur != workers {
		t.Errorf("max concurrent Bulk = %d, want exactly %d", idx.maxCur, workers)
	}
	if r.rep.indexed != 10000 || !r.rep.complete {
		t.Errorf("indexed=%d complete=%v, want 10000 true", r.rep.indexed, r.rep.complete)
	}
}

func TestPipeline_BucketsAndInvariants(t *testing.T) {
	// i%5: 0 passes, 1 blank, 2 too long, 3 oversized name, 4 empty code.
	line := func(i int) []byte {
		switch i % 5 {
		case 1:
			return []byte{}
		case 3:
			return fmt.Appendf(nil, "c%07d\t%s\tBrand\ten:snacks\ten:united-states\ta", i, strings.Repeat("x", 2000))
		case 4:
			return []byte("\tName\tBrand\ten:snacks\ten:united-states\ta")
		}
		return plGood(i)
	}
	tooLong := func(i int) bool { return i%5 == 2 }
	for _, dry := range []bool{false, true} {
		cfg, idx := plCfg(t), catalog.Indexer(&plIndexer{})
		if dry {
			cfg, idx = plCfg(t, "--dry-run"), nil
		}
		rep, err := plRun(context.Background(), t, &plLines{n: 100, line: line, tooLong: tooLong}, cfg, idx, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := map[skipReason]int{skipMalformed: 20, skipTooLong: 20, skipOversized: 20, skipInvalidCode: 20}
		for reason, n := range want {
			if rep.skipped[reason] != n {
				t.Errorf("dry=%v: bucket %v = %d, want %d", dry, reason, rep.skipped[reason], n)
			}
		}
		if rep.passed != 20 || rep.rowsRead != 100 {
			t.Errorf("dry=%v: passed=%d rows_read=%d, want 20 100", dry, rep.passed, rep.rowsRead)
		}
		if err := rep.check(); err != nil {
			t.Errorf("dry=%v: check: %v", dry, err)
		}
	}
}

func TestPipeline_NilLoggerIsAccepted(t *testing.T) {
	if _, err := plRun(context.Background(), t, &plLines{n: 3}, plCfg(t, "--dry-run"), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPipeline_NeverSendsAnEmptyBatch(t *testing.T) {
	for _, n := range []int{0, 1000, 2000} {
		idx := &plIndexer{}
		if _, err := plRun(context.Background(), t, &plLines{n: n}, plCfg(t), idx, nil); err != nil {
			t.Fatal(err)
		}
		if want := n / 1000; idx.calls != want {
			t.Errorf("%d rows: %d Bulk calls, want %d", n, idx.calls, want)
		}
	}
	// Rows that all fail the filters send nothing.
	idx := &plIndexer{}
	src := &plLines{n: 50, line: func(int) []byte { return []byte{} }}
	if _, err := plRun(context.Background(), t, src, plCfg(t), idx, nil); err != nil || idx.calls != 0 {
		t.Errorf("err=%v Bulk calls=%d, want nil 0", err, idx.calls)
	}
}
