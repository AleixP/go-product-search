package opensearch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestPublish_SwapFailureNeverDeletesOldIndices(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.failPaths["/_aliases"] = true
	c := newTestClient(t, f)

	if err := c.Publish(context.Background(), "products-new"); err == nil {
		t.Fatal("Publish() error = nil, want error when the alias swap fails")
	}
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "DELETE ") {
			t.Errorf("an index was deleted after a failed swap: %s", r)
		}
	}
}

func TestCreateIndex_ExistenceCheckFailureSendsNoCreate(t *testing.T) {
	f := newFakeCluster(t)
	f.failPaths["/products"] = true
	c := newTestClient(t, f)

	if name, err := c.CreateIndex(context.Background()); err == nil {
		t.Fatalf("CreateIndex() = %q, nil, want error when the existence check fails", name)
	}
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "PUT ") {
			t.Errorf("an index was created after a failed check: %s", r)
		}
	}
}

func TestNew_HTTPSAddressPassesValidation(t *testing.T) {
	// Nothing listens here; the failure must come from the ping, not from validation.
	_, err := New(context.Background(), "https://127.0.0.1:1")
	if err == nil {
		t.Fatal("New against a closed port: error = nil, want error")
	}
	if strings.Contains(err.Error(), "invalid OpenSearch address") {
		t.Errorf("https address rejected by validation: %v", err)
	}
}

func TestCreateIndex_NameUsesUTC(t *testing.T) {
	f := newFakeCluster(t)
	c := newTestClient(t, f)
	c.now = func() time.Time { return time.Date(2026, 11, 9, 13, 0, 0, 0, time.FixedZone("X", 3600)) }

	name, err := c.CreateIndex(context.Background())
	if err != nil {
		t.Fatalf("CreateIndex() error = %v", err)
	}
	if want := "products-20261109t120000z"; name != want {
		t.Errorf("CreateIndex() = %q, want %q", name, want)
	}
}

func TestPublish_ListFailureIsLoggedAndKeepsSwap(t *testing.T) {
	f := newFakeCluster(t)
	f.indices = []string{"products-old", "products-new"}
	f.aliased = []string{"products-old"}
	f.failPaths["/products-*"] = true
	c := newTestClient(t, f)
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))

	if err := c.Publish(context.Background(), "products-new"); err != nil {
		t.Fatalf("Publish() error = %v, want nil (best-effort cleanup)", err)
	}
	if n := f.count("POST /_aliases"); n != 1 {
		t.Errorf("alias swaps = %d, want 1", n)
	}
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "DELETE ") {
			t.Errorf("a delete was sent although the list failed: %s", r)
		}
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "list old indices failed") {
		t.Errorf("log = %q, want a WARN about the failed list", got)
	}
}

func TestClosedCluster_ReturnsErrorsWithoutPanic(t *testing.T) {
	f := newFakeCluster(t)
	c := newTestClient(t, f)
	f.srv.Close()

	if _, err := c.CreateIndex(context.Background()); err == nil {
		t.Error("CreateIndex() on a closed cluster: error = nil, want error")
	}
	if err := c.Publish(context.Background(), "products-new"); err == nil {
		t.Error("Publish() on a closed cluster: error = nil, want error")
	}
}

func TestNew_SetsProductionClock(t *testing.T) {
	f := newFakeCluster(t)
	c, err := New(context.Background(), f.srv.URL)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if c.now == nil {
		t.Fatal("New() left the clock nil")
	}
	if d := time.Since(c.now()); d < -time.Minute || d > time.Minute {
		t.Errorf("clock is %v away from the wall clock, want under a minute", d)
	}
	name, err := c.CreateIndex(context.Background())
	if err != nil {
		t.Fatalf("CreateIndex() error = %v", err)
	}
	ts, err := time.Parse(indexTimeFormat, strings.TrimPrefix(name, indexPrefix))
	if err != nil {
		t.Fatalf("index name %q has no timestamp: %v", name, err)
	}
	if d := time.Since(ts); d < -time.Minute || d > time.Minute {
		t.Errorf("index timestamp %v is %v from now, want under a minute", ts, d)
	}
}

func TestNew_ClientBuildFailureWarnsWithoutSecrets(t *testing.T) {
	boom := errors.New("boom")
	orig := newAPIClient
	newAPIClient = func(opensearchapi.Config) (*opensearchapi.Client, error) { return nil, boom }
	t.Cleanup(func() { newAPIClient = orig })

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, err := New(context.Background(), "https://example.invalid:9200/s3cr3t-path?token=s3cr3t-query")
	if !errors.Is(err, boom) {
		t.Fatalf("New() error = %v, want it to wrap the client error", err)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "create OpenSearch client failed") {
		t.Errorf("log = %q, want a WARN about the client build", got)
	}
	if !strings.Contains(logs.String(), "address=https://example.invalid:9200") {
		t.Errorf("log %q does not carry the scheme and host", logs.String())
	}
	for _, secret := range []string{"s3cr3t-path", "s3cr3t-query"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(err.Error(), secret) {
			t.Errorf("secret %q leaked: log=%q err=%v", secret, logs.String(), err)
		}
	}
}
