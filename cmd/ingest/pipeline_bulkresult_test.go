package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// firstBatch reports whether docs is the first batch of a plLines source.
func firstBatch(docs []catalog.Product) bool { return docs[0].ID == "c0000000" }

// plRunBounded is plRun, but fails the test instead of hanging when the
// pipeline does not return within plWait.
func plRunBounded(t *testing.T, src lines, cfg config, idx catalog.Indexer) (*report, error) {
	t.Helper()
	type result struct {
		rep *report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := plRun(context.Background(), t, src, cfg, idx, nil)
		done <- result{rep, err}
	}()
	select {
	case r := <-done:
		return r.rep, r.err
	case <-time.After(plWait):
		t.Fatal("pipeline did not return")
		return nil, nil
	}
}

func TestPipeline_BulkErrorNamesTheBatchThatFailedNotTheLastOneSent(t *testing.T) {
	plNoLeak(t)
	boom := errors.New("batch one boom")
	third := make(chan struct{})
	idx := &plIndexer{hook: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		switch docs[0].ID {
		case "c0000000": // batch 1 fails only after batch 3 reached Bulk
			plWaitCh(t, third, "batch 3 to reach Bulk")
			return catalog.BulkResult{}, boom
		case "c0002000":
			close(third)
		}
		return catalog.BulkResult{Indexed: len(docs)}, nil
	}}
	_, err := plRun(context.Background(), t, &plLines{n: 3000}, plCfg(t), idx, nil)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "bulk batch 1 (1000 documents)") {
		t.Errorf("error = %v, want boom from \"bulk batch 1 (1000 documents)\"", err)
	}
}

func TestPipeline_BulkResultThatDoesNotAccountForItsBatchIsFatal(t *testing.T) {
	tests := []struct {
		name string
		res  catalog.BulkResult
		want string
	}{
		{"short", catalog.BulkResult{Indexed: 499}, "bulk batch 1: result accounts for 499 of 500 documents"},
		{"short with failures", catalog.BulkResult{Indexed: 300, Failed: 100}, "result accounts for 400 of 500 documents"},
		{"over", catalog.BulkResult{Indexed: 501}, "result accounts for 501 of 500 documents"},
		{"empty", catalog.BulkResult{}, "result accounts for 0 of 500 documents"},
		{"negative failed with the right sum", catalog.BulkResult{Indexed: 501, Failed: -1}, "result accounts for 500 of 500 documents"},
		{"negative indexed with the right sum", catalog.BulkResult{Indexed: -1, Failed: 501}, "result accounts for 500 of 500 documents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plNoLeak(t)
			idx := &plIndexer{hook: func(context.Context, []catalog.Product) (catalog.BulkResult, error) {
				return tt.res, nil
			}}
			rep, err := plRun(context.Background(), t, &plLines{n: 500}, plCfg(t), idx, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if rep.complete {
				t.Error("complete = true after an inconsistent Bulk result")
			}
			if rep.indexed != 0 || rep.failed != 0 {
				t.Errorf("indexed=%d failed=%d, want 0 0: the inconsistent result is not merged", rep.indexed, rep.failed)
			}
			if rep.passed != 500 {
				t.Errorf("passed = %d, want 500 (rows read are kept)", rep.passed)
			}
		})
	}
}

func TestPipeline_BulkResultThatAccountsForItsBatchIsCompleteEvenWhenEveryDocumentFailed(t *testing.T) {
	tests := []struct {
		name        string
		failed      func(n int) int
		wantIndexed int
		wantFailed  int
	}{
		{"some failed", func(int) int { return 100 }, 400, 100},
		{"all failed", func(n int) int { return n }, 0, 500},
		{"none failed", func(int) int { return 0 }, 500, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := &plIndexer{hook: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
				f := tt.failed(len(docs))
				return catalog.BulkResult{Indexed: len(docs) - f, Failed: f}, nil
			}}
			rep, err := plRun(context.Background(), t, &plLines{n: 500}, plCfg(t), idx, nil)
			if err != nil || !rep.complete || rep.indexed != tt.wantIndexed || rep.failed != tt.wantFailed {
				t.Errorf("err=%v complete=%v indexed=%d failed=%d, want nil true %d %d",
					err, rep.complete, rep.indexed, rep.failed, tt.wantIndexed, tt.wantFailed)
			}
		})
	}
}

func TestPipeline_AccountingErrorNamesTheBatchThatFailedNotTheLastOneSent(t *testing.T) {
	plNoLeak(t)
	third := make(chan struct{})
	idx := &plIndexer{hook: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		switch docs[0].ID {
		case "c0000000": // batch 1 answers only after batch 3 reached Bulk
			plWaitCh(t, third, "batch 3 to reach Bulk")
			return catalog.BulkResult{Indexed: len(docs) - 1}, nil
		case "c0002000":
			close(third)
		}
		return catalog.BulkResult{Indexed: len(docs)}, nil
	}}
	_, err := plRun(context.Background(), t, &plLines{n: 3000}, plCfg(t), idx, nil)
	if err == nil || !strings.Contains(err.Error(), "bulk batch 1: result accounts for 999 of 1000 documents") {
		t.Errorf("error = %v, want \"bulk batch 1: result accounts for 999 of 1000 documents\"", err)
	}
}

func TestPipeline_InconsistentBulkResultCancelsTheOtherBatchesInFlight(t *testing.T) {
	plNoLeak(t)
	second := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // frees a Bulk call if the pipeline never cancels it
	idx := &plIndexer{hook: func(c context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		if firstBatch(docs) {
			plWaitCh(t, second, "batch 2 to reach Bulk")
			return catalog.BulkResult{Indexed: len(docs) - 1}, nil
		}
		close(second)
		select {
		case <-c.Done():
			close(cancelled)
			return catalog.BulkResult{}, c.Err()
		case <-release:
			return catalog.BulkResult{}, errors.New("released: never cancelled")
		}
	}}
	_, err := plRunBounded(t, &plLines{n: 2000}, plCfg(t), idx)
	if err == nil || !strings.Contains(err.Error(), "result accounts for 999 of 1000 documents") {
		t.Fatalf("error = %v, want the accounting error, not the cancellation it caused", err)
	}
	plWaitCh(t, cancelled, "the other Bulk call to observe the cancellation")
}
