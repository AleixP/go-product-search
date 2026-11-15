package opensearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

var okSuggest = catalog.SuggestQuery{Text: "choc", Size: 10}

// suggestHit is one search hit with the given raw JSON as its name value.
func suggestHit(i int, rawName string) string {
	return fmt.Sprintf(`{"_id":"%d","_score":1.0,"_source":{"name":%s},"fields":{"name.norm":["x%d"]}}`, i, rawName, i)
}

func suggestResponse(hits ...string) string {
	return searchBodyWith(len(hits), 0, false, strings.Join(hits, ","), "")
}

func TestSuggest_SendsBodyToAliasSearchPath(t *testing.T) {
	var method, path string
	var got []byte
	c, _ := newSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		got, _ = io.ReadAll(r.Body)
		writeJSON(w, suggestResponse())
	})
	q := catalog.SuggestQuery{Text: `ch"oc <b>&`, Size: 7}
	if _, err := c.Suggest(context.Background(), q); err != nil {
		t.Fatalf("Suggest error = %v", err)
	}
	want, err := search.SuggestBody(q)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/products/_search" {
		t.Errorf("request = %s %s, want POST /products/_search", method, path)
	}
	if string(got) != string(want) {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestSuggest_DecodesHitsInOrder(t *testing.T) {
	c, _ := newSearchClient(t, answer(suggestResponse(
		suggestHit(1, `"Zeta"`), suggestHit(2, `"Alpha"`), suggestHit(3, `"Mid"`))))
	got, err := c.Suggest(context.Background(), okSuggest)
	if err != nil {
		t.Fatalf("Suggest error = %v", err)
	}
	want := []catalog.Suggestion{{Text: "Zeta"}, {Text: "Alpha"}, {Text: "Mid"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Suggest = %v, want %v", got, want)
	}
}

func TestSuggest_NoHitsGivesNonNilEmpty(t *testing.T) {
	c, _ := newSearchClient(t, answer(suggestResponse()))
	got, err := c.Suggest(context.Background(), okSuggest)
	if err != nil {
		t.Fatalf("Suggest error = %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("Suggest = %#v, want a non-nil empty slice", got)
	}
}

func TestSuggest_SizeCapDropsExtraHits(t *testing.T) {
	for _, tc := range []struct{ hits, size, want int }{
		{12, 10, 10}, {11, 10, 10}, {10, 10, 10}, {9, 10, 9}, {2, 1, 1},
	} {
		hits := make([]string, tc.hits)
		for i := range hits {
			hits[i] = suggestHit(i, fmt.Sprintf(`"n%d"`, i))
		}
		c, _ := newSearchClient(t, answer(suggestResponse(hits...)))
		got, err := c.Suggest(context.Background(), catalog.SuggestQuery{Text: "ab", Size: tc.size})
		if err != nil {
			t.Fatalf("hits %d size %d: error = %v", tc.hits, tc.size, err)
		}
		if len(got) != tc.want {
			t.Errorf("hits %d size %d: got %d suggestions, want %d", tc.hits, tc.size, len(got), tc.want)
		}
		for i, s := range got {
			if want := fmt.Sprintf("n%d", i); s.Text != want {
				t.Errorf("hits %d size %d: [%d] = %q, want %q", tc.hits, tc.size, i, s.Text, want)
			}
		}
	}
}

func TestSuggest_DropsExtraHitsBeforeDecodingThem(t *testing.T) {
	c, _ := newSearchClient(t, answer(suggestResponse(suggestHit(0, `"ok"`), suggestHit(1, `5`))))
	got, err := c.Suggest(context.Background(), catalog.SuggestQuery{Text: "ab", Size: 1})
	if err != nil || len(got) != 1 || got[0].Text != "ok" {
		t.Errorf("Suggest = %v, %v; want [ok], nil", got, err)
	}
}

func TestSuggest_NamesAreKeptAsIs(t *testing.T) {
	c, _ := newSearchClient(t, answer(suggestResponse(
		suggestHit(1, `"  x  "`), suggestHit(2, `"Crème Brûlée 日本 😀"`), suggestHit(3, `"<b>&\"q\""`))))
	got, err := c.Suggest(context.Background(), okSuggest)
	if err != nil {
		t.Fatalf("Suggest error = %v", err)
	}
	want := []catalog.Suggestion{{Text: "  x  "}, {Text: "Crème Brûlée 日本 😀"}, {Text: `<b>&"q"`}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Suggest = %q, want %q", got, want)
	}
}

func TestSuggest_IgnoresCollapseFieldsAndTotal(t *testing.T) {
	body := searchBodyWith(9999, 0, false, `{"_id":"1","_source":{"name":"A","other":1},"fields":{"name.norm":["a"]}}`, "")
	c, _ := newSearchClient(t, answer(body))
	got, err := c.Suggest(context.Background(), okSuggest)
	if err != nil || !reflect.DeepEqual(got, []catalog.Suggestion{{Text: "A"}}) {
		t.Errorf("Suggest = %v, %v; want [A], nil", got, err)
	}
}

func TestSuggest_BadNameFailsWithoutSourceText(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"missing", `{"note":"secret-body"}`},
		{"number", `{"name":5,"note":"secret-body"}`},
		{"empty", `{"name":"","note":"secret-body"}`},
		{"null", `{"name":null,"note":"secret-body"}`},
		{"object", `{"name":{"a":"secret-body"}}`},
		{"no source", ``},
		{"duplicate key", `{"name":"Alpha","name":5,"note":"secret-body"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := `{"_id":"2"}`
			if tc.source != "" {
				hit = `{"_id":"2","_source":` + tc.source + `}`
			}
			c, _ := newSearchClient(t, answer(suggestResponse(suggestHit(1, `"ok"`), hit)))
			_, err := c.Suggest(context.Background(), okSuggest)
			if want := "search products: decode hit 1"; err == nil || err.Error() != want {
				t.Errorf("Suggest error = %v, want %q", err, want)
			}
		})
	}
}

func TestSuggest_FirstBadHitIsHitZero(t *testing.T) {
	c, _ := newSearchClient(t, answer(suggestResponse(suggestHit(1, `5`), suggestHit(2, `"ok"`))))
	_, err := c.Suggest(context.Background(), okSuggest)
	if want := "search products: decode hit 0"; err == nil || err.Error() != want {
		t.Errorf("Suggest error = %v, want %q", err, want)
	}
}

func TestSuggest_IndexNotFoundNamesTheTypeNotTheReason(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"root_cause":[],"type":"index_not_found_exception","reason":"secret-reason"},"status":404}`)
	})
	_, err := c.Suggest(context.Background(), okSuggest)
	if err == nil || !strings.Contains(err.Error(), "index_not_found_exception") || strings.Contains(err.Error(), "secret-reason") {
		t.Errorf("Suggest error = %v, want the type and not the reason", err)
	}
}

