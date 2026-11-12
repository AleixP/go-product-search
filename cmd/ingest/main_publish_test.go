package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/opensearch"
)

func TestRun_PublishStateUnknownNeverDeletesTheIndex(t *testing.T) {
	f := &mwIdx{publishErr: fmt.Errorf("%w: swap alias products failed; check the products alias", opensearch.ErrPublishStateUnknown)}
	d, _ := mwDeps(f, nil)

	code, stdout, stderr := mwRun(context.Background(), d, "--input", mwFixture(), "--sample-every", "1", "--limit", "0")

	if code != exitFailure {
		t.Errorf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted = %v, want none: the new index may be live", f.deleted)
	}
	if f.published != 1 {
		t.Errorf("published = %d, want 1", f.published)
	}
	if !strings.Contains(stdout, "published=false") {
		t.Errorf("report lacks published=false:\n%s", stdout)
	}
	for _, want := range []string{"level=ERROR", mwIndex, "products alias"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestRun_PlainPublishErrorStillDeletesTheIndex(t *testing.T) {
	f := &mwIdx{publishErr: errors.New("boom")}
	d, _ := mwDeps(f, nil)

	code, _, _ := mwRun(context.Background(), d, "--input", mwFixture(), "--sample-every", "1", "--limit", "0")

	if code != exitFailure || len(f.deleted) != 1 || f.deleted[0] != mwIndex {
		t.Errorf("exit=%d deleted=%v, want 1 and [%s]", code, f.deleted, mwIndex)
	}
}

func TestRun_CleanupDeadlineIsThirtySeconds(t *testing.T) {
	f := &mwIdx{publishErr: errors.New("boom")}
	d, _ := mwDeps(f, nil)

	before := time.Now()
	mwRun(context.Background(), d, "--input", mwFixture(), "--sample-every", "1", "--limit", "0")

	if len(f.deleteDeadline) != 1 {
		t.Fatalf("deletes = %d, want 1", len(f.deleteDeadline))
	}
	got := f.deleteDeadline[0].Sub(before)
	if got < 30*time.Second || got >= 31*time.Second {
		t.Errorf("Delete deadline is %v away, want [30s, 31s)", got)
	}
}

func TestWarnCoverage_Boundary(t *testing.T) {
	tests := []struct {
		name      string
		bytesRead int64
		wantWarn  bool
	}{
		{"exactly 90 percent does not warn", 900, false},
		{"just below 90 percent warns", 899, true},
		{"fully read does not warn", 1000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			a := &app{log: slog.New(slog.NewTextHandler(&logs, nil))}
			rep := &report{limitReached: true, limit: 10, passed: 10, sampleEvery: 1, bytesRead: tt.bytesRead, bytesTotal: 1000}

			a.warnCoverage(rep)

			if got := strings.Contains(logs.String(), "level=WARN"); got != tt.wantWarn {
				t.Errorf("warn = %v, want %v; log:\n%s", got, tt.wantWarn, logs.String())
			}
		})
	}
}
