package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

func TestPipeline_CancelMidStreamReturnsCanceledFastEvenWhenBulkBlocks(t *testing.T) {
	plNoLeak(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := &plIndexer{hook: func(c context.Context, _ []catalog.Product) (catalog.BulkResult, error) {
		<-c.Done()
		return catalog.BulkResult{}, c.Err()
	}}
	src := &plLines{n: 100000}
	src.onNext = func(i int) {
		if i == 1500 {
			cancel()
		}
	}
	type result struct {
		rep *report
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		rep, err := plRun(ctx, t, src, plCfg(t), idx, nil)
		done <- result{rep, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(plWait):
		t.Fatal("pipeline did not return after the cancel")
	}
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", r.err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("returned after %v, want < 1s", d)
	}
	if got := src.reads.Load(); got != 1501 {
		t.Errorf("lines read = %d, want 1501: one line after the cancel at most", got)
	}
	if r.rep.complete {
		t.Error("complete = true after cancellation")
	}
	if r.rep.passed != 1501 || r.rep.rowsRead != 1501 {
		t.Errorf("passed=%d rows_read=%d, want 1501 (counts so far)", r.rep.passed, r.rep.rowsRead)
	}
}

func TestPipeline_CancelWhileProducerIsBlockedOnAFullPoolReturnsCanceled(t *testing.T) {
	plNoLeak(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	arrived := make(chan struct{}, 4)
	idx := &plIndexer{hook: func(c context.Context, _ []catalog.Product) (catalog.BulkResult, error) {
		arrived <- struct{}{}
		<-c.Done()
		return catalog.BulkResult{}, c.Err()
	}}
	errc := make(chan error, 1)
	go func() {
		_, err := plRun(ctx, t, &plLines{n: 100000}, plCfg(t), idx, nil)
		errc <- err
	}()
	plWaitCh(t, arrived, "first Bulk")
	plWaitCh(t, arrived, "second Bulk")
	start := time.Now()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("returned %v after the cancel, want < 1s", d)
		}
	case <-time.After(plWait):
		t.Fatal("pipeline did not return after the cancel")
	}
}

func TestPipeline_AlreadyCancelledContextReadsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &plLines{n: 10}
	rep, err := plRun(ctx, t, src, plCfg(t, "--dry-run"), nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if src.calls.Load() != 0 || rep.rowsRead != 0 || rep.complete {
		t.Errorf("next calls=%d rows_read=%d complete=%v, want 0 0 false", src.calls.Load(), rep.rowsRead, rep.complete)
	}
}

func TestPipeline_SourceErrorIsFatalAndKeepsTheCounts(t *testing.T) {
	plNoLeak(t)
	boom := errors.New("read boom")
	src := &plLines{n: 5000, failAt: 1500, failErr: boom}
	rep, err := plRun(context.Background(), t, src, plCfg(t), &plIndexer{}, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the source error", err)
	}
	if rep.complete || rep.limitReached {
		t.Errorf("complete=%v limitReached=%v, want false false", rep.complete, rep.limitReached)
	}
	if rep.rowsRead != 1499 || rep.passed != 1499 {
		t.Errorf("rows_read=%d passed=%d, want 1499 (the failed read is not a line)", rep.rowsRead, rep.passed)
	}
	if rep.indexed != 1000 {
		t.Errorf("indexed = %d, want 1000 (the full batch was sent and awaited)", rep.indexed)
	}
}
