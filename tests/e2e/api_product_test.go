package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// This file holds the T-9 subtests of TestAPI_EndToEnd (/products/{id} and
// /healthz) and the stub's answers for them.

const (
	// wantProductJSON is the stub's document 9990000000002 as the API renders it
	// (brands absent in the engine, so []). It is the second item of wantSearchJSON.
	wantProductJSON = `{"id":"9990000000002","name":"Green Tea <b>&","brands":[],"categories":["en:beverages","en:teas"],"nutriscore":""}` + "\n"
	wantNotFound    = `{"error":{"code":"not_found","message":"product not found"}}` + "\n"
	wantNoParams    = `{"error":{"code":"invalid_parameter","message":"unknown parameter; this endpoint accepts no parameters"}}` + "\n"
	wantHealthOK    = `{"status":"ok"}` + "\n"

	// productMarker is an id no log line or error body may carry; the stub
	// echoes it in its 404 answer, like the real engine does.
	productMarker = "s3cr3t-id-777"
)

// getDoc answers GET /products/_doc/{id}: the document 9990000000002 or a 404
// that echoes the id.
func (s *stubSearch) getDoc(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.gets++
	s.getURI = r.RequestURI
	s.mu.Unlock()
	if s.missing.Load() {
		writeJSON(w, http.StatusNotFound, `{"error":{"root_cause":[{"type":"index_not_found_exception","reason":"stub-secret-text"}],"type":"index_not_found_exception","reason":"stub-secret-text"},"status":404}`)
		return
	}
	if r.URL.Path == "/products/_doc/9990000000002" {
		writeJSON(w, http.StatusOK, `{"_index":"products-stub","_id":"9990000000002","_version":1,"found":true,"_source":{"name":"Green Tea <b>&","categories":["en:beverages","en:teas"],"nutriscore":""}}`)
		return
	}
	id, _ := json.Marshal(strings.TrimPrefix(r.URL.Path, "/products/_doc/"))
	writeJSON(w, http.StatusNotFound, `{"_index":"products-stub","_id":`+string(id)+`,"found":false}`)
}

// aliasExists answers HEAD /_alias/products: 404 when the alias is missing.
func (s *stubSearch) aliasExists(w http.ResponseWriter) {
	s.mu.Lock()
	s.aliasChecks++
	s.mu.Unlock()
	if s.missing.Load() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// productCalls returns the get and alias-check counters and the raw URI of the
// last get.
func (s *stubSearch) productCalls() (gets, aliasChecks int, getURI string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.aliasChecks, s.getURI
}

// waitStderr waits, bounded, until the child's stderr holds every want.
func waitStderr(t *testing.T, p *apiProc, wants ...string) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		log := p.stderr.String()
		ok := true
		for _, w := range wants {
			ok = ok && strings.Contains(log, w)
		}
		if ok {
			return log
		}
		select {
		case <-p.stderr.updated:
		case <-timer.C:
			t.Fatalf("stderr lacks %q within 5s:\n%s", wants, log)
		}
	}
}

