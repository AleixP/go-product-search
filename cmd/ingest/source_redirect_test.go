package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func srcRedirectChain(hops int, hits *atomic.Int32) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/r/{n}", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := strconv.Atoi(r.PathValue("n"))
		if n < hops {
			http.Redirect(w, r, fmt.Sprintf("/r/%d?%s", n+1, r.URL.RawQuery), http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "arrived\n")
	})
	return mux
}

func TestOpenInput_FollowsUpToFiveRedirects(t *testing.T) {
	var hits atomic.Int32
	s := srcStart(t, srcRedirectChain(5, &hits))
	src := srcOpenURL(t, s, "/r/0")

	got, err := io.ReadAll(src)

	if err != nil || string(got) != "arrived\n" {
		t.Errorf("ReadAll = %q, %v", got, err)
	}
	if hits.Load() != 6 {
		t.Errorf("server saw %d requests, want 6 (1 + 5 redirects)", hits.Load())
	}
}

func TestOpenInput_SixthRedirectFails(t *testing.T) {
	var hits atomic.Int32
	s := srcStart(t, srcRedirectChain(6, &hits))

	_, err := openInput(t.Context(), s.cl, s.url("/r/0?token="+srcSecret))

	if err == nil {
		t.Fatal("six redirects were followed")
	}
	if !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), "5") {
		t.Errorf("error %q should say it stopped after 5 redirects", err)
	}
	if hits.Load() != 6 {
		t.Errorf("server saw %d requests, want 6 (the 6th redirect target is never requested)", hits.Load())
	}
	srcNoLeak(t, err)
}

func TestOpenInput_RedirectToHTTPIsRefused(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainHits.Add(1) }))
	t.Cleanup(plain.Close)
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x?token="+srcSecret, http.StatusFound)
	}))

	_, err := openInput(t.Context(), s.cl, s.url("/start"))

	if err == nil {
		t.Fatal("redirect to http:// was followed")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error %q should say only https is allowed", err)
	}
	if plainHits.Load() != 0 {
		t.Error("the http:// target was contacted")
	}
	srcNoLeak(t, err)
}

// The target of the redirect serves 200, so a followed redirect would succeed
// and the test would see a missing policy check.
func TestOpenInput_RedirectWithUserinfoIsRefused(t *testing.T) {
	for name, userinfo := range map[string]string{
		"user and password":   "u:" + srcSecret,
		"empty user name":     ":" + srcSecret,
		"user name only":      srcSecret,
		"empty user and pass": "",
	} {
		t.Run(name, func(t *testing.T) {
			s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/x" {
					_, _ = io.WriteString(w, "ok\n")
					return
				}
				http.Redirect(w, r, "https://"+userinfo+"@"+r.Host+"/x", http.StatusFound)
			}))

			_, err := openInput(t.Context(), s.cl, s.url("/start"))

			if err == nil {
				t.Fatal("redirect carrying credentials was followed")
			}
			srcNoLeak(t, err)
		})
	}
}

func TestOpenInput_RedirectToAnotherHTTPSPathIsFollowed(t *testing.T) {
	s := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		http.Redirect(w, r, "https://"+r.Host+"/final", http.StatusFound)
	}))
	src := srcOpenURL(t, s, "/start")

	got, err := io.ReadAll(src)

	if err != nil || string(got) != "ok\n" {
		t.Errorf("ReadAll = %q, %v", got, err)
	}
}
