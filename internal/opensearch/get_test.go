package opensearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

func foundDoc(id, source string) string {
	return fmt.Sprintf(`{"_index":"products-x","_id":%q,"_version":1,"found":true,"_source":%s}`, id, source)
}

func TestGet_DecodesFoundDocument(t *testing.T) {
	c, _ := newSearchClient(t, answer(foundDoc("123",
		`{"name":"Tea <b>&","brands":["Acme"],"categories":["drinks"],"nutriscore":"b"}`)))
	got, err := c.Get(context.Background(), "123")
	if err != nil {
		t.Fatalf("Get error = %v", err)
	}
	want := catalog.Product{ID: "123", Name: "Tea <b>&", Brands: []string{"Acme"}, Categories: []string{"drinks"}, Nutriscore: "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get = %#v, want %#v", got, want)
	}
}

func TestGet_MissingListsBecomeEmptySlices(t *testing.T) {
	c, _ := newSearchClient(t, answer(foundDoc("7", `{"name":"X"}`)))
	got, err := c.Get(context.Background(), "7")
	if err != nil {
		t.Fatalf("Get error = %v", err)
	}
	if got.Brands == nil || len(got.Brands) != 0 || got.Categories == nil || len(got.Categories) != 0 {
		t.Errorf("Get = %#v, want non-nil empty Brands and Categories", got)
	}
}

func TestGet_RequestShape(t *testing.T) {
	for id, want := range map[string]string{
		"123": "/products/_doc/123",
		"a/b": "/products/_doc/a%2Fb",
		"a?b": "/products/_doc/a%3Fb",
		"a#b": "/products/_doc/a%23b",
		"é":   "/products/_doc/%C3%A9",
		"a b": "/products/_doc/a%20b",
	} {
		t.Run(id, func(t *testing.T) {
			var mu sync.Mutex
			var method, uri string
			var n int
			c, _ := newSearchClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				method, uri = r.Method, r.RequestURI
				n++
				mu.Unlock()
				writeJSON(w, foundDoc(id, `{"name":"X"}`))
			})
			if _, err := c.Get(context.Background(), id); err != nil {
				t.Fatalf("Get(%q) error = %v", id, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if n != 1 || method != http.MethodGet || uri != want {
				t.Errorf("Get(%q) sent %d x %s %s, want 1 x GET %s", id, n, method, uri, want)
			}
		})
	}
}

func notFoundHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}
}

func TestGet_NotFoundIsBareErrNotFound(t *testing.T) {
	c, _ := newSearchClient(t, notFoundHandler(`{"_index":"products-x","_id":"s3cr3t-id","found":false}`))
	_, err := c.Get(context.Background(), "s3cr3t-id")
	if !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-id") {
		t.Errorf("error echoes the id: %v", err)
	}
}

