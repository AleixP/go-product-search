package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const mwIndex = "products-20260101t000000z"

// mwIdx is a fake catalog.Indexer that records the lifecycle calls.
type mwIdx struct {
	mu          sync.Mutex
	bulk        func(ctx context.Context, docs []catalog.Product) (catalog.BulkResult, error)
	createErr   error
	finalizeErr error
	countErr    error
	publishErr  error

	created, published int
	deleted            []string
	deleteCtxErr       []error // ctx.Err() seen by each Delete
	deleteHasDeadline  []bool
	deleteDeadline     []time.Time
	accepted           int
}

func (f *mwIdx) CreateIndex(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	if f.createErr != nil {
		return "", f.createErr
	}
	return mwIndex, nil
}

func (f *mwIdx) Bulk(ctx context.Context, _ string, docs []catalog.Product) (catalog.BulkResult, error) {
	var res catalog.BulkResult
	if f.bulk != nil {
		var err error
		if res, err = f.bulk(ctx, docs); err != nil {
			return res, err
		}
	} else {
		res = catalog.BulkResult{Indexed: len(docs)}
	}
	f.mu.Lock()
	f.accepted += res.Indexed
	f.mu.Unlock()
	return res, nil
}

func (f *mwIdx) Finalize(context.Context, string) error { return f.finalizeErr }

func (f *mwIdx) Count(context.Context, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted, f.countErr
}

func (f *mwIdx) Publish(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published++
	return f.publishErr
}

func (f *mwIdx) Delete(ctx context.Context, index string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, index)
	f.deleteCtxErr = append(f.deleteCtxErr, ctx.Err())
	dl, ok := ctx.Deadline()
	f.deleteDeadline = append(f.deleteDeadline, dl)
	f.deleteHasDeadline = append(f.deleteHasDeadline, ok)
	return nil
}

// mwDeps returns deps whose factory hands out f and counts its calls.
func mwDeps(f *mwIdx, factoryErr error) (deps, *int) {
	calls := new(int)
	return deps{
		client: http.DefaultClient,
		newIndexer: func(context.Context, string) (catalog.Indexer, error) {
			*calls++
			if factoryErr != nil {
				return nil, factoryErr
			}
			return f, nil
		},
	}, calls
}

func mwRun(ctx context.Context, d deps, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(ctx, args, &out, &errb, d)
	return code, out.String(), errb.String()
}

func mwFixture() string { return filepath.Join("..", "..", "testdata", "products.tsv") }

const mwHeader = "code\tproduct_name\tbrands\tcategories_tags\tcountries_tags\tnutriscore_grade"

// mwWrite writes a TSV with the given header and n rows that pass the default filters.
func mwWrite(t *testing.T, header string, n int) string {
	t.Helper()
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n")
	}
	for i := range n {
		fmt.Fprintf(&b, "c%07d\tName %d\tBrand\ten:snacks\ten:united-states\ta\n", i, i)
	}
	path := filepath.Join(t.TempDir(), "in.tsv")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestShouldPublish(t *testing.T) {
	tests := []struct {
		passed, failed int
		want           bool
	}{
		{0, 0, false}, {100, 1, true}, {100, 2, false}, {50, 1, false}, {50, 0, true},
		{100000, 1000, true}, {100000, 1001, false}, {1, 0, true}, {99, 1, false},
	}
	for _, tt := range tests {
		if got := shouldPublish(tt.passed, tt.failed); got != tt.want {
			t.Errorf("shouldPublish(%d, %d) = %v, want %v", tt.passed, tt.failed, got, tt.want)
		}
	}
}

func TestTuneGC_SetsPercent50(t *testing.T) {
	prev := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(prev) })
	tuneGC()
	if got := debug.SetGCPercent(100); got != 50 {
		t.Errorf("GC percent after tuneGC = %d, want 50", got)
	}
}

