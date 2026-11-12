package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// wrapEOF is a lines source whose end of input is a wrapped io.EOF.
type wrapEOF struct{ *plLines }

func (w wrapEOF) next() ([]byte, bool, error) {
	line, tooLong, err := w.plLines.next()
	if errors.Is(err, io.EOF) {
		err = fmt.Errorf("source: %w", err)
	}
	return line, tooLong, err
}

func TestPipeline_WrappedEOFEndsTheRunCleanly(t *testing.T) {
	idx := &plIndexer{}
	rep, err := plRun(context.Background(), t, wrapEOF{&plLines{n: 1500}}, plCfg(t), idx, nil)
	if err != nil || !rep.complete || rep.indexed != 1500 || rep.limitReached {
		t.Errorf("err=%v complete=%v indexed=%d limitReached=%v, want nil true 1500 false",
			err, rep.complete, rep.indexed, rep.limitReached)
	}
}

func TestPipeline_TruncatedInputDropsThePartialBatchWithoutCallingBulk(t *testing.T) {
	idx := &plIndexer{}
	src := plProg{plLines: &plLines{n: 600}, size: 99999, read: func() int64 { return 6000 }}
	rep, err := plRun(context.Background(), t, src, plCfg(t), idx, nil)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want a truncation error", err)
	}
	if idx.calls != 0 || rep.indexed != 0 {
		t.Errorf("Bulk calls=%d indexed=%d, want 0 0: the partial batch is dropped", idx.calls, rep.indexed)
	}
	if rep.complete {
		t.Error("complete = true on truncated input")
	}
	if rep.passed != 600 {
		t.Errorf("passed = %d, want 600 (rows read are kept)", rep.passed)
	}
}

func TestPipeline_ReadErrorCancelsBulkCallsInFlightAndKeepsTheReadError(t *testing.T) {
	plNoLeak(t)
	boom := errors.New("read boom")
	arrived := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // frees a Bulk call if the pipeline never cancels it
	idx := &plIndexer{hook: func(c context.Context, _ []catalog.Product) (catalog.BulkResult, error) {
		arrived <- struct{}{}
		select {
		case <-c.Done():
			cancelled <- struct{}{}
			return catalog.BulkResult{}, c.Err()
		case <-release:
			return catalog.BulkResult{}, errors.New("released: never cancelled")
		}
	}}
	src := &plLines{n: 5000, failAt: 2500, failErr: boom}
	src.onNext = func(i int) {
		if i == 2499 { // both full batches are in flight before the read fails
			plWaitCh(t, arrived, "first Bulk")
			plWaitCh(t, arrived, "second Bulk")
		}
	}
	rep, err := plRunBounded(t, src, plCfg(t), idx)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the read error, not the cancellation it caused", err)
	}
	plWaitCh(t, cancelled, "first Bulk to observe the cancellation")
	plWaitCh(t, cancelled, "second Bulk to observe the cancellation")
	if rep.complete || rep.rowsRead != 2499 || rep.passed != 2499 {
		t.Errorf("complete=%v rows_read=%d passed=%d, want false 2499 2499", rep.complete, rep.rowsRead, rep.passed)
	}
}

func TestPipeline_BulkErrorBeforeAReadErrorStaysTheReturnedError(t *testing.T) {
	plNoLeak(t)
	boom := errors.New("bulk boom")
	second := make(chan struct{})
	observed := make(chan struct{})
	idx := &plIndexer{hook: func(c context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		if firstBatch(docs) {
			plWaitCh(t, second, "batch 2 to reach Bulk")
			return catalog.BulkResult{}, boom
		}
		close(second)
		<-c.Done()
		close(observed)
		return catalog.BulkResult{}, c.Err()
	}}
	src := &plLines{n: 5000, failAt: 2001, failErr: errors.New("read boom")}
	src.onNext = func(i int) {
		if i == 2000 { // both batches are sent; the read below fails once the Bulk error cancelled the group
			plWaitCh(t, observed, "the cancellation caused by the Bulk error")
		}
	}
	_, err := plRunBounded(t, src, plCfg(t), idx)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the Bulk error that came first", err)
	}
}
