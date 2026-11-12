// Command ingest loads the Open Food Facts CSV into OpenSearch.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch"
)

const (
	// maxFailedPercent is the share of passed documents that may fail to be
	// indexed while the new index is still published.
	maxFailedPercent = 1
	// gcPercent trades a little CPU for a lower peak RSS (44 vs 51 MB on the
	// adversarial input, see the proposal, Decisions 23).
	gcPercent = 50
	// cleanupTimeout bounds the deletion of an index that is not published.
	cleanupTimeout = 30 * time.Second
	// minCoveragePct is the coverage under which a run stopped by --limit warns.
	minCoveragePct = 90
)

// deps are the collaborators of run that tests replace.
type deps struct {
	client     *http.Client // used for https inputs
	newIndexer func(ctx context.Context, addr string) (catalog.Indexer, error)
}

// defaultDeps builds the production collaborators.
func defaultDeps() deps {
	return deps{
		client: newHTTPClient(),
		newIndexer: func(ctx context.Context, addr string) (catalog.Indexer, error) {
			c, err := opensearch.New(ctx, addr)
			if err != nil {
				return nil, err // not c: a nil *Client in the interface would not be nil
			}
			return c, nil
		},
	}
}

// tuneGC lowers the GC percent to keep the peak RSS under the 64 MB budget.
func tuneGC() { debug.SetGCPercent(gcPercent) }

// signalContext is cancelled by the first SIGINT or SIGTERM. It then stops
// listening, so a second signal gets the default behaviour and kills the process
// (signal.Stop restores it once no channel is registered).
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// main runs the command with the process arguments and exits with its code.
// The report goes to stdout; logs and errors go to stderr.
func main() {
	tuneGC()
	ctx, stop := signalContext()
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDeps())
	stop()
	os.Exit(code)
}

// shouldPublish is the publish rule: something passed and at most
// maxFailedPercent of it failed (integer maths, so under 100 documents no
// failure is tolerated).
func shouldPublish(passed, failed int) bool {
	return passed > 0 && failed*100 <= passed*maxFailedPercent
}

// run executes the command and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	cfg, err := parseConfig(args)
	if err != nil {
		var raw rawFlags
		fs := newFlagSet(stderr, &raw)
		if errors.Is(err, flag.ErrHelp) {
			fs.Usage()
			return exitOK
		}
		_, _ = fmt.Fprintf(stderr, "ingest: %v\n", err)
		fs.Usage()
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	a := &app{ctx: ctx, cfg: cfg, d: d, stdout: stdout, stderr: stderr, log: log}
	return a.run()
}

// app is one execution of the command.
type app struct {
	ctx            context.Context
	cfg            config
	d              deps
	stdout, stderr io.Writer
	log            *slog.Logger
}

// fail prints one error line and returns the exit code: 130 when the run was
// interrupted, 1 otherwise.
func (a *app) fail(err error) int {
	_, _ = fmt.Fprintf(a.stderr, "ingest: %v\n", err)
	if a.ctx.Err() != nil {
		return exitInterrupted
	}
	return exitFailure
}

