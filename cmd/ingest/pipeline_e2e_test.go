package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// plOpen opens path with the real source and line reader, reads the header and
// returns the joined lines value plus the header and the source.
func plOpen(t *testing.T, path string) (lines, header, *source) {
	t.Helper()
	src, err := openInput(context.Background(), nil, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	lr := newLineReader(src, 0)
	first, tooLong, err := lr.next()
	if err != nil || tooLong {
		t.Fatalf("header line: err=%v tooLong=%v", err, tooLong)
	}
	hdr, err := parseHeader(first)
	if err != nil {
		t.Fatal(err)
	}
	return progressLines{lr, src}, hdr, src
}

func TestPipeline_FixtureThroughTheRealReader(t *testing.T) {
	plNoLeak(t)
	path := filepath.Join("..", "..", "testdata", "products.tsv")
	in, hdr, src := plOpen(t, path)
	cfg, err := parseConfig([]string{"--countries", "en:united-states", "--sample-every", "1"})
	if err != nil {
		t.Fatal(err)
	}
	idx := &plIndexer{}
	rep := newReport(cfg, "testdata/products.tsv")
	if err := runPipeline(context.Background(), in, hdr, cfg, idx, "products-fixture", rep, nil); err != nil {
		t.Fatal(err)
	}
	if rep.rowsRead != 15 || rep.passed != 7 || rep.indexed != 7 || rep.failed != 0 {
		t.Errorf("rows_read=%d passed=%d indexed=%d failed=%d, want 15 7 7 0", rep.rowsRead, rep.passed, rep.indexed, rep.failed)
	}
	if rep.skipped[skipMalformed] != 1 || rep.skipped[skipInvalidCode] != 1 || rep.skipped[skipCountry] != 4 || rep.skipped[skipCategory] != 2 {
		t.Errorf("buckets = %v", rep.skipped)
	}
	if err := rep.check(); err != nil {
		t.Error(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.bytesRead != st.Size() || rep.bytesTotal != st.Size() || src.Size() != st.Size() {
		t.Errorf("bytesRead=%d bytesTotal=%d, want both %d", rep.bytesRead, rep.bytesTotal, st.Size())
	}
	want := []catalog.Product{
		{ID: "0046872021545", Name: "Smoked Mussels In Cottonseed Oil", Brands: []string{"Family"}, Categories: []string{"undefined"}, Nutriscore: "a"},
		{ID: "9990000000001", Name: `"Lone quote product`, Brands: []string{"Synthetic Brand"}, Categories: []string{"snacks"}, Nutriscore: "c"},
		{ID: "0862418000269", Name: "Rawpothecary, Probiotic Water", Brands: []string{"Rawpothecary Inc."}, Categories: []string{"beverages"}, Nutriscore: "b"},
		{ID: "0026395783168", Name: "Organic aloe water beverage", Brands: []string{"Lodc Inc."}, Categories: []string{"plant-based-foods-and-beverages", "beverages", "plant-based-beverages"}, Nutriscore: "c"},
		{ID: "0041757001803", Name: "Light snack cheese count", Brands: []string{"Bel Brands Usa"}, Categories: []string{"dairies", "fermented-foods", "fermented-milk-products", "cheeses"}, Nutriscore: "d"},
		{ID: "0099482439903", Name: "Whole milk", Brands: []string{"365 Everyday Value"}, Categories: []string{"dairies", "milks"}, Nutriscore: "e"},
		{ID: "0011822442411", Name: "Apricots", Brands: []string{"Big Win", "Rite Aid Corporation"}, Categories: []string{"undefined"}, Nutriscore: "d"},
	}
	if len(idx.copies) != 1 || !reflect.DeepEqual(idx.copies[0], want) {
		t.Errorf("documents received:\n got %+v\nwant %+v", idx.copies, want)
	}
	if idx.indices[0] != "products-fixture" {
		t.Errorf("index = %q", idx.indices[0])
	}
}

// plGzipFile writes n synthetic rows with a header, gzipped, and returns the path.
func plGzipFile(t *testing.T, n int) string {
	t.Helper()
	var plain bytes.Buffer
	plain.WriteString("code\tproduct_name\tbrands\tcategories_tags\tcountries_tags\tnutriscore_grade\n")
	for i := 0; i < n; i++ {
		plain.Write(plGood(i))
		plain.WriteByte('\n')
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := io.Copy(zw, &plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "p.tsv.gz")
	if err := os.WriteFile(path, gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPipeline_CompleteGzipFileEndsCleanWithBytesReadEqualToSize(t *testing.T) {
	plNoLeak(t)
	in, hdr, src := plOpen(t, plGzipFile(t, 3000))
	cfg := plCfg(t, "--dry-run")
	rep := newReport(cfg, "x")
	if err := runPipeline(context.Background(), in, hdr, cfg, nil, "i", rep, nil); err != nil {
		t.Fatal(err)
	}
	if rep.passed != 3000 || !rep.complete || rep.bytesRead != src.Size() || rep.bytesRead == 0 {
		t.Errorf("passed=%d complete=%v bytesRead=%d size=%d", rep.passed, rep.complete, rep.bytesRead, src.Size())
	}
}

func TestPipeline_TruncatedGzipIsFatalAndKeepsTheCounts(t *testing.T) {
	plNoLeak(t)
	full, err := os.ReadFile(plGzipFile(t, 3000))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cut.tsv.gz")
	if err := os.WriteFile(path, full[:len(full)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	in, hdr, _ := plOpen(t, path)
	cfg := plCfg(t)
	idx := &plIndexer{}
	rep := newReport(cfg, "x")
	err = runPipeline(context.Background(), in, hdr, cfg, idx, "i", rep, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
	}
	if rep.complete || rep.passed == 0 || rep.passed >= 3000 {
		t.Errorf("complete=%v passed=%d, want incomplete with the rows read so far", rep.complete, rep.passed)
	}
}

func TestPipeline_LogsProgressEvery500000LinesAndOnceAtTheEnd(t *testing.T) {
	blank := func(int) []byte { return []byte{} }
	for _, tt := range []struct{ n, progress int }{{499999, 0}, {500000, 1}, {1000000, 2}} {
		var logs plLogs
		rep, err := plRun(context.Background(), t, &plLines{n: tt.n, line: blank}, plCfg(t, "--dry-run"), nil, plLogger(&logs))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(logs.lines("level=INFO", "ingest progress")); got != tt.progress {
			t.Errorf("%d lines: %d progress logs, want %d: %q", tt.n, got, tt.progress, logs.String())
		}
		fin := logs.lines("level=INFO", "ingest finished")
		if len(fin) != 1 || !strings.Contains(fin[0], fmt.Sprintf("rows_read=%d", tt.n)) {
			t.Errorf("%d lines: finished logs = %q", tt.n, fin)
		}
		if rep.skipped[skipMalformed] != tt.n {
			t.Errorf("malformed = %d, want %d", rep.skipped[skipMalformed], tt.n)
		}
	}
}

func TestPipeline_ProgressLogCarriesTheCounters(t *testing.T) {
	var logs plLogs
	_, err := plRun(context.Background(), t, &plLines{n: 500000}, plCfg(t), &plIndexer{}, plLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	p := logs.lines("ingest progress")
	if len(p) != 1 {
		t.Fatalf("progress logs = %q", p)
	}
	for _, want := range []string{"rows_read=500000", "passed=500000", "indexed=", "failed=", "elapsed="} {
		if !strings.Contains(p[0], want) {
			t.Errorf("progress log %q lacks %q", p[0], want)
		}
	}
}

func TestPipeline_ProgressLogShowsTheIndexedAndFailedCountsSoFar(t *testing.T) {
	// Only the first 1,000 rows pass: one batch, 990 indexed and 10 failed. The
	// producer waits for its WARN (written after the merge) before it goes on, so
	// the counters at line 500,000 are deterministic.
	var logs plLogs
	idx := &plIndexer{hook: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		return catalog.BulkResult{Indexed: 990, Failed: 10}, nil
	}}
	src := &plLines{n: 500000, line: func(i int) []byte {
		if i < 1000 {
			return plGood(i)
		}
		return []byte{}
	}}
	src.onNext = func(i int) {
		if i != 2000 {
			return
		}
		deadline := time.Now().Add(plWait)
		for len(logs.lines("level=WARN")) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := plRun(context.Background(), t, src, plCfg(t), idx, plLogger(&logs)); err != nil {
		t.Fatal(err)
	}
	p := logs.lines("ingest progress")
	if len(p) != 1 || !strings.Contains(p[0], "indexed=990") || !strings.Contains(p[0], "failed=10") || !strings.Contains(p[0], "passed=1000") {
		t.Errorf("progress logs = %q, want indexed=990 failed=10 passed=1000", p)
	}
}

func TestPipeline_NoFinishedLogAfterAnError(t *testing.T) {
	var logs plLogs
	boom := errors.New("boom")
	_, err := plRun(context.Background(), t, &plLines{n: 10, failAt: 5, failErr: boom}, plCfg(t, "--dry-run"), nil, plLogger(&logs))
	if !errors.Is(err, boom) || len(logs.lines("ingest finished")) != 0 {
		t.Errorf("err=%v logs=%q", err, logs.String())
	}
}
