package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const (
	// progressEvery is how many lines pass between updates of rep.bytesRead.
	progressEvery = 4096
	// logEvery is how many lines pass between INFO progress logs.
	logEvery = 500000
	// maxMergedFailureTypes caps the named failure types kept in the report; a
	// further type is counted under otherFailureType. Same rule as the adapter
	// (internal/opensearch/bulk.go): 32 named types plus "other", 33 keys at most.
	maxMergedFailureTypes = 32
	otherFailureType      = "other"
	// warnSampleIDs is how many failed ids one WARN log line shows.
	warnSampleIDs = 5
)

// lines is the source of input lines. The Item 5 line reader implements it.
type lines interface {
	next() (line []byte, tooLong bool, err error)
}

// progress is an optional capability of a lines value: how far the underlying
// input was consumed. *source implements it; a test fake may not.
type progress interface {
	BytesRead() int64
	Size() int64
}

// progressLines joins a line reader and its source, so that the source's
// progress reaches the pipeline: progressLines{newLineReader(src, 0), src}.
type progressLines struct {
	lines
	progress
}

// pipeline is the state of one runPipeline call.
type pipeline struct {
	rep *report
	idx catalog.Indexer
	log *slog.Logger

	mu sync.Mutex // guards rep.indexed, failed, failureTypes and failedIDs

	// readFailed is set by the producer when it cancelled the in-flight Bulk
	// calls because of its own read error. Only the runPipeline goroutine uses it.
	readFailed bool

	// indexed and failed mirror rep.indexed and rep.failed for the progress log,
	// which runs in the producer while merges are still happening.
	indexed, failed atomic.Int64
}

// runPipeline reads src until the end of the input or until cfg.Limit rows have
// passed every filter, and bulk-indexes the passed rows into index through idx.
//
// Contract (for the caller, Item 10):
//   - rep must come from newReport; runPipeline fills its counters: rows_read,
//     buckets, passed (through rep.add), limitReached, complete, bytesRead,
//     bytesTotal (only when src also implements progress), indexed, failed,
//     failureTypes and failedIDs (at most 10). It never sets docCount, index or
//     published. The report is only touched by this goroutine and, under a
//     mutex, by the bulk goroutines; it is safe to read after runPipeline returns.
//   - hdr describes the header line, which the caller has already consumed from src.
//   - idx == nil means dry run: nothing is batched, Bulk is never called, the
//     counting and the limit work the same. A typed nil is not detected.
//   - A nil log discards.
//   - Normal end: end of input, or the limit reached (the last batch is flushed
//     and awaited, rep.limitReached is set). Only then rep.complete is set and the
//     error is nil. With an indexer, complete means passed == indexed + failed.
//   - Any error (cancellation, a read error, a Bulk error of the whole request,
//     truncated input) leaves complete false and the counters as they were when
//     the run stopped: rows read so far, documents of the batches that finished.
//     The partial batch being built is dropped. Every bulk goroutine has finished
//     when runPipeline returns, error or not. A cancelled ctx gives an error for
//     which errors.Is(err, context.Canceled) holds; a Bulk error is wrapped with
//     its batch number and keeps its cause.
//   - Failures of single documents (BulkResult.Failed) do not stop the run.
//   - A Bulk result must account for its whole batch: Indexed + Failed equal to
//     the number of documents sent, neither negative. Otherwise it is a fatal
//     error of that batch ("bulk batch N: result accounts for X of M documents"),
//     handled like a Bulk error (the other calls in flight are cancelled), the
//     inconsistent result is not added to the report and complete stays false.
//   - A read error of src cancels the Bulk calls in flight, which stops them
//     sooner; the read error is the one returned and the report keeps the counts.
//   - An empty input (the header read returned io.EOF) is the caller's case
//     (Item 10); runPipeline starts after the header.
//   - Truncation check: at a clean end of input (not when the limit stopped the
//     run, not on an error), before the last partial batch is sent, so that batch
//     is dropped like on any other error. If src implements progress with Size() > 0 and
//     BytesRead() != Size(), the run fails: "input ended after N of M bytes". It
//     catches a plain local file that ends before the size it was opened with, or
//     a source that reports a clean end early. Limits: Size() <= 0 (unknown)
//     skips it; a plain file cut exactly at a line boundary before the open cannot
//     be detected; truncated gzip and HTTP bodies are errors of the reader itself.
//
// Memory: the producer holds one batch being built, at most workers batches are
// in flight (g.Go blocks when they are all busy, which is the backpressure that
// keeps the reader from running ahead), see batchBytes. g.Go is not
// context-aware: errgroup v0.23.0 blocks on its semaphore until an in-flight Bulk
// returns. The adapter bounds every Bulk call with its context and a 60 s
// timeout, so the producer cannot hang with the real adapter.
func runPipeline(
	ctx context.Context, src lines, hdr header, cfg config,
	idx catalog.Indexer, index string, rep *report, log *slog.Logger,
) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &pipeline{rep: rep, idx: idx, log: log}
	prog, _ := src.(progress)
	if prog != nil {
		rep.bytesTotal = prog.Size()
		defer func() { rep.bytesRead = prog.BytesRead() }()
	}
	start := time.Now()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	// stop lets the producer cancel the Bulk calls in flight; g.Wait releases bctx.
	bctx, stop := context.WithCancel(gctx)
	perr := p.produce(bctx, stop, g, src, prog, hdr, cfg, index, start)
	// Always wait: no bulk goroutine may outlive the call. A Bulk error is the
	// root cause of a producer that stopped on the cancellation it triggered; a
	// read error is the root cause of the Bulk errors it triggered itself.
	werr := g.Wait()
	if p.readFailed {
		return perr
	}
	if werr != nil {
		return werr
	}
	if perr != nil {
		return perr
	}
	rep.complete = true
	log.Info("ingest finished", "rows_read", rep.rowsRead, "passed", rep.passed,
		"indexed", rep.indexed, "failed", rep.failed, "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}

// produce reads, decodes, filters and batches. It returns nil at a normal end.
func (p *pipeline) produce(
	ctx context.Context, stop context.CancelFunc, g *errgroup.Group, src lines, prog progress,
	hdr header, cfg config, index string, start time.Time,
) error {
	var b batcher
	nbatch := 0
	send := func(batch []catalog.Product) {
		if batch == nil {
			return
		}
		nbatch++
		n := nbatch
		g.Go(func() error {
			res, err := p.idx.Bulk(ctx, index, batch)
			if err != nil {
				return fmt.Errorf("bulk batch %d (%d documents): %w", n, len(batch), err)
			}
			if res.Indexed < 0 || res.Failed < 0 || res.Indexed+res.Failed != len(batch) {
				return fmt.Errorf("bulk batch %d: result accounts for %d of %d documents",
					n, res.Indexed+res.Failed, len(batch))
			}
			p.merge(res)
			return nil
		})
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ingest cancelled after %d rows: %w", p.rep.rowsRead, err)
		}
		line, tooLong, err := src.next()
		if errors.Is(err, io.EOF) {
			if err := truncated(prog); err != nil {
				return err
			}
			send(b.take())
			return nil
		}
		if err != nil {
			if ctx.Err() == nil { // not already stopped by a Bulk error or a cancel
				p.readFailed = true
				stop()
			}
			return fmt.Errorf("read input after %d rows: %w", p.rep.rowsRead, err)
		}
		var rec record
		var reason skipReason
		if !tooLong {
			rec, reason = hdr.decode(line)
		}
		passed := p.rep.add(tooLong, rec, reason)
		p.tick(prog, start)
		if !passed {
			continue
		}
		if p.idx != nil {
			send(b.add(rec.product()))
		}
		if cfg.Limit > 0 && p.rep.passed == cfg.Limit {
			p.rep.limitReached = true
			send(b.take())
			return nil
		}
	}
}

