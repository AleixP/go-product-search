package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// plMerger returns a pipeline wired like runPipeline does, to call merge
// directly.
func plMerger(t *testing.T, logs *plLogs) *pipeline {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	if logs != nil {
		log = plLogger(logs)
	}
	return &pipeline{rep: newReport(plCfg(t), "x"), log: log}
}

func TestMerge_SumsIndexedAndFailed(t *testing.T) {
	p := plMerger(t, nil)
	p.merge(catalog.BulkResult{Indexed: 10, Failed: 1})
	p.merge(catalog.BulkResult{Indexed: 5, Failed: 2})
	if p.rep.indexed != 15 || p.rep.failed != 3 {
		t.Errorf("indexed=%d failed=%d, want 15 3", p.rep.indexed, p.rep.failed)
	}
}

func TestMerge_FailureTypesCapAt32NamedPlusOther(t *testing.T) {
	p := plMerger(t, nil)
	for i := 0; i < 33; i++ {
		p.merge(catalog.BulkResult{Failed: 1, FailureTypes: map[string]int{fmt.Sprintf("t%02d", i): 1}})
	}
	ft := p.rep.failureTypes
	if len(ft) != maxMergedFailureTypes+1 {
		t.Fatalf("%d keys, want %d named + other: %v", len(ft), maxMergedFailureTypes, ft)
	}
	if ft["other"] != 1 || ft["t31"] != 1 || ft["t32"] != 0 {
		t.Errorf("failureTypes = %v, want t00..t31 and other=1", ft)
	}
	// A known type keeps counting at the cap; a new one goes to other.
	p.merge(catalog.BulkResult{Failed: 3, FailureTypes: map[string]int{"t00": 2, "brand-new": 1}})
	if ft["t00"] != 3 || ft["other"] != 2 || len(ft) != maxMergedFailureTypes+1 {
		t.Errorf("after the cap: %v, want t00=3 other=2 and still %d keys", ft, maxMergedFailureTypes+1)
	}
}

func TestMerge_OtherFromAnAdapterDoesNotUseANamedSlot(t *testing.T) {
	p := plMerger(t, nil)
	p.merge(catalog.BulkResult{Failed: 2, FailureTypes: map[string]int{"other": 2}})
	for i := 0; i < 33; i++ {
		p.merge(catalog.BulkResult{Failed: 1, FailureTypes: map[string]int{fmt.Sprintf("t%02d", i): 1}})
	}
	ft := p.rep.failureTypes
	if len(ft) != maxMergedFailureTypes+1 || ft["t31"] != 1 || ft["t32"] != 0 || ft["other"] != 3 {
		t.Errorf("failureTypes = %v, want 32 named + other=3", ft)
	}
}

func TestMerge_NoFailureTypesLeavesTheMapNil(t *testing.T) {
	p := plMerger(t, nil)
	p.merge(catalog.BulkResult{Indexed: 3})
	if p.rep.failureTypes != nil {
		t.Errorf("failureTypes = %v, want nil", p.rep.failureTypes)
	}
}

func TestMerge_FailedIDsAreCappedAtTenInOrder(t *testing.T) {
	ids := func(from, n int) []string {
		var out []string
		for i := from; i < from+n; i++ {
			out = append(out, fmt.Sprintf("id%d", i))
		}
		return out
	}
	tests := []struct {
		name    string
		results [][]string
		want    int
	}{
		{"three results of four", [][]string{ids(0, 4), ids(4, 4), ids(8, 4)}, 10},
		{"exactly ten", [][]string{ids(0, 10)}, 10},
		{"nine stay nine", [][]string{ids(0, 9)}, 9},
		{"eleven in one result", [][]string{ids(0, 11)}, 10},
		{"full list then more", [][]string{ids(0, 10), ids(10, 3)}, 10},
	}
	for _, tt := range tests {
		p := plMerger(t, nil)
		for _, r := range tt.results {
			p.merge(catalog.BulkResult{Failed: len(r), FailedIDs: r})
		}
		got := p.rep.failedIDs
		if len(got) != tt.want {
			t.Errorf("%s: %d ids %v, want %d", tt.name, len(got), got, tt.want)
			continue
		}
		for i, id := range got {
			if id != fmt.Sprintf("id%d", i) {
				t.Errorf("%s: id[%d] = %s, want id%d (completion order)", tt.name, i, id, i)
			}
		}
	}
}

