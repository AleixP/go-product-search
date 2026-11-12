package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// plProgOf wraps l as a source with progress: BytesRead is 10 bytes per line
// served, Size is size.
func plProgOf(l *plLines, size int64) plProg {
	return plProg{plLines: l, size: size, read: func() int64 { return l.reads.Load() * 10 }}
}

func TestProgressConstants(t *testing.T) {
	if progressEvery != 4096 || logEvery != 500000 {
		t.Errorf("progressEvery=%d logEvery=%d, want 4096 500000", progressEvery, logEvery)
	}
}

func TestPipeline_BytesReadIsUpdatedEveryProgressEveryLinesAndAtTheEnd(t *testing.T) {
	const n = 3*progressEvery + 7
	l := &plLines{n: n}
	src := plProgOf(l, n*10)
	var rep *report
	check := func(i int, want int64) {
		if rep != nil && rep.bytesRead != want {
			t.Errorf("before line %d: bytesRead = %d, want %d", i, rep.bytesRead, want)
		}
	}
	l.onNext = func(i int) {
		switch i {
		case progressEvery - 1:
			check(i, 0) // not yet: only progressEvery-1 rows were counted
		case progressEvery:
			check(i, progressEvery*10)
		case 2*progressEvery - 1:
			check(i, progressEvery*10)
		case 2 * progressEvery:
			check(i, 2*progressEvery*10)
		}
	}
	cfg := plCfg(t, "--dry-run")
	rep = newReport(cfg, "x")
	if err := runPipeline(context.Background(), src, plHeader(t), cfg, nil, "i", rep, nil); err != nil {
		t.Fatal(err)
	}
	if rep.bytesRead != n*10 || rep.bytesTotal != n*10 {
		t.Errorf("bytesRead=%d bytesTotal=%d, want %d %d", rep.bytesRead, rep.bytesTotal, n*10, n*10)
	}
}

func TestPipeline_BytesTotalIsSetFromTheStartAndUnknownIsKept(t *testing.T) {
	l := &plLines{n: 10}
	var rep *report
	l.onNext = func(i int) {
		if i == 1 && rep.bytesTotal != 100 {
			t.Errorf("bytesTotal during the run = %d, want 100", rep.bytesTotal)
		}
	}
	cfg := plCfg(t, "--dry-run")
	rep = newReport(cfg, "x")
	if err := runPipeline(context.Background(), plProgOf(l, 100), plHeader(t), cfg, nil, "i", rep, nil); err != nil {
		t.Fatal(err)
	}
	// Size -1: unknown stays -1 and the check is skipped.
	rep, err := plRun(context.Background(), t, plProgOf(&plLines{n: 10}, -1), cfg, nil, nil)
	if err != nil || rep.bytesTotal != -1 || rep.bytesRead != 100 {
		t.Errorf("err=%v bytesTotal=%d bytesRead=%d, want nil -1 100", err, rep.bytesTotal, rep.bytesRead)
	}
}

func TestPipeline_WithoutProgressTheByteCountersAreUntouched(t *testing.T) {
	rep, err := plRun(context.Background(), t, &plLines{n: 3 * progressEvery}, plCfg(t, "--dry-run"), nil, nil)
	if err != nil || rep.bytesRead != 0 || rep.bytesTotal != -1 {
		t.Errorf("err=%v bytesRead=%d bytesTotal=%d, want nil 0 -1", err, rep.bytesRead, rep.bytesTotal)
	}
}

