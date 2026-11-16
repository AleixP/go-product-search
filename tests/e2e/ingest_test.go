// Package e2e holds the cross-package end-to-end tests. Most run the real
// binaries against a stub OpenSearch served by httptest: no network and no real
// cluster. The package also holds the Docker-backed integration test
// (integration_test.go), which starts its own OpenSearch container.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ingestPkg = "github.com/AleixP/go-product-search/cmd/ingest"
	oldIndex  = "products-20200101t000000z"
	alias     = "products"
)

// stubCluster is a stateful stand-in for the OpenSearch REST calls the ingest
// command makes. Any other request fails the test.
type stubCluster struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	reqs    []string                   // "METHOD /path"
	indices map[string]map[string]bool // index -> stored document ids
	aliased []string                   // indices the products alias points at
	// failItem decides, per position inside one bulk request, whether the
	// item is answered with an error.
	failItem func(pos int) bool
}

// newStubCluster starts a cluster that already serves oldIndex behind the alias.
func newStubCluster(t *testing.T, failItem func(pos int) bool) *stubCluster {
	t.Helper()
	s := &stubCluster{
		t:        t,
		indices:  map[string]map[string]bool{oldIndex: {"old-doc": true}},
		aliased:  []string{oldIndex},
		failItem: failItem,
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *stubCluster) notFound(w http.ResponseWriter, name string) {
	writeJSON(w, http.StatusNotFound, `{"error":{"root_cause":[{"type":"index_not_found_exception","reason":"no such index [`+name+`]"}],"type":"index_not_found_exception","reason":"no such index [`+name+`]"},"status":404}`)
}

// indexGet answers GET /<names> with the given concrete indices, or 404.
func (s *stubCluster) indexGet(w http.ResponseWriter, names []string, missing string) {
	if len(names) == 0 {
		s.notFound(w, missing)
		return
	}
	m := map[string]any{}
	for _, n := range names {
		m[n] = map[string]any{"aliases": map[string]any{}, "mappings": map[string]any{}, "settings": map[string]any{}}
	}
	b, _ := json.Marshal(m)
	writeJSON(w, http.StatusOK, string(b))
}

func (s *stubCluster) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, key)

	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	switch {
	case key == "HEAD /":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && path == alias:
		s.indexGet(w, s.aliased, alias) // an alias resolves to its concrete indices
	case r.Method == http.MethodGet && path == "products-*":
		s.indexGet(w, s.sortedIndices(), path)
	case r.Method == http.MethodGet && path == "_alias/"+alias:
		if len(s.aliased) == 0 {
			s.notFound(w, alias)
			return
		}
		m := map[string]any{}
		for _, n := range s.aliased {
			m[n] = map[string]any{"aliases": map[string]any{alias: map[string]any{}}}
		}
		b, _ := json.Marshal(m)
		writeJSON(w, http.StatusOK, string(b))
	case key == "POST /_aliases":
		s.applyAliases(w, body)
	case r.Method == http.MethodPut && len(parts) == 1:
		s.indices[path] = map[string]bool{}
		writeJSON(w, http.StatusOK, `{"acknowledged":true,"shards_acknowledged":true,"index":"`+path+`"}`)
	case r.Method == http.MethodPut && len(parts) == 2 && parts[1] == "_settings":
		writeJSON(w, http.StatusOK, `{"acknowledged":true}`)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "_bulk":
		s.bulk(w, parts[0], body)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "_refresh":
		writeJSON(w, http.StatusOK, `{"_shards":{"total":1,"successful":1,"failed":0}}`)
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && len(parts) == 2 && parts[1] == "_count":
		docs, ok := s.indices[parts[0]]
		if !ok {
			s.notFound(w, parts[0])
			return
		}
		writeJSON(w, http.StatusOK, `{"count":`+strconv.Itoa(len(docs))+`,"_shards":{"total":1,"successful":1,"skipped":0,"failed":0}}`)
	case r.Method == http.MethodDelete && len(parts) == 1:
		if _, ok := s.indices[path]; !ok {
			s.notFound(w, path)
			return
		}
		delete(s.indices, path)
		writeJSON(w, http.StatusOK, `{"acknowledged":true}`)
	default:
		s.t.Errorf("unexpected request %s", key)
		w.WriteHeader(http.StatusTeapot)
	}
}