func TestSuggest_UndecodableErrorBodyIsNotLeaked(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "<html>secret-body</html>")
	})
	_, err := c.Suggest(context.Background(), okSuggest)
	if want := "search products: undecodable response (status 500)"; err == nil || err.Error() != want {
		t.Errorf("Suggest error = %v, want %q", err, want)
	}
}

func TestSuggest_MalformedSuccessBodyIsNotLeaked(t *testing.T) {
	c, _ := newSearchClient(t, answer(`{"hits": secret-body`))
	_, err := c.Suggest(context.Background(), okSuggest)
	if want := "search products: undecodable response"; err == nil || err.Error() != want {
		t.Errorf("Suggest error = %v, want %q", err, want)
	}
}

func TestSuggest_FailedShardsIsAnError(t *testing.T) {
	body := `{"took":1,"timed_out":false,"_shards":{"total":3,"successful":2,"failed":1},"hits":{"hits":[` + suggestHit(1, `"ok"`) + `]}}`
	c, _ := newSearchClient(t, answer(body))
	_, err := c.Suggest(context.Background(), okSuggest)
	if want := "search products: 1 of 3 shards failed"; err == nil || err.Error() != want {
		t.Errorf("Suggest error = %v, want %q", err, want)
	}
}

func TestSuggest_TimedOutIsAnError(t *testing.T) {
	c, _ := newSearchClient(t, answer(searchBodyWith(1, 0, true, suggestHit(1, `"ok"`), "")))
	_, err := c.Suggest(context.Background(), okSuggest)
	if want := "search products: timed out"; err == nil || err.Error() != want {
		t.Errorf("Suggest error = %v, want %q", err, want)
	}
}

