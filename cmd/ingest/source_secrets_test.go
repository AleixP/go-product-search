package main

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// --- secrets in every failure mode ---

func TestErrors_NeverCarryURLSecrets(t *testing.T) {
	secretQuery := "?token=" + srcSecret
	closedPort := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}()
	tls1 := srcStart(t, srcServe([]byte("x")))
	status := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	redir := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/x"+secretQuery, http.StatusFound)
	}))
	var hits atomic.Int32
	loop := srcStart(t, srcRedirectChain(9, &hits))
	stall := srcStartH(t, srcStall)
	cut := srcStart(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "abc")
	}))

	modes := map[string]func() error{
		"connection refused": func() error {
			_, err := openInput(t.Context(), newHTTPClient(), "https://"+closedPort+"/p"+secretQuery)
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Errorf("connect failure lost its cause: %v", err)
			}
			return err
		},
		"tls verification with the default client": func() error {
			_, err := openInput(t.Context(), newHTTPClient(), tls1.url("/p"+secretQuery))
			var cv *tls.CertificateVerificationError
			if !errors.As(err, &cv) {
				t.Errorf("TLS failure lost its cause: %v", err)
			}
			return err
		},
		"status": func() error {
			_, err := openInput(t.Context(), status.cl, status.url("/p"+secretQuery))
			return err
		},
		"redirect to http": func() error {
			_, err := openInput(t.Context(), redir.cl, redir.url("/p"+secretQuery))
			return err
		},
		"too many redirects": func() error {
			_, err := openInput(t.Context(), loop.cl, loop.url("/r/0"+secretQuery))
			return err
		},
		"idle timeout": func() error {
			src, err := openInputIdle(t.Context(), stall.cl, stall.url("/p"+secretQuery), 100*time.Millisecond)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			_, err = io.ReadAll(src)
			return err
		},
		"truncated body": func() error {
			src, err := openInput(t.Context(), cut.cl, cut.url("/p"+secretQuery))
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			_, err = io.ReadAll(src)
			return err
		},
		"userinfo in the input": func() error {
			_, err := openInput(t.Context(), newHTTPClient(), "https://u:"+srcSecret+"@127.0.0.1/p"+secretQuery)
			return err
		},
		"http input": func() error {
			_, err := openInput(t.Context(), newHTTPClient(), "http://u:"+srcSecret+"@127.0.0.1/p"+secretQuery)
			return err
		},
	}
	for name, fn := range modes {
		t.Run(name, func(t *testing.T) {
			err := fn()

			srcNoLeak(t, err)
		})
	}
}