// tick runs after every line: it refreshes the byte counters every
// progressEvery lines and logs progress every logEvery lines.
func (p *pipeline) tick(prog progress, start time.Time) {
	if prog != nil && p.rep.rowsRead%progressEvery == 0 {
		p.rep.bytesRead = prog.BytesRead()
	}
	if p.rep.rowsRead%logEvery == 0 {
		p.log.Info("ingest progress", "rows_read", p.rep.rowsRead, "passed", p.rep.passed,
			"indexed", p.indexed.Load(), "failed", p.failed.Load(), "elapsed", time.Since(start).Round(time.Millisecond))
	}
}

// truncated reports a clean end of input that came before the size the source
// was opened with. See the truncation check on runPipeline.
func truncated(prog progress) error {
	if prog == nil {
		return nil
	}
	if size := prog.Size(); size > 0 && prog.BytesRead() != size {
		return fmt.Errorf("input ended after %d of %d bytes: the file or response is truncated", prog.BytesRead(), size)
	}
	return nil
}

// merge adds the result of one Bulk call to the report. Failed ids are kept in
// completion order, at most maxFailedIDs; failure types are kept by name, at
// most maxMergedFailureTypes of them, the rest under "other".
func (p *pipeline) merge(res catalog.BulkResult) {
	p.mu.Lock()
	p.rep.indexed += res.Indexed
	p.rep.failed += res.Failed
	p.indexed.Add(int64(res.Indexed))
	p.failed.Add(int64(res.Failed))
	for _, typ := range slices.Sorted(maps.Keys(res.FailureTypes)) {
		p.addFailureType(typ, res.FailureTypes[typ])
	}
	room := maxFailedIDs - len(p.rep.failedIDs)
	p.rep.failedIDs = append(p.rep.failedIDs, res.FailedIDs[:min(room, len(res.FailedIDs))]...)
	p.mu.Unlock()
	if res.Failed > 0 {
		p.log.Warn("bulk batch had failed documents", "failed", res.Failed, "indexed", res.Indexed,
			"failure_types", res.FailureTypes, "sample_ids", res.FailedIDs[:min(warnSampleIDs, len(res.FailedIDs))])
	}
}

// addFailureType counts n failures of typ; the caller holds p.mu.
func (p *pipeline) addFailureType(typ string, n int) {
	ft := p.rep.failureTypes
	if ft == nil {
		ft = map[string]int{}
		p.rep.failureTypes = ft
	}
	if _, known := ft[typ]; !known {
		named := len(ft)
		if _, hasOther := ft[otherFailureType]; hasOther {
			named--
		}
		if named >= maxMergedFailureTypes {
			typ = otherFailureType
		}
	}
	ft[typ] += n
}
