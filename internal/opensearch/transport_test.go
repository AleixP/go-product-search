package opensearch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestNewTransport_RaisesIdleConnectionsPerHost(t *testing.T) {
	tr := newTransport()
	if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost || maxIdleConnsPerHost != 64 {
		t.Errorf("MaxIdleConnsPerHost = %d (const %d), want 64", tr.MaxIdleConnsPerHost, maxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < maxIdleConnsPerHost {
		t.Errorf("MaxIdleConns = %d, want at least %d", tr.MaxIdleConns, maxIdleConnsPerHost)
	}
}

func TestNewTransport_DoesNotTouchDefaultTransport(t *testing.T) {
	def := http.DefaultTransport.(*http.Transport)
	before := def.MaxIdleConnsPerHost
	tr := newTransport()
	if tr == def {
		t.Fatal("newTransport returned http.DefaultTransport itself")
	}
	if def.MaxIdleConnsPerHost != before {
		t.Errorf("http.DefaultTransport.MaxIdleConnsPerHost changed from %d to %d", before, def.MaxIdleConnsPerHost)
	}
	if tr.Proxy == nil || tr.DialContext == nil || tr.IdleConnTimeout == 0 {
		t.Error("clone lost the standard defaults (proxy, dialer, idle timeout)")
	}
}

func TestNewTransport_MaxIdleConnsNeverBelowPerHostNorLowered(t *testing.T) {
	for _, tt := range []struct{ def, want int }{{10, 64}, {100, 100}, {500, 500}} {
		orig := http.DefaultTransport
		http.DefaultTransport = &http.Transport{MaxIdleConns: tt.def}
		got := func() int {
			defer func() { http.DefaultTransport = orig }()
			return newTransport().MaxIdleConns
		}()
		if got != tt.want {
			t.Errorf("MaxIdleConns with default %d = %d, want %d", tt.def, got, tt.want)
		}
	}
}

type notATransport struct{}

func (notATransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrNotSupported
}

func TestNewTransport_FallbackWhenDefaultIsNotATransport(t *testing.T) {
	orig := http.DefaultTransport
	http.DefaultTransport = notATransport{}
	t.Cleanup(func() { http.DefaultTransport = orig })

	tr := newTransport()
	if tr == nil || tr.MaxIdleConnsPerHost != 64 || tr.MaxIdleConns < 64 {
		t.Fatalf("fallback transport = %+v, want MaxIdleConnsPerHost 64", tr)
	}
	if tr.Proxy == nil || tr.DialContext == nil || tr.IdleConnTimeout == 0 || tr.TLSHandshakeTimeout == 0 {
		t.Error("fallback transport lacks the standard defaults")
	}
}

func pingServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestNew_PassesItsOwnTransportToTheClient(t *testing.T) {
	srv := pingServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	var got opensearchapi.Config
	orig := newAPIClient
	newAPIClient = func(cfg opensearchapi.Config) (*opensearchapi.Client, error) {
		got = cfg
		return orig(cfg)
	}
	t.Cleanup(func() { newAPIClient = orig })

	if _, err := New(context.Background(), srv.URL); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tr, ok := got.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport = %T, want *http.Transport", got.Client.Transport)
	}
	if tr == http.DefaultTransport {
		t.Error("client uses http.DefaultTransport itself")
	}
	if tr.MaxIdleConnsPerHost != 64 {
		t.Errorf("client MaxIdleConnsPerHost = %d, want 64", tr.MaxIdleConnsPerHost)
	}
}

// TestNew_ReusesIdleConnectionsUnderConcurrency pins the reason for the raised
// limit: with the default of 2 idle connections per host, a second burst of 50
// concurrent requests opens about 48 new connections (and drops requests on a
// real node); with 64 it reuses the idle ones.
func TestNew_ReusesIdleConnectionsUnderConcurrency(t *testing.T) {
	const n = 50
	var conns atomic.Int64
	var armed atomic.Bool
	var arrived atomic.Int64
	allArrived := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	states := map[net.Conn]http.ConnState{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		if !armed.Load() {
			return
		}
		if arrived.Add(1) == n {
			close(allArrived)
		}
		// Hold every request until all n are in flight, but never indefinitely:
		// a transport that caps concurrency must fail the assertion, not hang.
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-allArrived:
		case <-release:
		case <-timer.C:
		}
	}))
	srv.Config.ConnState = func(c net.Conn, st http.ConnState) {
		mu.Lock()
		states[c] = st
		mu.Unlock()
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close
	c, err := New(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	burst := func() {
		arrived.Store(0)
		allArrived = make(chan struct{})
		armed.Store(true)
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				if _, err := c.api.Ping(ctx, nil); err != nil {
					t.Errorf("Ping() error = %v", err)
				}
			}()
		}
		wg.Wait()
		armed.Store(false)
	}
	burst()
	if t.Failed() {
		t.Fatal("first burst failed: concurrency is capped below the burst size")
	}
	// Let the client park the connections of the first burst as idle: poll until
	// the server sees every connection idle (bounded, not a fixed sleep).
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		idle := 0
		for _, st := range states {
			if st == http.StateIdle {
				idle++
			}
		}
		done := idle == len(states)
		mu.Unlock()
		if done {
			break
		}
	}
	before := conns.Load()
	burst()
	if opened := conns.Load() - before; opened > 5 {
		t.Errorf("second burst opened %d new connections, want at most 5 (idle connections reused)", opened)
	}
}

// trackTransport records the transport New hands to the client and returns a
// function that drops its idle connections. httptest.Server.Close only does
// that for http.DefaultTransport, which the client no longer uses; without it a
// test that closes the server races with the client noticing the dead connection.
func trackTransport(t *testing.T) (closeIdle func()) {
	t.Helper()
	var tr *http.Transport
	orig := newAPIClient
	newAPIClient = func(cfg opensearchapi.Config) (*opensearchapi.Client, error) {
		tr, _ = cfg.Client.Transport.(*http.Transport)
		return orig(cfg)
	}
	t.Cleanup(func() { newAPIClient = orig })
	return func() {
		if tr != nil {
			tr.CloseIdleConnections()
		}
	}
}
