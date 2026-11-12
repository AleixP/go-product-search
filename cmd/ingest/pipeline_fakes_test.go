package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const plWait = 10 * time.Second // upper bound of every positive wait; never a margin

// plHeader is the header of the synthetic rows made by plGood.
func plHeader(t *testing.T) header {
	t.Helper()
	h, err := parseHeader([]byte("code\tproduct_name\tbrands\tcategories_tags\tcountries_tags\tnutriscore_grade"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// plGood is a row that passes the default filters of plCfg. Codes sort like i.
func plGood(i int) []byte {
	return fmt.Appendf(nil, "c%07d\tName %d\tBrand\ten:snacks\ten:united-states\ta", i, i)
}

// plCfg is a configuration with every row sampled, no limit and the default
// filters; args override.
func plCfg(t *testing.T, args ...string) config {
	t.Helper()
	cfg, err := parseConfig(append([]string{"--sample-every", "1", "--limit", "0"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// plLines is an in-memory lines source.
type plLines struct {
	n       int                // lines before EOF
	line    func(i int) []byte // content of line i (0-based); default plGood
	tooLong func(i int) bool
	failAt  int   // 1-based read that fails, 0 = never
	failErr error // returned by that read
	onNext  func(i int)
	reads   atomic.Int64 // lines served
	calls   atomic.Int64 // calls of next, EOF included
}

func (f *plLines) next() ([]byte, bool, error) {
	i := int(f.calls.Add(1)) - 1
	if f.onNext != nil {
		f.onNext(i)
	}
	if f.failAt > 0 && i+1 == f.failAt {
		return nil, false, f.failErr
	}
	if i >= f.n {
		return nil, false, io.EOF
	}
	f.reads.Add(1)
	if f.tooLong != nil && f.tooLong(i) {
		return nil, true, nil
	}
	if f.line != nil {
		return f.line(i), false, nil
	}
	return plGood(i), false, nil
}

// plProg adds the optional progress interface to a source.
type plProg struct {
	*plLines
	size int64
	read func() int64
}

func (p plProg) BytesRead() int64 { return p.read() }
func (p plProg) Size() int64      { return p.size }

// plIndexer is a fake catalog.Indexer. Only Bulk is used by the pipeline.
type plIndexer struct {
	hook func(ctx context.Context, docs []catalog.Product) (catalog.BulkResult, error)

	mu      sync.Mutex
	calls   int
	cur     int
	maxCur  int
	indices []string
	batches [][]catalog.Product // the slices exactly as received (aliasing check)
	copies  [][]catalog.Product // a copy taken at call time
}

func (f *plIndexer) Bulk(ctx context.Context, index string, docs []catalog.Product) (catalog.BulkResult, error) {
	f.mu.Lock()
	f.calls++
	f.cur++
	f.maxCur = max(f.maxCur, f.cur)
	f.indices = append(f.indices, index)
	f.batches = append(f.batches, docs)
	f.copies = append(f.copies, append([]catalog.Product(nil), docs...))
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.cur--
		f.mu.Unlock()
	}()
	if f.hook != nil {
		return f.hook(ctx, docs)
	}
	return catalog.BulkResult{Indexed: len(docs)}, nil
}

func (f *plIndexer) CreateIndex(context.Context) (string, error) { panic("unused") }
func (f *plIndexer) Finalize(context.Context, string) error      { panic("unused") }
func (f *plIndexer) Count(context.Context, string) (int, error)  { panic("unused") }
func (f *plIndexer) Publish(context.Context, string) error       { panic("unused") }
func (f *plIndexer) Delete(context.Context, string) error        { panic("unused") }

// sizes returns the received batch sizes ordered by first document id.
func (f *plIndexer) sizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	bs := append([][]catalog.Product(nil), f.copies...)
	sort.Slice(bs, func(i, j int) bool { return bs[i][0].ID < bs[j][0].ID })
	out := make([]int, len(bs))
	for i, b := range bs {
		out[i] = len(b)
	}
	return out
}

// plRun runs the pipeline with a fresh report.
func plRun(ctx context.Context, t *testing.T, src lines, cfg config, idx catalog.Indexer, log *slog.Logger) (*report, error) {
	t.Helper()
	rep := newReport(cfg, "test-input")
	err := runPipeline(ctx, src, plHeader(t), cfg, idx, "products-test", rep, log)
	return rep, err
}

// plNoLeak fails the test when goroutines started by the pipeline are still
// alive after it returned. Call it first: it registers a Cleanup.
func plNoLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(plWait)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			runtime.Gosched()
			time.Sleep(time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > before {
			t.Errorf("goroutines: %d before, %d after the pipeline returned: leak", before, n)
		}
	})
}

// plLogs is a slog handler target safe for concurrent writers.
type plLogs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *plLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *plLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines returns the log lines containing every want substring.
func (l *plLogs) lines(want ...string) []string {
	var out []string
	for _, ln := range strings.Split(l.String(), "\n") {
		ok := ln != ""
		for _, w := range want {
			ok = ok && strings.Contains(ln, w)
		}
		if ok {
			out = append(out, ln)
		}
	}
	return out
}

func plLogger(l *plLogs) *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// plWaitCh waits for ch or fails the test.
func plWaitCh(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(plWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func plEqualInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