func TestRun_PublishesTheFixture(t *testing.T) {
	f := &mwIdx{}
	d, _ := mwDeps(f, nil)
	code, stdout, stderr := mwRun(context.Background(), d, "--input", mwFixture(), "--sample-every", "1", "--limit", "0")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if f.created != 1 || f.published != 1 || len(f.deleted) != 0 {
		t.Errorf("created=%d published=%d deleted=%v, want 1 1 none", f.created, f.published, f.deleted)
	}
	for _, want := range []string{"rows_read=15\n", "passed=7\n", "indexed=7\n", "failed=0\n", "doc_count=7\n", "index=" + mwIndex + "\n", "published=true\n", "dry_run=false\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stderr, "level=ERROR") || strings.Contains(stderr, "level=WARN") {
		t.Errorf("clean run logged a problem:\n%s", stderr)
	}
}

func TestRun_TooManyFailuresDeletesAndExits1(t *testing.T) {
	f := &mwIdx{bulk: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		return catalog.BulkResult{Indexed: len(docs) - 1, Failed: 1, FailureTypes: map[string]int{"mapper_parsing_exception": 1}, FailedIDs: []string{docs[0].ID}}, nil
	}}
	d, _ := mwDeps(f, nil)
	code, stdout, _ := mwRun(context.Background(), d, "--input", mwFixture(), "--sample-every", "1", "--limit", "0")
	if code != exitFailure {
		t.Fatalf("exit = %d, want 1", code)
	}
	if f.published != 0 || len(f.deleted) != 1 || f.deleted[0] != mwIndex {
		t.Errorf("published=%d deleted=%v, want 0 and [%s]", f.published, f.deleted, mwIndex)
	}
	if !strings.Contains(stdout, "published=false\n") || !strings.Contains(stdout, "failed=1\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

func TestRun_ToleratedFailuresPublishWithWarning(t *testing.T) {
	var once sync.Once
	f := &mwIdx{bulk: func(_ context.Context, docs []catalog.Product) (catalog.BulkResult, error) {
		res := catalog.BulkResult{Indexed: len(docs)}
		once.Do(func() {
			res = catalog.BulkResult{Indexed: len(docs) - 2, Failed: 2, FailureTypes: map[string]int{"x": 2}}
		})
		return res, nil
	}}
	d, _ := mwDeps(f, nil)
	path := mwWrite(t, mwHeader, 200) // 2 of 200 = 1%
	code, stdout, stderr := mwRun(context.Background(), d, "--input", path, "--sample-every", "1", "--limit", "0")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if f.published != 1 || len(f.deleted) != 0 {
		t.Errorf("published=%d deleted=%v, want 1 none", f.published, f.deleted)
	}
	if !strings.Contains(stdout, "published=true\n") || !strings.Contains(stdout, "failed=2\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "published with failed documents") {
		t.Errorf("no WARN for tolerated failures:\n%s", stderr)
	}
}

func TestRun_NothingPassedIsAFailure(t *testing.T) {
	f := &mwIdx{}
	d, _ := mwDeps(f, nil)
	path := mwWrite(t, mwHeader, 0)
	code, _, _ := mwRun(context.Background(), d, "--input", path)
	if code != exitFailure || f.published != 0 || len(f.deleted) != 1 {
		t.Errorf("exit=%d published=%d deleted=%v, want 1 0 one delete", code, f.published, f.deleted)
	}
}

func TestRun_DryRunNeverUsesTheIndexer(t *testing.T) {
	f := &mwIdx{}
	d, calls := mwDeps(f, nil)
	code, stdout, stderr := mwRun(context.Background(), d, "--dry-run", "--input", mwFixture(), "--sample-every", "1", "--limit", "0")
	if code != exitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr)
	}
	if *calls != 0 || f.created != 0 || f.published != 0 || len(f.deleted) != 0 {
		t.Errorf("factory=%d created=%d published=%d deleted=%v, want all zero", *calls, f.created, f.published, f.deleted)
	}
	for _, want := range []string{"rows_read=15\n", "passed=7\n", "published=false\n", "dry_run=true\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "doc_count=") || strings.Contains(stdout, "index=") {
		t.Errorf("dry run reported an index:\n%s", stdout)
	}
}