// applyAliases applies the remove and add actions of POST /_aliases.
func (s *stubCluster) applyAliases(w http.ResponseWriter, body []byte) {
	var req struct {
		Actions []map[string]struct {
			Index string `json:"index"`
			Alias string `json:"alias"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, `{"error":"bad body","status":400}`)
		return
	}
	for _, act := range req.Actions {
		if a, ok := act["remove"]; ok && a.Alias == alias {
			s.aliased = slices.DeleteFunc(s.aliased, func(n string) bool { return n == a.Index })
		}
		if a, ok := act["add"]; ok && a.Alias == alias {
			s.aliased = append(s.aliased, a.Index)
		}
	}
	writeJSON(w, http.StatusOK, `{"acknowledged":true}`)
}

// bulk stores the documents of an NDJSON bulk body and answers one item per
// document; items chosen by failItem are rejected and not stored.
func (s *stubCluster) bulk(w http.ResponseWriter, index string, body []byte) {
	docs, ok := s.indices[index]
	if !ok {
		s.notFound(w, index)
		return
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	items := make([]map[string]any, 0, len(lines)/2)
	hasErrors := false
	for i := 0; i+1 < len(lines); i += 2 {
		var act struct {
			Index struct {
				ID string `json:"_id"`
			} `json:"index"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &act); err != nil {
			s.t.Errorf("bulk action line %q: %v", lines[i], err)
		}
		item := map[string]any{"_index": index, "_id": act.Index.ID, "status": 201}
		if s.failItem != nil && s.failItem(i/2) {
			item["status"] = 400
			item["error"] = map[string]any{"type": "mapper_parsing_exception", "reason": "rejected by the stub"}
			hasErrors = true
		} else {
			docs[act.Index.ID] = true
		}
		items = append(items, map[string]any{"index": item})
	}
	out, _ := json.Marshal(map[string]any{"took": 1, "errors": hasErrors, "items": items})
	writeJSON(w, http.StatusOK, string(out))
}

func (s *stubCluster) sortedIndices() []string {
	names := make([]string, 0, len(s.indices))
	for n := range s.indices {
		if strings.HasPrefix(n, "products-") {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// state returns a snapshot of the indices and the alias targets.
func (s *stubCluster) state() (indices, aliased []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedIndices(), slices.Clone(s.aliased)
}

func (s *stubCluster) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// buildIngest compiles the real command into dir and returns the binary path.
func buildIngest(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "ingest")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ingestPkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", ingestPkg, err, out)
	}
	return bin
}

// runIngest executes the binary and returns its exit code, stdout and stderr.
func runIngest(t *testing.T, bin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v", bin, err)
	}
	return code, out.String(), errb.String()
}

// parseReport turns the "key=value" lines of the report into a map.
func parseReport(t *testing.T, stdout string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("report line %q is not key=value\n%s", line, stdout)
		}
		m[k] = v
	}
	return m
}

// num reads an integer report value, failing the test when it is missing.
func num(t *testing.T, rep map[string]string, key string) int {
	t.Helper()
	v, ok := rep[key]
	if !ok {
		t.Fatalf("report has no %q line: %v", key, rep)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("report %s=%q is not a number", key, v)
	}
	return n
}

// checkInvariants asserts the report arithmetic: every row lands in exactly one
// bucket, and the valid rows are the passed ones plus the filter buckets.
func checkInvariants(t *testing.T, rep map[string]string) {
	t.Helper()
	skips := []string{"too_long", "malformed", "invalid_code", "oversized", "no_name", "not_sampled"}
	filters := []string{"country", "brand", "category", "nutriscore"}
	passed := num(t, rep, "passed")
	sum, filtered := passed, passed
	for _, k := range skips {
		sum += num(t, rep, k)
	}
	for _, k := range filters {
		n := num(t, rep, k)
		sum += n
		filtered += n
	}
	if rows := num(t, rep, "rows_read"); rows != sum {
		t.Errorf("rows_read = %d, want skip buckets + passed = %d", rows, sum)
	}
	if valid := num(t, rep, "valid"); valid != filtered {
		t.Errorf("valid = %d, want passed + filter buckets = %d", valid, filtered)
	}
	if got := num(t, rep, "indexed") + num(t, rep, "failed"); got != passed {
		t.Errorf("indexed + failed = %d, want passed = %d", got, passed)
	}
}