// productSubtests runs the /products/{id} and /healthz subtests. They leave
// every switch cleared.
func productSubtests(t *testing.T, stub *stubSearch, client *http.Client, base string, proc *apiProc) {
	t.Run("product returns the exact object and asks for one get by id", func(t *testing.T) {
		gets, aliases, _ := stub.productCalls()
		got := do(t, client, http.MethodGet, base+"/products/9990000000002")
		wantJSONReply(t, got, http.StatusOK, wantProductJSON)
		if !strings.Contains(wantSearchJSON, strings.TrimSuffix(got.body, "\n")) {
			t.Errorf("product %q is not an item of the search answer", got.body)
		}
		g, a, uri := stub.productCalls()
		if g-gets != 1 || a != aliases || uri != "/products/_doc/9990000000002" {
			t.Errorf("stub saw %d gets (want 1), %d alias checks (want 0), uri %q", g-gets, a-aliases, uri)
		}
	})

	t.Run("an unknown id is the fixed 404 and echoes nothing", func(t *testing.T) {
		got := do(t, client, http.MethodGet, base+"/products/"+productMarker)
		wantJSONReply(t, got, http.StatusNotFound, wantNotFound)
	})

	t.Run("invalid ids are the same 404 and never reach OpenSearch", func(t *testing.T) {
		gets, _, _ := stub.productCalls()
		for _, p := range []string{strings.Repeat("9", 129), "%FF", "%00", "%7F", "%C2%85"} {
			wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/"+p), http.StatusNotFound, wantNotFound)
		}
		if g, _, _ := stub.productCalls(); g != gets {
			t.Errorf("stub saw %d gets for invalid ids, want 0", g-gets)
		}
	})

	t.Run("a slash in the id stays one escaped path segment", func(t *testing.T) {
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/a%2Fb"), http.StatusNotFound, wantNotFound)
		if _, _, uri := stub.productCalls(); uri != "/products/_doc/a%2Fb" {
			t.Errorf("stub saw RequestURI %q, want /products/_doc/a%%2Fb", uri)
		}
	})

	t.Run("a query string is 400 on both endpoints and makes no engine call", func(t *testing.T) {
		gets, aliases, _ := stub.productCalls()
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/9990000000002?x=1"), http.StatusBadRequest, wantNoParams)
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/healthz?x=1"), http.StatusBadRequest, wantNoParams)
		if g, a, _ := stub.productCalls(); g != gets || a != aliases {
			t.Errorf("stub saw %d gets and %d alias checks, want 0 and 0", g-gets, a-aliases)
		}
	})

	t.Run("healthz is 200 ok after one alias check", func(t *testing.T) {
		_, aliases, _ := stub.productCalls()
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/healthz"), http.StatusOK, wantHealthOK)
		if _, a, _ := stub.productCalls(); a-aliases != 1 {
			t.Errorf("stub saw %d alias checks, want 1", a-aliases)
		}
	})

	t.Run("a missing index or alias is a fixed 503 with no engine text, then recovery", func(t *testing.T) {
		stub.missing.Store(true)
		t.Cleanup(func() { stub.missing.Store(false) })
		gets, aliases, _ := stub.productCalls()
		for _, p := range []string{"/healthz", "/products/9990000000002"} {
			got := do(t, client, http.MethodGet, base+p)
			wantJSONReply(t, got, http.StatusServiceUnavailable, unavailableJSON)
			for _, leak := range []string{"stub-secret-text", "index_not_found"} {
				if strings.Contains(got.body, leak) {
					t.Errorf("%s body leaks %q: %s", p, leak, got.body)
				}
			}
		}
		if g, a, _ := stub.productCalls(); g-gets != 1 || a-aliases != 1 {
			t.Errorf("stub saw %d gets and %d alias checks, want 1 and 1", g-gets, a-aliases)
		}

		stub.missing.Store(false)
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/healthz"), http.StatusOK, wantHealthOK)
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/9990000000002"), http.StatusOK, wantProductJSON)
	})

	t.Run("HEAD is 200 without a body and POST is 405 on both endpoints", func(t *testing.T) {
		gets, aliases, _ := stub.productCalls()
		for _, p := range []string{"/products/9990000000002", "/healthz"} {
			if got := do(t, client, http.MethodHead, base+p); got.status != http.StatusOK || got.body != "" {
				t.Errorf("HEAD %s = %d %q, want 200 and no body", p, got.status, got.body)
			}
		}
		if g, a, _ := stub.productCalls(); g-gets != 1 || a-aliases != 1 {
			t.Errorf("stub saw %d gets and %d alias checks for HEAD, want 1 and 1", g-gets, a-aliases)
		}
		gets, aliases, _ = stub.productCalls()
		for _, p := range []string{"/products/9990000000002", "/healthz"} {
			got := do(t, client, http.MethodPost, base+p)
			if got.status != http.StatusMethodNotAllowed || got.header.Get("Allow") != "GET, HEAD" {
				t.Errorf("POST %s = %d Allow %q, want 405 and GET, HEAD", p, got.status, got.header.Get("Allow"))
			}
		}
		if g, a, _ := stub.productCalls(); g != gets || a != aliases {
			t.Errorf("stub saw %d gets and %d alias checks for POST, want 0 and 0", g-gets, a-aliases)
		}
	})

	t.Run("stderr never carries the id or the engine text", func(t *testing.T) {
		log := waitStderr(t, proc, "get product failed", "health check failed")
		for _, leak := range []string{productMarker, "stub-secret-text"} {
			if strings.Contains(log, leak) {
				t.Errorf("API stderr leaks %q:\n%s", leak, log)
			}
		}
	})
}
