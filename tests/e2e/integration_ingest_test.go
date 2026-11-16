package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ingestBound is the longest the real ingest may run (as in runIngest). It is
// a var so a test can shorten it, like stopGrace.
var ingestBound = time.Minute

// runIngestGuarded is runIngest through guardedScript: the child also ends,
// with SIGINT then SIGKILL, when the test process dies, or when ctx is
// cancelled, so a -test.timeout panic or a SIGKILL cannot leave it running.
// It returns the exit code and the collected stdout and stderr.
func runIngestGuarded(t *testing.T, ctx context.Context, bin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	grace := time.Duration(stopGrace) * time.Second
	ctx, cancel := context.WithTimeout(ctx, ingestBound)
	defer cancel()
	argv := append([]string{"-c", guardedScript, "sh", strconv.Itoa(stopGrace), bin}, args...)
	cmd := exec.CommandContext(ctx, "sh", argv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Cancel = func() error { return stdin.Close() } // EOF asks the wrapper for the graceful stop
	cmd.WaitDelay = grace + 5*time.Second
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		t.Fatalf("%s did not finish within %v (or was interrupted): %v\nstderr:\n%s", bin, ingestBound, ctx.Err(), errb.String())
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v", bin, err)
	}
	return code, out.String(), errb.String()
}

// ingestIntegration runs the real ingest binary on the fixture against addr and
// checks its report against the literals. It reports false when the ingest
// failed, so the caller does not go on to query an empty index.
func ingestIntegration(t *testing.T, ctx context.Context, bin, addr string) bool {
	t.Helper()
	return t.Run("ingest publishes the fixture", func(t *testing.T) {
		fixture, err := filepath.Abs(integrationFixture)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		code, stdout, stderr := runIngestGuarded(t, ctx, bin,
			"--input", fixture, "--opensearch", addr, "--sample-every", "1", "--limit", "0")
		t.Logf("ingest took %v", time.Since(start).Round(time.Millisecond))
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		rep := parseReport(t, stdout)
		checkInvariants(t, rep)
		if rep["published"] != "true" || rep["dry_run"] != "false" {
			t.Errorf("published=%q dry_run=%q, want true false", rep["published"], rep["dry_run"])
		}
		for _, w := range []struct {
			key  string
			want int
		}{
			{"rows_read", wantRowsRead}, {"passed", wantPassed}, {"indexed", wantIndexed},
			{"failed", 0}, {"doc_count", wantDocCount}, {"country", wantCountry},
			{"brand", wantBrand}, {"category", wantCategory}, {"nutriscore", wantNutriscore},
		} {
			if got := num(t, rep, w.key); got != w.want {
				t.Errorf("report %s = %d, want %d", w.key, got, w.want)
			}
		}
		if !strings.HasPrefix(rep["index"], alias+"-") {
			t.Errorf("report index = %q, want a %s-* name", rep["index"], alias)
		}
		if t.Failed() {
			t.Fatalf("the ingest report is not the expected one:\n%s", stdout)
		}
	})
}

// searchItem and searchReply decode the JSON of GET /search.
type searchItem struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Brands     []string `json:"brands"`
	Categories []string `json:"categories"`
	Nutriscore string   `json:"nutriscore"`
}

type searchReply struct {
	Total  int                        `json:"total"`
	Page   int                        `json:"page"`
	Size   int                        `json:"size"`
	Items  []searchItem               `json:"items"`
	Facets map[string]json.RawMessage `json:"facets"`
	body   string
}

// getSearch sends GET /search?<query> and decodes the 200 reply.
func getSearch(t *testing.T, client *http.Client, base, query string) searchReply {
	t.Helper()
	got := do(t, client, http.MethodGet, base+"/search?"+query)
	if got.status != http.StatusOK {
		t.Fatalf("GET /search?%s status = %d, want 200\nbody: %s", query, got.status, got.body)
	}
	var r searchReply
	if err := json.Unmarshal([]byte(got.body), &r); err != nil {
		t.Fatalf("GET /search?%s: bad JSON: %v\nbody: %s", query, err, got.body)
	}
	r.body = got.body
	return r
}