func (a *app) run() int {
	src, err := openInput(a.ctx, a.d.client, a.cfg.Input)
	if err != nil {
		return a.fail(err)
	}
	defer func() { _ = src.Close() }()
	lr := newLineReader(src, 0)
	hdr, err := readHeader(lr)
	if err != nil {
		return a.fail(err)
	}
	rep := newReport(a.cfg, redactedInput(a.cfg.Input))
	in := progressLines{lr, src}

	if a.cfg.DryRun {
		if err := runPipeline(a.ctx, in, hdr, a.cfg, nil, "", rep, a.log); err != nil {
			a.writeReport(rep)
			return a.fail(err)
		}
		a.warnCoverage(rep)
		return a.finish(rep)
	}

	idx, err := a.d.newIndexer(a.ctx, a.cfg.OpenSearch)
	if err != nil {
		return a.fail(err)
	}
	index, err := idx.CreateIndex(a.ctx)
	if err != nil {
		return a.fail(err)
	}
	rep.index = index
	err = runPipeline(a.ctx, in, hdr, a.cfg, idx, index, rep, a.log)
	if err == nil {
		err = a.seal(idx, index, rep)
	}
	if err != nil {
		a.writeReport(rep)
		a.cleanup(idx, index)
		return a.fail(err)
	}
	a.warnCoverage(rep)
	if !shouldPublish(rep.passed, rep.failed) {
		a.writeReport(rep)
		a.cleanup(idx, index)
		if rep.passed == 0 {
			return a.fail(errors.New("no document passed the filters: nothing to publish"))
		}
		return a.fail(fmt.Errorf("not publishing %s: %d of %d documents failed (at most %d%% allowed)",
			index, rep.failed, rep.passed, maxFailedPercent))
	}
	if err := idx.Publish(a.ctx, index); err != nil {
		a.writeReport(rep)
		if errors.Is(err, opensearch.ErrPublishStateUnknown) {
			// The new index may be live behind the alias: never delete it.
			a.log.Error("publish state unknown; the new index is left in place, check where the products alias points",
				"index", index)
		} else {
			a.cleanup(idx, index)
		}
		return a.fail(err)
	}
	rep.published = true
	if rep.failed > 0 {
		a.log.Warn("published with failed documents", "index", index, "failed", rep.failed, "passed", rep.passed)
	}
	return a.finish(rep)
}

// seal makes the documents searchable, records the document count and checks
// the report invariants.
func (a *app) seal(idx catalog.Indexer, index string, rep *report) error {
	if err := idx.Finalize(a.ctx, index); err != nil {
		return err
	}
	n, err := idx.Count(a.ctx, index)
	if err != nil {
		return err
	}
	rep.docCount, rep.hasDocCount = n, true
	return rep.check()
}

// finish prints the report of a successful run.
func (a *app) finish(rep *report) int {
	if err := rep.check(); err != nil {
		a.log.Error("report check failed", "error", err)
	}
	a.writeReport(rep)
	return exitOK
}

// writeReport prints the report on stdout; a write error is only logged.
func (a *app) writeReport(rep *report) {
	if err := rep.write(a.stdout); err != nil {
		a.log.Error("write report", "error", err)
	}
}

// cleanup deletes the new index with a live context, even when the run was
// cancelled. The alias is never touched here.
func (a *app) cleanup(idx catalog.Indexer, index string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), cleanupTimeout)
	defer cancel()
	if err := idx.Delete(ctx, index); err != nil {
		a.log.Error("delete unpublished index", "index", index, "error", err)
	}
}

// warnCoverage warns when --limit stopped the read early: the file is sorted by
// code, so the suggestion extrapolates from one code range.
func (a *app) warnCoverage(rep *report) {
	if !rep.limitReached {
		return
	}
	_, suggested, ok := rep.estimate()
	if !ok {
		return
	}
	pct := float64(min(rep.bytesRead, rep.bytesTotal)) * 100 / float64(rep.bytesTotal)
	if pct >= minCoveragePct {
		return
	}
	a.log.Warn("stopped by --limit before reading the whole input; the projection extrapolates from one code range",
		"coverage_pct", fmt.Sprintf("%.1f", pct), "suggested_sample_every", suggested)
}

// readHeader reads and parses the header line.
func readHeader(lr *lineReader) (header, error) {
	line, tooLong, err := lr.next()
	switch {
	case errors.Is(err, io.EOF):
		return header{}, errors.New("input is empty")
	case err != nil:
		return header{}, fmt.Errorf("read header: %w", err)
	case tooLong:
		return header{}, errors.New("header line is too long")
	}
	h, err := parseHeader(line)
	if err != nil {
		return header{}, fmt.Errorf("header: %w", err)
	}
	return h, nil
}

// redactedInput is the --input value without userinfo, query or fragment.
func redactedInput(input string) string {
	if u, err := classifyInput(input); err == nil && u != nil {
		return redactURL(u)
	}
	return input
}
