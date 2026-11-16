// Command api serves the product search HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/httpapi"
	"github.com/AleixP/go-product-search/internal/opensearch"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	defaultAddr       = "127.0.0.1:8080"
	defaultOpenSearch = "http://localhost:9200"
)

// shutdownTimeout bounds the graceful shutdown; a variable so tests can shorten it.
var shutdownTimeout = 10 * time.Second

// listen opens the listening socket; a variable so tests can inject failures.
var listen = net.Listen

// backend is what the API needs from the search engine: search, suggest, get by id and health check.
type backend interface {
	catalog.Searcher
	catalog.Suggester
	catalog.Getter
	catalog.HealthChecker
}

// openSearcher connects to OpenSearch and returns it as a backend.
func openSearcher(ctx context.Context, addr string) (backend, error) {
	c, err := opensearch.New(ctx, addr)
	if err != nil {
		return nil, err // not c: a nil *Client in the interface would not be nil
	}
	return c, nil
}

// bindsAllInterfaces reports whether a listener address has an empty or
// unspecified host (0.0.0.0, ::), that is, accepts connections on every interface.
func bindsAllInterfaces(a net.Addr) bool {
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return false
	}
	host, _, _ = strings.Cut(host, "%") // drop an IPv6 zone
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// signalContext is cancelled by the first SIGINT or SIGTERM. It then stops
// listening, so a second signal gets the default behaviour and kills the process
// (signal.Stop restores it once no channel is registered).
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

func main() {
	ctx, stop := signalContext()
	code := run(ctx, os.Args[1:], os.Stderr, openSearcher)
	stop()
	os.Exit(code)
}

func newLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// newServer returns the HTTP server with bounded timeouts and header size.
func newServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// run serves until ctx is done and returns the process exit code. Logs and
// errors go to stderr.
func run(ctx context.Context, args []string, stderr io.Writer,
	newSearcher func(ctx context.Context, addr string) (backend, error)) int {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultAddr, "address to listen on (host:port)")
	osAddr := fs.String("opensearch", defaultOpenSearch, "OpenSearch address (http or https)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: api [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage // the flag package already printed the error and the usage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "api: unexpected argument(s): %d positional argument(s) given, none allowed\n", fs.NArg())
		fs.Usage()
		return exitUsage
	}
	log := newLogger(stderr)

	s, err := newSearcher(ctx, *osAddr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "api: %v\n", err)
		return exitFailure
	}
	ln, err := listen("tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "api: %v\n", err)
		return exitFailure
	}
	if bindsAllInterfaces(ln.Addr()) {
		log.Warn("listening on all interfaces; the API has no authentication", "addr", ln.Addr().String())
	}
	srv := newServer(httpapi.New(s, s, s, s, log), log)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String()) // contract: read by the e2e test

	select {
	case err := <-serveErr:
		log.Error("serve", "error", err)
		return exitFailure
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Error("shutdown", "error", err)
		return exitFailure
	}
	return exitOK
}