func TestIngest_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ingest binary; skipped under -short")
	}
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "products.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	bin := buildIngest(t, t.TempDir())
	args := func(s *stubCluster, extra ...string) []string {
		return append([]string{"--input", fixture, "--opensearch", s.srv.URL, "--sample-every", "1", "--limit", "0"}, extra...)
	}

	t.Run("publishes the new index", func(t *testing.T) {
		s := newStubCluster(t, nil)
		code, stdout, stderr := runIngest(t, bin, args(s)...)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		rep := parseReport(t, stdout)
		checkInvariants(t, rep)
		if rep["published"] != "true" || rep["dry_run"] != "false" {
			t.Errorf("published=%q dry_run=%q, want true false", rep["published"], rep["dry_run"])
		}
		if rows, passed := num(t, rep, "rows_read"), num(t, rep, "passed"); rows != 15 || passed != 7 {
			t.Errorf("rows_read=%d passed=%d, want 15 7 (fixture)", rows, passed)
		}
		if num(t, rep, "failed") != 0 || num(t, rep, "indexed") != num(t, rep, "passed") {
			t.Errorf("indexed=%s failed=%s, want all passed documents indexed", rep["indexed"], rep["failed"])
		}
		if got, want := num(t, rep, "doc_count"), num(t, rep, "passed"); got != want {
			t.Errorf("doc_count = %d, want passed = %d", got, want)
		}
		indices, aliased := s.state()
		index := rep["index"]
		if !strings.HasPrefix(index, "products-") {
			t.Fatalf("report index = %q, want a products-* name", index)
		}
		if !slices.Equal(aliased, []string{index}) {
			t.Errorf("alias points at %v, want only %s", aliased, index)
		}
		if !slices.Equal(indices, []string{index}) {
			t.Errorf("indices = %v, want only %s (the old one deleted)", indices, index)
		}
	})

	t.Run("failed documents above 1 percent do not publish", func(t *testing.T) {
		s := newStubCluster(t, func(pos int) bool { return pos%2 == 0 }) // 4 of 7 fail
		code, stdout, stderr := runIngest(t, bin, args(s)...)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "not publishing") {
			t.Errorf("stderr = %q, want it to say the index is not published", stderr)
		}
		rep := parseReport(t, stdout)
		checkInvariants(t, rep)
		if rep["published"] != "false" {
			t.Errorf("published = %q, want false", rep["published"])
		}
		if num(t, rep, "failed") == 0 {
			t.Errorf("failed = 0, want the stub rejections counted")
		}
		indices, aliased := s.state()
		if !slices.Equal(aliased, []string{oldIndex}) {
			t.Errorf("alias points at %v, want it unchanged at %s", aliased, oldIndex)
		}
		if !slices.Equal(indices, []string{oldIndex}) {
			t.Errorf("indices = %v, want only %s (the new one deleted)", indices, oldIndex)
		}
	})

	t.Run("usage error exits 2 without touching the cluster", func(t *testing.T) {
		s := newStubCluster(t, nil)
		code, stdout, stderr := runIngest(t, bin, "--opensearch", s.srv.URL, "--sample-every", "0")
		if code != 2 {
			t.Fatalf("exit code = %d, want 2\nstderr:\n%s", code, stderr)
		}
		if !strings.Contains(stderr, "--sample-every must be >= 1") {
			t.Errorf("stderr = %q, want the usage error", stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if n := s.requestCount(); n != 0 {
			t.Errorf("stub received %d requests, want 0", n)
		}
	})

	t.Run("binary is built outside the repository", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join("..", "..", "ingest")); err == nil {
			t.Error("a binary named ingest exists in the repo root")
		}
	})
}