func TestRun_InputProblemsExit1BeforeTouchingOpenSearch(t *testing.T) {
	tests := []struct {
		name, path, want string
	}{
		{"missing column", mwWrite(t, "code\tproduct_name", 3), "brands"},
		{"empty input", mwWrite(t, "", 0), "empty"},
		{"missing file", filepath.Join(t.TempDir(), "nope.tsv"), "nope.tsv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &mwIdx{}
			d, calls := mwDeps(f, nil)
			code, _, stderr := mwRun(context.Background(), d, "--input", tt.path)
			if code != exitFailure {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr, tt.want) || !strings.Contains(stderr, "ingest: ") {
				t.Errorf("stderr lacks %q:\n%s", tt.want, stderr)
			}
			if *calls != 0 || f.created != 0 {
				t.Errorf("OpenSearch touched: factory=%d created=%d", *calls, f.created)
			}
		})
	}
}

func TestRun_PingFailureExits1(t *testing.T) {
	f := &mwIdx{}
	d, _ := mwDeps(f, errors.New("ping OpenSearch at http://localhost:9200: connection refused"))
	code, _, stderr := mwRun(context.Background(), d, "--input", mwFixture())
	if code != exitFailure || f.created != 0 {
		t.Errorf("exit=%d created=%d, want 1 0", code, f.created)
	}
	if !strings.Contains(stderr, "connection refused") {
		t.Errorf("stderr lacks the cause:\n%s", stderr)
	}
}

func TestRun_StepErrorsDeleteTheIndex(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		f    *mwIdx
	}{
		{"finalize", &mwIdx{finalizeErr: boom}},
		{"count", &mwIdx{countErr: boom}},
		{"publish", &mwIdx{publishErr: boom}},
		{"bulk", &mwIdx{bulk: func(context.Context, []catalog.Product) (catalog.BulkResult, error) {
			return catalog.BulkResult{}, boom
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := mwDeps(tt.f, nil)
			code, _, stderr := mwRun(context.Background(), d, "--input", mwFixture(), "--sample-every", "1", "--limit", "0")
			if code != exitFailure {
				t.Fatalf("exit = %d, want 1", code)
			}
			if len(tt.f.deleted) != 1 || tt.f.deleted[0] != mwIndex {
				t.Errorf("deleted = %v, want [%s]", tt.f.deleted, mwIndex)
			}
			if !strings.Contains(stderr, "boom") {
				t.Errorf("stderr lacks the cause:\n%s", stderr)
			}
		})
	}
}

func TestRun_CreateIndexErrorExits1WithoutDelete(t *testing.T) {
	f := &mwIdx{createErr: errors.New("boom")}
	d, _ := mwDeps(f, nil)
	code, _, _ := mwRun(context.Background(), d, "--input", mwFixture())
	if code != exitFailure || len(f.deleted) != 0 || f.published != 0 {
		t.Errorf("exit=%d deleted=%v published=%d, want 1 none 0", code, f.deleted, f.published)
	}
}

func TestRun_CancelledContextCleansUpWithALiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	f := &mwIdx{bulk: func(ctx context.Context, _ []catalog.Product) (catalog.BulkResult, error) {
		close(started)
		<-ctx.Done()
		return catalog.BulkResult{}, ctx.Err()
	}}
	d, _ := mwDeps(f, nil)
	go func() { <-started; cancel() }()
	code, _, stderr := mwRun(ctx, d, "--input", mwFixture(), "--sample-every", "1", "--limit", "0")
	if code != exitInterrupted {
		t.Fatalf("exit = %d, want 130; stderr:\n%s", code, stderr)
	}
	if len(f.deleted) != 1 || f.published != 0 {
		t.Fatalf("deleted=%v published=%d, want one delete and no publish", f.deleted, f.published)
	}
	if f.deleteCtxErr[0] != nil || !f.deleteHasDeadline[0] {
		t.Errorf("Delete ctx err=%v deadline=%v, want a live context with a deadline", f.deleteCtxErr[0], f.deleteHasDeadline[0])
	}
}

