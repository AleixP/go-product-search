package opensearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// waitFor polls cond until it holds or the bound passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// withAnalyzeBound sets the bound New reads, for the clients the test creates.
func withAnalyzeBound(t *testing.T, n int) {
	t.Helper()
	old := analyzeBound
	analyzeBound = n
	t.Cleanup(func() { analyzeBound = old })
}

// gatedAnalyze makes the fake block every analyze request until release is
// closed, and tracks how many are inside at once.
type gatedAnalyze struct {
	release chan struct{}
	once    sync.Once
	cur     atomic.Int32
	max     atomic.Int32
	total   atomic.Int32
}

func newGate(t *testing.T) *gatedAnalyze {
	g := &gatedAnalyze{release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gatedAnalyze) open() { g.once.Do(func() { close(g.release) }) }

func (g *gatedAnalyze) handler(w http.ResponseWriter, r *http.Request, texts []string, _ int) {
	g.total.Add(1)
	n := g.cur.Add(1)
	defer g.cur.Add(-1)
	for {
		m := g.max.Load()
		if n <= m || g.max.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-g.release:
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
	}
	writeJSON(w, tokensBody(foldAll(texts)...))
}

var brandQuery = catalog.Query{Page: 1, Size: 20, Brands: []string{"SECRETVALUE"}}

func TestAnalyzeBound_DefaultIsEight(t *testing.T) {
	if maxAnalyzeInFlight != 8 {
		t.Errorf("maxAnalyzeInFlight = %d, want 8 (half the engine analyze queue of 16)", maxAnalyzeInFlight)
	}
	_, c, _ := newEngine(t)
	if got := cap(c.analyzeSlots); got != maxAnalyzeInFlight {
		t.Errorf("slots = %d, want %d", got, maxAnalyzeInFlight)
	}
}

func TestSearch_AtMostTheBoundOfAnalyzeCallsAreInFlight(t *testing.T) {
	e, c, _ := newEngine(t)
	g := newGate(t)
	e.analyze = g.handler
	callers := 3 * maxAnalyzeInFlight
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, err := c.Search(context.Background(), brandQuery)
			errs <- err
		}()
	}
	waitFor(t, "the bound to fill", func() bool { return g.cur.Load() == int32(maxAnalyzeInFlight) })
	// Give the callers beyond the bound time to (wrongly) get through.
	time.Sleep(100 * time.Millisecond)
	if got := g.cur.Load(); got != int32(maxAnalyzeInFlight) {
		t.Errorf("in flight while blocked = %d, want %d", got, maxAnalyzeInFlight)
	}
	g.open()
	for i := 0; i < callers; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("Search error = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d callers finished", i, callers)
		}
	}
	if got := g.max.Load(); got != int32(maxAnalyzeInFlight) {
		t.Errorf("max concurrent analyze requests = %d, want %d", got, maxAnalyzeInFlight)
	}
	if got := g.total.Load(); got != int32(callers) {
		t.Errorf("analyze requests = %d, want %d", got, callers)
	}
	if got := len(c.analyzeSlots); got != 0 {
		t.Errorf("slots held after all callers finished = %d, want 0", got)
	}
}

func TestSearch_ContextEndingWhileWaitingForASlotMakesNoRequest(t *testing.T) {
	withAnalyzeBound(t, 1)
	e, c, _ := newEngine(t)
	g := newGate(t)
	e.analyze = g.handler
	first := make(chan error, 1)
	go func() {
		_, err := c.Search(context.Background(), brandQuery)
		first <- err
	}()
	waitFor(t, "the first call to hold the slot", func() bool { return g.cur.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Search(ctx, brandQuery)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Search error = %v, want one wrapping context.DeadlineExceeded", err)
	}
	if want := "search products: normalize filters: waiting for an analyze slot: context deadline exceeded"; err == nil || err.Error() != want {
		t.Errorf("Search error = %v, want %q", err, want)
	}
	if got := g.total.Load(); got != 1 {
		t.Errorf("analyze requests = %d, want 1 (the waiting caller sends none)", got)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Search(cctx, brandQuery); done <- err }()
	time.Sleep(20 * time.Millisecond)
	ccancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Search error = %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled waiter did not return")
	}

	g.open()
	select {
	case err := <-first:
		if err != nil {
			t.Errorf("first Search error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first call did not finish")
	}
	if got := len(c.analyzeSlots); got != 0 {
		t.Errorf("slots held = %d, want 0", got)
	}
}

func TestSearch_EverySlotIsReleasedOnEveryPath(t *testing.T) {
	structErr := func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"root_cause":[],"type":"x","reason":"y"},"status":400}`)
	}
	tests := []struct {
		name    string
		analyze func(http.ResponseWriter, *http.Request, []string, int)
		ctx     func() (context.Context, context.CancelFunc)
		wantErr bool
	}{
		{name: "success"},
		{name: "engine error", analyze: structErr, wantErr: true},
		{name: "token count mismatch", wantErr: true, analyze: func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) { writeJSON(w, tokensBody()) }},
		{name: "empty token", wantErr: true, analyze: func(w http.ResponseWriter, _ *http.Request, _ []string, _ int) { writeJSON(w, tokensBody("")) }},
		{name: "cancelled before the call", wantErr: true, ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withAnalyzeBound(t, 1)
			e, c, _ := newEngine(t)
			e.analyze = tt.analyze
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tt.ctx != nil {
				ctx, cancel = tt.ctx()
			}
			defer cancel()
			for i := 0; i < 3; i++ { // a leaked slot blocks the second call
				done := make(chan error, 1)
				go func() { _, err := c.Search(ctx, brandQuery); done <- err }()
				select {
				case err := <-done:
					if (err != nil) != tt.wantErr {
						t.Fatalf("call %d: error = %v, wantErr %v", i, err, tt.wantErr)
					}
					if err != nil && strings.Contains(err.Error(), "SECRETVALUE") {
						t.Errorf("error leaks a value: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("call %d blocked: the slot was not released", i)
				}
				if got := len(c.analyzeSlots); got != 0 {
					t.Errorf("call %d: slots held = %d, want 0", i, got)
				}
			}
		})
	}
}