func TestGet_MissingAliasIsNotNotFound(t *testing.T) {
	c, _ := newSearchClient(t, notFoundHandler(
		`{"error":{"root_cause":[],"type":"index_not_found_exception","reason":"stub-secret-text"},"status":404}`))
	_, err := c.Get(context.Background(), "1")
	if err == nil || errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Get error = %v, want a non-ErrNotFound error", err)
	}
	if want := "get product: status 404, type index_not_found_exception"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestGet_NotFoundWithUndecodableBody(t *testing.T) {
	c, _ := newSearchClient(t, notFoundHandler(`<html>nope</html>`))
	_, err := c.Get(context.Background(), "1")
	if err == nil || errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Get error = %v, want a non-ErrNotFound error", err)
	}
	if want := "get product: undecodable response (status 404)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestGet_FoundFalseOn200IsErrNotFound(t *testing.T) {
	c, _ := newSearchClient(t, answer(`{"_index":"products-x","_id":"1","found":false}`))
	if _, err := c.Get(context.Background(), "1"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestGet_SourceNotAnObjectDoesNotLeak(t *testing.T) {
	c, _ := newSearchClient(t, answer(foundDoc("1", `"leaky-source-text"`)))
	_, err := c.Get(context.Background(), "1")
	if err == nil {
		t.Fatal("Get error = nil, want an error")
	}
	if want := "get product: decode source"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestGet_ServerErrorKeepsStatusAndTypeOnly(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"boom_exception","reason":"stub-secret-text"},"status":500}`))
	})
	_, err := c.Get(context.Background(), "1")
	if err == nil {
		t.Fatal("Get error = nil, want an error")
	}
	if want := "get product: status 500, type boom_exception"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestGet_ConnectionErrorKeepsCauseDropsAddressPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c, err := New(context.Background(), srv.URL+"/tok3n")
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	srv.Close()
	_, err = c.Get(context.Background(), "1")
	if err == nil {
		t.Fatal("Get error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want the cause kept", err)
	}
	if strings.Contains(err.Error(), "tok3n") {
		t.Errorf("error echoes the address path: %v", err)
	}
}

func TestGet_DeadlineAgainstSilentServer(t *testing.T) {
	release := make(chan struct{})
	c, _ := newSearchClient(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Get(ctx, "1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get error = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Get took %v, want under 1s", d)
	}
}

func TestGet_UnaddressableIDsMakeNoRequest(t *testing.T) {
	var mu sync.Mutex
	var n int
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		writeJSON(w, foundDoc("x", `{"name":"X"}`))
	})
	for _, id := range []string{"", ".", ".."} {
		if _, err := c.Get(context.Background(), id); !errors.Is(err, catalog.ErrNotFound) {
			t.Errorf("Get(%q) error = %v, want ErrNotFound", id, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 0 {
		t.Errorf("fake saw %d requests, want 0", n)
	}
}

func TestGet_LargeSourceDecodesExactly(t *testing.T) {
	ctrl := strings.Repeat(`\u0001`, 16<<10)
	name := strings.Repeat(`\u0001`, 1<<10)
	c, _ := newSearchClient(t, answer(foundDoc("1",
		fmt.Sprintf(`{"name":"%s","brands":["%s"],"categories":["%s"],"nutriscore":""}`, name, ctrl, ctrl))))
	got, err := c.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get error = %v", err)
	}
	if len(got.Name) != 1<<10 || len(got.Brands[0]) != 16<<10 || len(got.Categories[0]) != 16<<10 {
		t.Errorf("lengths = %d, %d, %d, want %d, %d, %d",
			len(got.Name), len(got.Brands[0]), len(got.Categories[0]), 1<<10, 16<<10, 16<<10)
	}
}

func TestGet_NotFoundJSONWithoutFoundIsNotNotFound(t *testing.T) {
	c, _ := newSearchClient(t, notFoundHandler(`{"_index":"products-x","_id":"1"}`))
	_, err := c.Get(context.Background(), "1")
	if err == nil || errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Get error = %v, want non-ErrNotFound", err)
	}
	if want := "get product: undecodable response (status 404)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestGet_NotFoundWithFoundTrueIsNotNotFound(t *testing.T) {
	c, _ := newSearchClient(t, notFoundHandler(`{"_index":"products-x","_id":"1","found":true}`))
	_, err := c.Get(context.Background(), "1")
	if err == nil || errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("Get error = %v, want non-ErrNotFound", err)
	}
}

func TestGet_ServerErrorNonJSONKeepsStatusOnly(t *testing.T) {
	c, _ := newSearchClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<html>stub-secret-text</html>`))
	})
	_, err := c.Get(context.Background(), "1")
	if err == nil {
		t.Fatal("Get error = nil, want an error")
	}
	if want := "get product: undecodable response (status 500)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestGet_ConnectionErrorKeepsOperationPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c, err := New(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	srv.Close()
	_, err = c.Get(context.Background(), "1")
	if err == nil || !strings.HasPrefix(err.Error(), "get product: ") {
		t.Errorf("error = %v, want prefix %q", err, "get product: ")
	}
}

// An empty or null 404 body carries no found:false, so it is an undecodable
// response and not a missing product.
func TestGet_NotFoundEmptyOrNullBodyIsNotNotFound(t *testing.T) {
	for name, body := range map[string]string{"empty": ``, "null": `null`} {
		t.Run(name, func(t *testing.T) {
			c, _ := newSearchClient(t, notFoundHandler(body))
			_, err := c.Get(context.Background(), "1")
			if err == nil || errors.Is(err, catalog.ErrNotFound) {
				t.Fatalf("Get error = %v, want non-ErrNotFound", err)
			}
			if want := "get product: undecodable response (status 404)"; err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
		})
	}
}
