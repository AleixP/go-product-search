package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProbeDocker(t *testing.T) {
	const wait = 100 * time.Millisecond
	boom := errors.New("boom")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // lets the ignoring probes below return

	tests := []struct {
		name    string
		health  func(context.Context) error
		wantErr string // "" means nil
	}{
		{"healthy", func(context.Context) error { return nil }, ""},
		{"error is passed through", func(context.Context) error { return boom }, "boom"},
		{"panic is recovered", func(context.Context) error { panic("no docker host") }, "no docker host"},
		{"hung probe that honours its context", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, "did not answer within"},
		{"hung probe that ignores its context", func(context.Context) error { <-release; return nil }, "did not answer within"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			err := probeDocker(context.Background(), wait, tt.health)
			if d := time.Since(start); d > 10*wait {
				t.Errorf("took %v, want about %v at most", d, wait)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("probeDocker = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("probeDocker = %v, want an error with %q", err, tt.wantErr)
			}
		})
	}
}

func TestProbeDocker_ParentCancelEndsTheProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := probeDocker(ctx, time.Minute, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if err == nil {
		t.Fatal("probeDocker = nil, want an error for a cancelled parent")
	}
}
