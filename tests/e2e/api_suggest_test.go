package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// This file holds the T-6 subtests of TestAPI_EndToEnd (/suggest). It reuses
// the stub's canned search answer: the adapter reads only _source.name.

const (
	wantSuggestJSON = `{"suggestions":[{"text":"Smoked Mussels In Cottonseed Oil"},{"text":"Green Tea <b>&"}]}` + "\n"
	emptySuggest    = `{"suggestions":[]}` + "\n"

	// suggestMarker is a q value no response, log line or error may carry.
	suggestMarker = "zzmarkerqq"
)

func badParam(msg string) string {
	return `{"error":{"code":"invalid_parameter","message":"` + msg + `"}}` + "\n"
}

// suggestSubtests runs the /suggest subtests. They leave every switch cleared.
func suggestSubtests(t *testing.T, stub *stubSearch, client *http.Client, base string, proc *apiProc) {
	t.Run("suggest returns the names in order and asks for one collapsed search", func(t *testing.T) {
		before := len(stub.analyzed())
		_, _, _, searches := stub.last()
		got := do(t, client, http.MethodGet, base+"/suggest?q=te")
		wantJSONReply(t, got, http.StatusOK, wantSuggestJSON)

		body, method, path, after := stub.last()
		if after-searches != 1 {
			t.Errorf("stub saw %d searches, want 1", after-searches)
		}
		if method != http.MethodPost || path != "/products/_search" {
			t.Errorf("stub saw %s %s, want POST /products/_search", method, path)
		}
		for _, want := range []string{
			`"name.autocomplete"`, `"operator":"and"`, `"collapse":{"field":"name.norm"}`,
			`"track_total_hits":false`, `"size":10`, `"_source":["name"]`,
			`"sort":[{"_score":"desc"},{"name.norm":"asc"}]`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("stub body %s lacks %s", body, want)
			}
		}
		for _, bad := range []string{`"post_filter"`, `"aggs"`} {
			if strings.Contains(body, bad) {
				t.Errorf("stub body %s has %s", body, bad)
			}
		}
		if n := len(stub.analyzed()) - before; n != 0 {
			t.Errorf("stub saw %d analyze calls, want 0", n)
		}
	})

	t.Run("suggest size reaches the engine and caps the list", func(t *testing.T) {
		got := do(t, client, http.MethodGet, base+"/suggest?q=te&size=1")
		wantJSONReply(t, got, http.StatusOK, `{"suggestions":[{"text":"Smoked Mussels In Cottonseed Oil"}]}`+"\n")
		if body, _, _, _ := stub.last(); !strings.Contains(body, `"size":1,`) {
			t.Errorf("stub body %s lacks \"size\":1", body)
		}

		wantJSONReply(t, do(t, client, http.MethodGet, base+"/suggest?q=te&size=20"), http.StatusOK, wantSuggestJSON)
		if body, _, _, _ := stub.last(); !strings.Contains(body, `"size":20,`) {
			t.Errorf("stub body %s lacks \"size\":20", body)
		}
	})

	t.Run("a short or empty q is an empty list without a search", func(t *testing.T) {
		_, _, _, before := stub.last()
		for _, u := range []string{"/suggest?q=a", "/suggest", "/suggest?q=%20", "/suggest?q=%C2%85", "/suggest?q="} {
			wantJSONReply(t, do(t, client, http.MethodGet, base+u), http.StatusOK, emptySuggest)
		}
		if _, _, _, after := stub.last(); after != before {
			t.Errorf("stub saw %d searches for short queries, want 0", after-before)
		}
	})

	t.Run("invalid suggest parameters are 400 and never reach OpenSearch", func(t *testing.T) {
		_, _, _, before := stub.last()
		unknown := badParam("unknown parameter; allowed parameters are q and size")
		sizeMsg := badParam("size must be an integer between 1 and 20")
		tests := []struct{ name, url, body string }{
			{"page", "/suggest?page=1", unknown},
			{"tag as key", "/suggest?%3Cb%3E=1", unknown},
			{"control character", "/suggest?q=%00", badParam("q must not contain control characters")},
			{"q twice", "/suggest?q=a&q=b", badParam("parameter q must appear at most once")},
			{"size zero", "/suggest?q=te&size=0", sizeMsg},
			{"size 21", "/suggest?q=te&size=21", sizeMsg},
			{"size text", "/suggest?q=te&size=abc", sizeMsg},
			{"size twice", "/suggest?q=te&size=5&size=6", badParam("parameter size must appear at most once")},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantJSONReply(t, do(t, client, http.MethodGet, base+tt.url), http.StatusBadRequest, tt.body)
			})
		}
		if _, _, _, after := stub.last(); after != before {
			t.Errorf("stub saw %d searches for invalid requests, want 0", after-before)
		}
	})

	t.Run("a missing index is a fixed 503, q is never logged and the API recovers", func(t *testing.T) {
		stub.missing.Store(true)
		t.Cleanup(func() { stub.missing.Store(false) })
		got := do(t, client, http.MethodGet, base+"/suggest?q="+suggestMarker)
		wantJSONReply(t, got, http.StatusServiceUnavailable, unavailableJSON)
		for _, leak := range []string{"stub-secret-text", "index_not_found", suggestMarker} {
			if strings.Contains(got.body, leak) {
				t.Errorf("body leaks %q: %s", leak, got.body)
			}
		}
		// The stderr copy of the child may lag the response: wait, bounded.
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(proc.stderr.String(), "suggest failed") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		log := proc.stderr.String()
		if !strings.Contains(log, "suggest failed") {
			t.Fatalf("no \"suggest failed\" log line within 5s:\n%s", log)
		}
		if strings.Contains(log, suggestMarker) {
			t.Errorf("API stderr leaks q:\n%s", log)
		}

		stub.missing.Store(false)
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/suggest?q=te"), http.StatusOK, wantSuggestJSON)
	})

	t.Run("HEAD /suggest is 200 without a body and one search", func(t *testing.T) {
		_, _, _, before := stub.last()
		got := do(t, client, http.MethodHead, base+"/suggest?q=te")
		if got.status != http.StatusOK || got.body != "" {
			t.Errorf("got %d %q, want 200 and no body", got.status, got.body)
		}
		if _, _, _, after := stub.last(); after-before != 1 {
			t.Errorf("stub saw %d searches for HEAD, want 1", after-before)
		}
	})

	t.Run("POST /suggest is 405 with Allow and /suggest/ is a plain 404", func(t *testing.T) {
		_, _, _, before := stub.last()
		got := do(t, client, http.MethodPost, base+"/suggest?q=te")
		if got.status != http.StatusMethodNotAllowed {
			t.Errorf("POST status = %d, want 405", got.status)
		}
		if allow := got.header.Get("Allow"); !strings.Contains(allow, "GET") {
			t.Errorf("Allow = %q, want it to list GET", allow)
		}
		got = do(t, client, http.MethodGet, base+"/suggest/")
		if got.status != http.StatusNotFound || got.body != "404 page not found\n" {
			t.Errorf("got %d %q, want 404 and the mux's plain body", got.status, got.body)
		}
		if _, _, _, after := stub.last(); after != before {
			t.Errorf("stub saw %d searches, want 0", after-before)
		}
	})
}
