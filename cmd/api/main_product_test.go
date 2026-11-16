package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// Get answers every request with one fixed product.
func (fakeSearcher) Get(context.Context, string) (catalog.Product, error) {
	return catalog.Product{ID: "1", Name: "Tea"}, nil
}

// CheckHealth reports a healthy catalog.
func (fakeSearcher) CheckHealth(context.Context) error { return nil }

func TestRun_ServesProductAndHealthz(t *testing.T) {
	addr, cancel, done, _ := startRun(t, []string{"--addr", "127.0.0.1:0"}, fakeSearcher{})
	tests := []struct{ path, want string }{
		{"/products/1", `{"id":"1","name":"Tea","brands":[],"categories":[],"nutriscore":""}` + "\n"},
		{"/healthz", `{"status":"ok"}` + "\n"},
	}
	for _, tt := range tests {
		resp, err := httpClient.Get("http://" + addr + tt.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != tt.want {
			t.Errorf("GET %s = %d %q, want 200 %q", tt.path, resp.StatusCode, body, tt.want)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q", tt.path, ct)
		}
	}
	cancel()
	if code := waitCode(t, done, time.Second); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
}