func TestSuggest_ShardFailureWinsOverTimeoutAndBadHits(t *testing.T) {
	body := `{"took":1,"timed_out":true,"_shards":{"total":2,"successful":1,"failed":1},"hits":{"hits":[` + suggestHit(1, `5`) + `]}}`
	c, _ := newSearchClient(t, answer(body))
	_, err := c.Suggest(context.Background(), okSuggest)
	if want := "search products: 1 of 2 shards failed"; err == nil || err.Error() != want {
		t.Errorf("Suggest error = %v, want %q", err, want)
	}
}

func TestSuggest_TimeoutWinsOverBadHits(t *testing.T) {
	c, _ := newSearchClient(t, answer(searchBodyWith(1, 0, true, suggestHit(1, `5`), "")))
	_, err := c.Suggest(context.Background(), okSuggest)
	if want := "search products: timed out"; err == nil || err.Error() != want {
		t.Errorf("Suggest error = %v, want %q", err, want)
	}
}

func TestSuggest_MakesOnlyTheSearchCall(t *testing.T) {
	var searches, others atomic.Int32
	c, _ := newSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/products/_search" {
			searches.Add(1)
		} else {
			others.Add(1)
		}
		writeJSON(w, suggestResponse())
	})
	if _, err := c.Suggest(context.Background(), okSuggest); err != nil {
		t.Fatal(err)
	}
	if searches.Load() != 1 || others.Load() != 0 {
		t.Errorf("searches = %d, other calls = %d, want 1 and 0 (no _analyze)", searches.Load(), others.Load())
	}
}

func TestSuggest_ContextDeadlineReturnsPromptly(t *testing.T) {
	release := make(chan struct{})
	c, _ := newSearchClient(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	// Registered after the server's own cleanup, so it runs first and frees the handler.
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.Suggest(ctx, okSuggest)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Suggest did not return within 5s of a 50ms context deadline")
	}
	if err == nil {
		t.Fatal("Suggest error = nil, want error")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Suggest took %v, want < 1s", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, DeadlineExceeded) = false for %v", err)
	}
}

// The adapter has no name length limit of its own: ingest caps names at 1 KiB
// upstream, so these cases pin that nothing here truncates or rejects, at the
// cap, one byte past it (including mid-rune) and far beyond it.
func TestSuggest_LongNamesAreNotTruncated(t *testing.T) {
	tests := []struct{ name, text string }{
		{"1024 ascii bytes", strings.Repeat("a", 1024)},
		{"512 two-byte runes", strings.Repeat("é", 512)},
		{"1025 ascii bytes", strings.Repeat("a", 1025)},
		{"512 two-byte runes plus one byte", strings.Repeat("é", 512) + "a"},
		{"64 KiB ascii", strings.Repeat("a", 64<<10)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newSearchClient(t, answer(suggestResponse(suggestHit(1, `"`+tt.text+`"`))))
			got, err := c.Suggest(context.Background(), catalog.SuggestQuery{Text: "ab", Size: 10})
			if err != nil {
				t.Fatalf("Suggest error = %v", err)
			}
			want := []catalog.Suggestion{{Text: tt.text}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Suggest = %d suggestions, want 1 with the %d-byte name unchanged", len(got), len(tt.text))
			}
		})
	}
}

func TestSuggest_NonPositiveSizeGivesEmptyNonNilList(t *testing.T) {
	for _, size := range []int{0, -1} {
		c, _ := newSearchClient(t, answer(suggestResponse(suggestHit(1, `"a"`), suggestHit(2, `"b"`))))
		got, err := c.Suggest(context.Background(), catalog.SuggestQuery{Text: "ab", Size: size})
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("size %d: Suggest = %#v, %v; want empty non-nil, nil", size, got, err)
		}
	}
}
