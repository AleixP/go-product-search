package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// routedFakes holds the four fakes behind a router built with New.
type routedFakes struct {
	s  *fakeSearcher
	sg *fakeSuggester
	g  *fakeGetter
	hc *fakeHealth
}

func newRoutes() (http.Handler, routedFakes) {
	f := routedFakes{&fakeSearcher{}, &fakeSuggester{}, &fakeGetter{product: catalog.Product{ID: "123", Name: "Tea"}}, &fakeHealth{}}
	return New(f.s, f.sg, f.g, f.hc, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))), f
}

func (f routedFakes) assertCalls(t *testing.T, search, suggest, get, health int) {
	t.Helper()
	if f.s.calls != search || f.sg.calls != suggest || len(f.g.ids) != get || f.hc.calls != health {
		t.Errorf("calls search/suggest/get/health = %d/%d/%d/%d, want %d/%d/%d/%d",
			f.s.calls, f.sg.calls, len(f.g.ids), f.hc.calls, search, suggest, get, health)
	}
}

func TestRoutes_ProductReachesGetterOnly(t *testing.T) {
	h, f := newRoutes()

	rec := do(h, http.MethodGet, "/products/123")

	assertBody(t, rec, http.StatusOK, `{"id":"123","name":"Tea","brands":[],"categories":[],"nutriscore":""}`+"\n")
	f.assertCalls(t, 0, 0, 1, 0)
	if len(f.g.ids) != 1 || f.g.ids[0] != "123" {
		t.Errorf("getter ids = %q, want [123]", f.g.ids)
	}
}

func TestRoutes_HealthzReachesCheckerOnly(t *testing.T) {
	h, f := newRoutes()

	rec := do(h, http.MethodGet, "/healthz")

	assertBody(t, rec, http.StatusOK, `{"status":"ok"}`+"\n")
	f.assertCalls(t, 0, 0, 0, 1)
}

func TestRoutes_HeadMatchesBothEndpoints(t *testing.T) {
	// A real server is needed: the recorder does not drop HEAD bodies.
	for _, path := range []string{"/products/123", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			h, f := newRoutes()
			srv := httptest.NewServer(h)
			defer srv.Close()
			client := &http.Client{Timeout: 5 * time.Second}

			resp, err := client.Head(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK || len(body) != 0 {
				t.Errorf("HEAD %s = %d %q, want 200 and no body", path, resp.StatusCode, body)
			}
			if path == "/healthz" {
				f.assertCalls(t, 0, 0, 0, 1)
			} else {
				f.assertCalls(t, 0, 0, 1, 0)
			}
		})
	}
}

func TestRoutes_OtherMethodsAre405WithAllow(t *testing.T) {
	for _, path := range []string{"/products/123", "/healthz"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			t.Run(method+" "+path, func(t *testing.T) {
				h, f := newRoutes()

				rec := do(h, method, path)

				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s = %d, want 405", method, path, rec.Code)
				}
				if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
					t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
				}
				if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
					t.Errorf("Content-Type = %q, want text/plain", got)
				}
				if got := rec.Body.String(); got != "Method Not Allowed\n" {
					t.Errorf("body = %q, want the mux's plain text", got)
				}
				f.assertCalls(t, 0, 0, 0, 0)
			})
		}
	}
}

func TestRoutes_LookalikePathsArePlain404(t *testing.T) {
	// "/products/" is pinned to what the mux does: the wildcard {id} does not
	// match an empty segment, so it is a plain 404 and never reaches the getter.
	for _, path := range []string{"/products", "/products/", "/products/1/2", "/healthz/", "/HEALTHZ", "/health", "/Products/1"} {
		t.Run(path, func(t *testing.T) {
			h, f := newRoutes()

			rec := do(h, http.MethodGet, path)

			if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
				t.Errorf("GET %s = %d %q, want the mux's plain 404", path, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", got)
			}
			f.assertCalls(t, 0, 0, 0, 0)
		})
	}
}

func TestRoutes_EscapedIDsReachTheGetterUnescaped(t *testing.T) {
	tests := []struct{ path, want string }{
		{"/products/a%2Fb", "a/b"},
		{"/products/%2E%2E", ".."}, // escaped dots are not separators; the adapter guards ".."
		{"/products/%2E", "."},
		{"/products/a%20b", "a b"},
		{"/products/%C3%A9", "é"},
		{"/products/%2541", "%41"}, // decoded once only: PathValue's result is not unescaped again
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			h, f := newRoutes()

			rec := do(h, http.MethodGet, tt.path)

			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", tt.path, rec.Code)
			}
			if len(f.g.ids) != 1 || f.g.ids[0] != tt.want {
				t.Errorf("getter ids = %q, want [%q]", f.g.ids, tt.want)
			}
			f.assertCalls(t, 0, 0, 1, 0)
		})
	}
}

func TestRoutes_InvalidEscapedIDsAreJSON404WithoutCalls(t *testing.T) {
	for _, path := range []string{"/products/%FF", "/products/%00", "/products/%7F", "/products/%C2%85", "/products/a%0Ab"} {
		t.Run(path, func(t *testing.T) {
			h, f := newRoutes()

			rec := do(h, http.MethodGet, path)

			assertBody(t, rec, http.StatusNotFound, notFoundBody)
			f.assertCalls(t, 0, 0, 0, 0)
		})
	}
}

func TestRoutes_DotDotPathRedirects(t *testing.T) {
	h, f := newRoutes()

	rec := do(h, http.MethodGet, "/products/..")

	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET /products/.. = %d, want 307", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
	f.assertCalls(t, 0, 0, 0, 0)
}

func TestRoutes_SearchAndSuggestStillRouted(t *testing.T) {
	h, f := newRoutes()

	do(h, http.MethodGet, "/search?q=x")
	do(h, http.MethodGet, "/suggest?q=te")

	f.assertCalls(t, 1, 1, 0, 0)
}

func TestRoutes_ProductAndHealthErrorsUseTheEnvelope(t *testing.T) {
	h, f := newRoutes()
	f.g.err = catalog.ErrNotFound
	f.hc.err = http.ErrServerClosed

	assertBody(t, do(h, http.MethodGet, "/products/9"), http.StatusNotFound, notFoundBody)
	assertBody(t, do(h, http.MethodGet, "/healthz"), http.StatusServiceUnavailable, unavailableBody)
	assertBody(t, do(h, http.MethodGet, "/healthz?x=1"), http.StatusBadRequest, noParametersBody)
	assertBody(t, do(h, http.MethodGet, "/products/9?x=1"), http.StatusBadRequest, noParametersBody)
	f.assertCalls(t, 0, 0, 1, 1)
}