func itemIDs(items []searchItem) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func integrationIDs() []string {
	ids := make([]string, len(integrationDocs))
	for i, d := range integrationDocs {
		ids[i] = d.id
	}
	return ids
}

// integrationDataSubtests checks what the ingest put in the index: the set of
// documents, their fields, the dropped rows and pagination. Order is never
// asserted: the facet order and the hit order belong to other subtests.
func integrationDataSubtests(t *testing.T, client *http.Client, base string) {
	want := integrationIDs()
	slices.Sort(want)

	t.Run("search lists exactly the indexed documents", func(t *testing.T) {
		r := getSearch(t, client, base, "size=100")
		if r.Total != len(integrationDocs) {
			t.Errorf("total = %d, want %d", r.Total, len(integrationDocs))
		}
		got := itemIDs(r.Items)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
	})

	t.Run("every document has the expected fields", func(t *testing.T) {
		r := getSearch(t, client, base, "size=100")
		byID := make(map[string]searchItem, len(r.Items))
		for _, it := range r.Items {
			byID[it.ID] = it
		}
		for _, d := range integrationDocs {
			it, ok := byID[d.id]
			if !ok {
				t.Errorf("document %s is missing", d.id)
				continue
			}
			if it.Name != d.name || it.Nutriscore != d.grade ||
				!slices.Equal(it.Brands, d.brands) || !slices.Equal(it.Categories, d.categories) {
				t.Errorf("document %s = %+v, want name %q brands %q categories %q grade %q",
					d.id, it, d.name, d.brands, d.categories, d.grade)
			}
		}
	})

	t.Run("rows dropped by the ingest filters are absent", func(t *testing.T) {
		r := getSearch(t, client, base, "size=100")
		for _, id := range itemIDs(r.Items) {
			if slices.Contains(integrationDropped, id) {
				t.Errorf("dropped row %s was indexed", id)
			}
		}
	})

	t.Run("pagination walks every document once", func(t *testing.T) {
		const size = 7
		pages := (len(integrationDocs) + size - 1) / size
		seen := map[string]int{}
		for p := 1; p <= pages; p++ {
			r := getSearch(t, client, base, "size="+strconv.Itoa(size)+"&page="+strconv.Itoa(p))
			if r.Total != len(integrationDocs) {
				t.Errorf("page %d total = %d, want %d", p, r.Total, len(integrationDocs))
			}
			wantLen := min(size, len(integrationDocs)-(p-1)*size)
			if len(r.Items) != wantLen {
				t.Errorf("page %d has %d items, want %d", p, len(r.Items), wantLen)
			}
			for _, id := range itemIDs(r.Items) {
				seen[id]++
			}
		}
		got := make([]string, 0, len(seen))
		for id, n := range seen {
			if n != 1 {
				t.Errorf("document %s came back on %d pages, want 1", id, n)
			}
			got = append(got, id)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("union of the pages = %v, want %v", got, want)
		}

		past := getSearch(t, client, base, "size="+strconv.Itoa(size)+"&page="+strconv.Itoa(pages+1))
		var raw struct {
			Items *[]json.RawMessage `json:"items"` // nil pointer: absent or null
		}
		if err := json.Unmarshal([]byte(past.body), &raw); err != nil {
			t.Fatalf("page %d: bad JSON: %v", pages+1, err)
		}
		if past.Total != len(integrationDocs) || raw.Items == nil || len(*raw.Items) != 0 {
			t.Errorf("page %d: total = %d, body = %s; want the same total and items []", pages+1, past.Total, past.body)
		}
		for _, k := range []string{"brands", "categories", "nutriscore"} {
			if _, ok := past.Facets[k]; !ok {
				t.Errorf("page %d has no facets.%s", pages+1, k)
			}
		}
	})
}