func TestPipeline_BytesReadIsFilledOnErrorAndOnCancel(t *testing.T) {
	boom := errors.New("boom")
	l := &plLines{n: 100, failAt: 51, failErr: boom}
	rep, err := plRun(context.Background(), t, plProgOf(l, 1000), plCfg(t, "--dry-run"), nil, nil)
	if !errors.Is(err, boom) || rep.bytesRead != 500 || rep.bytesTotal != 1000 {
		t.Errorf("source error: err=%v bytesRead=%d bytesTotal=%d, want boom 500 1000", err, rep.bytesRead, rep.bytesTotal)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l = &plLines{n: 100}
	l.onNext = func(i int) {
		if i == 20 {
			cancel()
		}
	}
	rep, err = plRun(ctx, t, plProgOf(l, 1000), plCfg(t, "--dry-run"), nil, nil)
	if !errors.Is(err, context.Canceled) || rep.bytesRead != 210 || rep.bytesTotal != 1000 {
		t.Errorf("cancel: err=%v bytesRead=%d bytesTotal=%d, want canceled 210 1000", err, rep.bytesRead, rep.bytesTotal)
	}
}

func TestPipeline_TruncatedInputAtCleanEOFIsFatal(t *testing.T) {
	tests := []struct {
		name       string
		read, size int64
		wantErr    bool
	}{
		{"read less than size", 1000, 1500, true},
		{"read more than size", 1600, 1500, true},
		{"equal", 1500, 1500, false},
		{"unknown size skips the check", 7, -1, false},
		{"zero size skips the check", 7, 0, false},
		{"size 1 is checked", 5, 1, true},
	}
	for _, tt := range tests {
		l := &plLines{n: 100}
		src := plProg{plLines: l, size: tt.size, read: func() int64 { return tt.read }}
		rep, err := plRun(context.Background(), t, src, plCfg(t, "--dry-run"), nil, nil)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
		if rep.complete == tt.wantErr {
			t.Errorf("%s: complete = %v", tt.name, rep.complete)
		}
		if rep.passed != 100 || rep.rowsRead != 100 {
			t.Errorf("%s: counts lost: passed=%d rows_read=%d", tt.name, rep.passed, rep.rowsRead)
		}
		if tt.wantErr && !containsAll(err.Error(), "truncated") {
			t.Errorf("%s: error %q should say truncated and give the sizes", tt.name, err)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestPipeline_TruncationErrorSaysHowManyBytesWereRead(t *testing.T) {
	src := plProg{plLines: &plLines{n: 5}, size: 1500, read: func() int64 { return 1000 }}
	_, err := plRun(context.Background(), t, src, plCfg(t, "--dry-run"), nil, nil)
	want := "input ended after 1000 of 1500 bytes: the file or response is truncated"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to contain %q", err, want)
	}
}

func TestPipeline_TruncationIsNotCheckedWhenTheLimitStopsTheRun(t *testing.T) {
	src := plProg{plLines: &plLines{n: 100}, size: 1500, read: func() int64 { return 10 }}
	rep, err := plRun(context.Background(), t, src, plCfg(t, "--dry-run", "--limit", "50"), nil, nil)
	if err != nil || !rep.limitReached || !rep.complete {
		t.Errorf("err=%v limitReached=%v complete=%v, want nil true true", err, rep.limitReached, rep.complete)
	}
}

func TestPipeline_TruncationDoesNotHideTheRealError(t *testing.T) {
	boom := errors.New("boom")
	src := plProg{plLines: &plLines{n: 100, failAt: 10, failErr: boom}, size: 1500, read: func() int64 { return 10 }}
	_, err := plRun(context.Background(), t, src, plCfg(t, "--dry-run"), nil, nil)
	if !errors.Is(err, boom) || strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %v, want the source error only", err)
	}
}

func TestPipeline_TruncatedInputWithAnIndexerIsAnIncompleteRun(t *testing.T) {
	// A fatal truncation at EOF is an error of the run: the report is not complete.
	idx := &plIndexer{}
	src := plProg{plLines: &plLines{n: 10}, size: 99, read: func() int64 { return 1 }}
	rep, err := plRun(context.Background(), t, src, plCfg(t), idx, nil)
	if err == nil || rep.complete {
		t.Errorf("err=%v complete=%v, want an error and incomplete", err, rep.complete)
	}
}