func TestRun_PreCancelledDryRunExits130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, _ := mwDeps(&mwIdx{}, nil)
	code, _, _ := mwRun(ctx, d, "--dry-run", "--input", mwFixture())
	if code != exitInterrupted {
		t.Errorf("exit = %d, want 130", code)
	}
}

func TestRun_UsageErrorNeverBuildsAnything(t *testing.T) {
	d, calls := mwDeps(&mwIdx{}, nil)
	code, stdout, _ := mwRun(context.Background(), d, "--sample-every", "0")
	if code != exitUsage || *calls != 0 || stdout != "" {
		t.Errorf("exit=%d factory=%d stdout=%q, want 2 0 empty", code, *calls, stdout)
	}
}

func TestRun_TruncatedHTTPSBodyFailsAndDeletes(t *testing.T) {
	body := mwHeader + "\n" + "c0000001\tName\tBrand\ten:snacks\ten:united-states\ta\n"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+5000))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	f := &mwIdx{}
	d, _ := mwDeps(f, nil)
	d.client = srv.Client()
	code, _, stderr := mwRun(context.Background(), d, "--input", srv.URL+"/x.tsv", "--sample-every", "1", "--limit", "0")
	if code != exitFailure {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if f.published != 0 || len(f.deleted) != 1 {
		t.Errorf("published=%d deleted=%v, want 0 and one delete", f.published, f.deleted)
	}
}

func TestRun_CoverageWarningWhenTheLimitStopsEarly(t *testing.T) {
	path := mwWrite(t, mwHeader, 40000)
	d, _ := mwDeps(&mwIdx{}, nil)
	_, _, stderr := mwRun(context.Background(), d, "--dry-run", "--input", path, "--sample-every", "1", "--limit", "10")
	for _, want := range []string{"level=WARN", "suggested_sample_every=", "coverage_pct="} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	_, _, stderr = mwRun(context.Background(), d, "--dry-run", "--input", path, "--sample-every", "1", "--limit", "0")
	if strings.Contains(stderr, "suggested_sample_every") {
		t.Errorf("a full read warned about coverage:\n%s", stderr)
	}
	_, _, stderr = mwRun(context.Background(), d, "--dry-run", "--input", mwFixture(), "--sample-every", "1", "--limit", "3")
	if strings.Contains(stderr, "suggested_sample_every") {
		t.Errorf("a limit hit at 100%% coverage warned:\n%s", stderr)
	}
}

func TestRun_ReportInputIsRedacted(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(mwHeader + "\n"))
	}))
	t.Cleanup(srv.Close)
	d, _ := mwDeps(&mwIdx{}, nil)
	d.client = srv.Client()
	_, stdout, _ := mwRun(context.Background(), d, "--dry-run", "--input", srv.URL+"/x.tsv?token=s3cr3t")
	if strings.Contains(stdout, "s3cr3t") || !strings.Contains(stdout, "/x.tsv") {
		t.Errorf("report input not redacted:\n%s", stdout)
	}
}

func TestSignalContext_CancelsOnSIGTERM(t *testing.T) {
	ctx, stop := signalContext()
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("context not cancelled by SIGTERM")
	}
}

func TestDefaultDeps_AreBuilt(t *testing.T) {
	d := defaultDeps()
	if d.client == nil || d.newIndexer == nil {
		t.Fatalf("defaultDeps() = %+v, want both set", d)
	}
	if _, err := d.newIndexer(context.Background(), "ftp://x"); err == nil {
		t.Error("newIndexer accepted an ftp address")
	}
}