func TestMerge_WarnsOncePerFailedResultWithTypesAndFiveSampleIDs(t *testing.T) {
	var logs plLogs
	p := plMerger(t, &logs)
	p.merge(catalog.BulkResult{Indexed: 100}) // no failure: no WARN
	if n := len(logs.lines("level=WARN")); n != 0 {
		t.Fatalf("WARN for a result without failures: %q", logs.String())
	}
	p.merge(catalog.BulkResult{
		Indexed: 3, Failed: 8,
		FailureTypes: map[string]int{"mapper_parsing_exception": 5, "version_conflict": 3},
		FailedIDs:    []string{"id1", "id2", "id3", "id4", "id5", "id6", "id7", "id8"},
	})
	warns := logs.lines("level=WARN")
	if len(warns) != 1 {
		t.Fatalf("%d WARN lines, want 1: %q", len(warns), logs.String())
	}
	w := warns[0]
	for _, want := range []string{"mapper_parsing_exception", "version_conflict", "id1", "id5", "failed=8"} {
		if !strings.Contains(w, want) {
			t.Errorf("WARN %q does not mention %q", w, want)
		}
	}
	if strings.Contains(w, "id6") {
		t.Errorf("WARN %q has more than 5 sample ids", w)
	}
	// A result with failures and no sample ids still warns.
	p.merge(catalog.BulkResult{Failed: 1})
	if n := len(logs.lines("level=WARN")); n != 2 {
		t.Errorf("%d WARN lines, want 2", n)
	}
}

func TestPipeline_WarnsOncePerFailedBatchResult(t *testing.T) {
	var logs plLogs
	idx := &plIndexer{hook: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		if len(docs) == 500 { // the last batch is clean
			return catalog.BulkResult{Indexed: 500}, nil
		}
		return catalog.BulkResult{Indexed: len(docs) - 1, Failed: 1, FailureTypes: map[string]int{"x": 1}, FailedIDs: []string{docs[0].ID}}, nil
	}}
	if _, err := plRun(context.Background(), t, &plLines{n: 2500}, plCfg(t), idx, plLogger(&logs)); err != nil {
		t.Fatal(err)
	}
	if n := len(logs.lines("level=WARN")); n != 2 {
		t.Errorf("%d WARN lines, want 2 (two failed batches, one clean): %q", n, logs.String())
	}
}

func TestPipeline_ConcurrentMergesAreSafe(t *testing.T) {
	// Both Bulk calls return at the same moment: -race flags an unlocked merge.
	var arrived, release = make(chan struct{}, 2), make(chan struct{})
	idx := &plIndexer{hook: func(ctx context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		arrived <- struct{}{}
		<-release
		return catalog.BulkResult{Indexed: len(docs) - 1, Failed: 1, FailureTypes: map[string]int{"x": 1}, FailedIDs: []string{"a"}}, nil
	}}
	errc := make(chan error, 1)
	var rep *report
	go func() {
		var err error
		rep, err = plRun(context.Background(), t, &plLines{n: 2000}, plCfg(t), idx, nil)
		errc <- err
	}()
	plWaitCh(t, arrived, "first Bulk")
	plWaitCh(t, arrived, "second Bulk")
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if rep.indexed != 1998 || rep.failed != 2 || rep.failureTypes["x"] != 2 || len(rep.failedIDs) != 2 {
		t.Errorf("indexed=%d failed=%d types=%v ids=%v", rep.indexed, rep.failed, rep.failureTypes, rep.failedIDs)
	}
}

func TestMerge_TypesOverTheCapInOneResultKeepTheSmallestNamesFirst(t *testing.T) {
	// 40 distinct types in one result: the order is by name, so the outcome does
	// not depend on map iteration.
	types := map[string]int{}
	for i := 0; i < 40; i++ {
		types[fmt.Sprintf("t%02d", i)] = 1
	}
	p := plMerger(t, nil)
	p.merge(catalog.BulkResult{Failed: 40, FailureTypes: types})
	ft := p.rep.failureTypes
	if len(ft) != maxMergedFailureTypes+1 || ft["other"] != 8 {
		t.Fatalf("failureTypes = %v, want 32 named and other=8", ft)
	}
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("t%02d", i)
		if want := map[bool]int{true: 1, false: 0}[i < maxMergedFailureTypes]; ft[name] != want {
			t.Errorf("failureTypes[%s] = %d, want %d (names are taken in sorted order)", name, ft[name], want)
		}
	}
}
