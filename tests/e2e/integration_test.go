package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/opensearch"
)

// integrationImage is the OpenSearch image the integration test runs. It must
// equal the one docker-compose.yml runs (TestIntegrationImage_MatchesDockerCompose).
const integrationImage = "opensearchproject/opensearch:2.19.7"

// Every wait of the integration test is bounded (see also the constants in
// api_test.go and runIngest).
const (
	containerStartWait = 5 * time.Minute  // image pull, start and the module's 120 s readiness wait
	guardWait          = 10 * time.Second // one request of the live-cluster guard
	teardownWait       = 60 * time.Second // stop grace of the container at cleanup
	interruptGrace     = 30 * time.Second // a signalled container start may finish before it is cancelled
)

const (
	javaOptsValue = "-Xms512m -Xmx512m" // heap of the test container, as in docker-compose.yml
	javaOpts      = "OPENSEARCH_JAVA_OPTS=" + javaOptsValue
)

const shortSkipMessage = "starts an OpenSearch container; skipped under -short"

// skipCheckHelper is set only in the child of TestIntegration_SkipsUnderShort:
// there startOpenSearch fails before it touches Docker, so a dropped skip
// cannot start a container.
var skipCheckHelper = os.Getenv("INTEGRATION_HELPER") == "skip-check"

const (
	liveClusterPort = 9200 // the compose cluster; the integration test never talks to it
	loopback        = "127.0.0.1"
)

var (
	errInvalidAddr = errors.New("guard: the address is not a valid http(s) URL")
	errRefusedPort = fmt.Errorf("guard: refusing port %d, the port of the local compose cluster", liveClusterPort)
)

// freshClusterError returns nil only when addr is a cluster that has never
// held product data: no products alias and no products-* index. It checks the
// address first (invalid URL, port 9200) and sends nothing in that case.
// Messages are fixed text plus a status code, never a response body.
func freshClusterError(ctx context.Context, addr string) error {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errInvalidAddr
	}
	// The port is compared as a number: "09200" is port 9200 as well.
	if port, err := strconv.Atoi(u.Port()); err == nil && port == liveClusterPort {
		return errRefusedPort
	}
	client := &http.Client{
		Timeout:   guardWait,
		Transport: &http.Transport{DisableKeepAlives: true},
		// A redirect is never followed: the guard answers about this address only.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	status, _, err := guardGet(ctx, client, addr+"/_alias/"+alias)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("guard: GET /_alias/%s answered %d, want 404 (the alias must not exist)", alias, status)
	}

	status, body, err := guardGet(ctx, client, addr+"/_cat/indices/"+alias+"-*?format=json")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("guard: GET /_cat/indices/%s-* answered %d, want 200", alias, status)
	}
	var indices []json.RawMessage
	if err := json.Unmarshal(body, &indices); err != nil || indices == nil {
		return fmt.Errorf("guard: GET /_cat/indices/%s-* did not answer a JSON array", alias)
	}
	if len(indices) != 0 {
		return fmt.Errorf("guard: the cluster already has %d %s-* indices", len(indices), alias)
	}
	return nil
}

// guardGet sends one GET and returns the status and a size-limited body. Its
// errors name only the path, never the transport error (it may quote the URL).
func guardGet(ctx context.Context, c *http.Client, rawURL string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, errInvalidAddr
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("guard: GET %s failed: %w", req.URL.Path, errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("guard: reading the reply of GET %s failed", req.URL.Path)
	}
	return resp.StatusCode, body, nil
}

// dockerProblem reports why no usable Docker daemon is available, or nil. The
// whole probe, including the library's own host lookup (it pings the daemon
// with a context it does not take from us), runs under dockerProbeWait. The
// library panics when it finds no Docker host: probeDocker recovers it.
func dockerProblem(ctx context.Context) error {
	return probeDocker(ctx, dockerProbeWait, func(ctx context.Context) error {
		provider, err := testcontainers.ProviderDefault.GetProvider()
		if err != nil {
			return err
		}
		defer func() { _ = provider.Close() }()
		return provider.Health(ctx)
	})
}

// startOpenSearch runs a fresh OpenSearch container published on random
// loopback ports and returns its address. Without Docker the test is skipped,
// or failed when CI is set.
//
// Cleanup on every exit path: t.Cleanup terminates the container. A signal
// (parent is cancelled by notifyInterrupt) does not abort the start at once:
// cancelling the library while it creates the Ryuk reaper leaves that
// container in state Created, and it then blocks the next run of the same
// test session. The start gets interruptGrace to finish (about 7 s with the
// image cached) and the test then fails with the container cleaned up; only a
// start that is still pulling the image after the grace is cancelled. A
// second signal kills the process as usual. If the process dies without
// running cleanups (SIGKILL, a -test.timeout panic), Ryuk removes the
// containers about 10 s later.
//
// shortcut: residual windows, not closed. (1) A SIGKILL, or a second signal,
// before the library registers with Ryuk (the first second of the start) leaves
// a container, in state Created or running, that no reaper removes. (2) A
// signal that arrives during an image pull and cancels the start after the
// grace can leave a Created container the same way. Remove such containers
// with: docker rm -f $(docker ps -aq --filter label=org.testcontainers)
func startOpenSearch(t *testing.T, parent context.Context) string {
	t.Helper()
	if skipCheckHelper {
		t.Fatal("container start attempted in skip-check helper")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), containerStartWait)
	defer cancel()
	stopGraceTimer := context.AfterFunc(parent, func() { time.AfterFunc(interruptGrace, cancel) })
	defer stopGraceTimer()

	if err := dockerProblem(ctx); err != nil {
		msg := fmt.Sprintf("integration test needs a running Docker daemon: %s", strings.ReplaceAll(err.Error(), "\n", "; "))
		if os.Getenv("CI") != "" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}

	start := time.Now()
	ctr, err := opensearch.Run(ctx, integrationImage,
		testcontainers.WithEnv(map[string]string{"OPENSEARCH_JAVA_OPTS": javaOptsValue}),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			// An empty HostPort makes the library ask Docker for a random one.
			hc.PortBindings = network.PortMap{
				network.MustParsePort("9200/tcp"): {{HostIP: netip.MustParseAddr(loopback)}},
				network.MustParsePort("9600/tcp"): {{HostIP: netip.MustParseAddr(loopback)}},
			}
		}),
	)
	// Registered before CleanupContainer, so it runs after the termination
	// (cleanups run last-in-first-out) and fails the test if the container
	// is still there: a dropped CleanupContainer must not pass silently.
	t.Cleanup(func() { checkContainerGone(t, ctr) })
	testcontainers.CleanupContainer(t, ctr, testcontainers.StopTimeout(teardownWait))
	if err != nil {
		t.Fatalf("start %s: %v", integrationImage, err)
	}
	if parent.Err() != nil {
		t.Fatal("interrupted by a signal while the container started")
	}

	info, err := ctr.Inspect(ctx)
	if err != nil {
		t.Fatalf("inspect container: %v", err)
	}
	if !slices.Contains(info.Config.Env, javaOpts) {
		t.Fatalf("container env lacks %q", javaOpts)
	}
	var port string
	for _, p := range []string{"9200/tcp", "9600/tcp"} {
		bindings := info.NetworkSettings.Ports[network.MustParsePort(p)]
		if len(bindings) == 0 {
			t.Fatalf("container publishes no host binding for %s", p)
		}
		for _, b := range bindings {
			if b.HostIP != netip.MustParseAddr(loopback) {
				t.Fatalf("%s is published on %v, want %s only", p, b.HostIP, loopback)
			}
		}
		if p == "9200/tcp" {
			port = bindings[0].HostPort
		}
	}
	addr := "http://" + loopback + ":" + port
	t.Logf("OpenSearch %s at %s, started in %v", integrationImage, addr, time.Since(start).Round(time.Millisecond))
	return addr
}

// composeHasImage reports whether a line of the compose file is exactly
// "image: <image>" (indentation and a trailing space allowed). A longer tag or
// a commented-out line does not count.
func composeHasImage(compose, image string) bool {
	re := regexp.MustCompile(`(?m)^[ \t]*image:[ \t]+` + regexp.QuoteMeta(image) + `[ \t\r]*$`)
	return re.MatchString(compose)
}

func TestIntegrationImage_MatchesDockerCompose(t *testing.T) {
	b, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !composeHasImage(string(b), integrationImage) {
		t.Errorf("docker-compose.yml has no line %q", "image: "+integrationImage)
	}
}

func TestComposeHasImage(t *testing.T) {
	const img = "opensearchproject/opensearch:2.19.7"
	tests := []struct {
		name    string
		compose string
		want    bool
	}{
		{"exact line", "services:\n  os:\n    image: " + img + "\n", true},
		{"no final newline", "image: " + img, true},
		{"trailing space and CRLF", "  image: " + img + " \r\n", true},
		{"longer tag", "  image: " + img + "-x\n", false},
		{"longer patch", "  image: " + img + "1\n", false},
		{"other repository prefix", "  image: x/" + img + "\n", false},
		{"commented out", "  # image: " + img + "\n", false},
		{"image appears only in a value", "  command: image: " + img + "\n", false},
		{"different version", "  image: opensearchproject/opensearch:2.19.6\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := composeHasImage(tt.compose, img); got != tt.want {
				t.Errorf("composeHasImage(%q) = %v, want %v", tt.compose, got, tt.want)
			}
		})
	}
}

// guardStub serves the two guard requests and counts every request it sees.
type guardStub struct {
	srv         *httptest.Server
	requests    atomic.Int32
	aliasStatus int    // status of GET /_alias/products
	indicesCode int    // status of GET /_cat/indices/products-*
	indicesBody string // body of that reply
	aliasBody   string
}

func newGuardStub(t *testing.T, aliasStatus int, indicesCode int, indicesBody string) *guardStub {
	t.Helper()
	g := &guardStub{aliasStatus: aliasStatus, indicesCode: indicesCode, indicesBody: indicesBody, aliasBody: `{}`}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_alias/"+alias:
			writeJSON(w, g.aliasStatus, g.aliasBody)
		case r.Method == http.MethodGet && r.URL.Path == "/_cat/indices/"+alias+"-*" && r.URL.Query().Get("format") == "json":
			writeJSON(w, g.indicesCode, g.indicesBody)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func TestFreshClusterError_FreshClusterIsNil(t *testing.T) {
	g := newGuardStub(t, http.StatusNotFound, http.StatusOK, `[]`)
	if err := freshClusterError(context.Background(), g.srv.URL); err != nil {
		t.Fatalf("freshClusterError = %v, want nil", err)
	}
	if n := g.requests.Load(); n != 2 {
		t.Errorf("stub saw %d requests, want 2", n)
	}
}

func TestFreshClusterError_RejectsUsedClusters(t *testing.T) {
	tests := []struct {
		name        string
		aliasStatus int
		indicesCode int
		indicesBody string
	}{
		{"alias exists", http.StatusOK, http.StatusOK, `[]`},
		{"alias answers 500", http.StatusInternalServerError, http.StatusOK, `[]`},
		{"an index exists", http.StatusNotFound, http.StatusOK, `[{"index":"products-20200101t000000z"}]`},
		{"index list answers 500", http.StatusNotFound, http.StatusInternalServerError, `[]`},
		{"index list answers 404", http.StatusNotFound, http.StatusNotFound, `[]`},
		{"index list is not JSON", http.StatusNotFound, http.StatusOK, `not json`},
		{"index list is null", http.StatusNotFound, http.StatusOK, `null`},
		{"index list is an object", http.StatusNotFound, http.StatusOK, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuardStub(t, tt.aliasStatus, tt.indicesCode, tt.indicesBody)
			if err := freshClusterError(context.Background(), g.srv.URL); err == nil {
				t.Fatal("freshClusterError = nil, want an error")
			}
		})
	}
}

func TestFreshClusterError_NeverQuotesTheResponseBody(t *testing.T) {
	const marker = "BODY-MARKER-4f7a"
	tests := []struct {
		name        string
		aliasStatus int
		aliasBody   string
		indicesCode int
		indicesBody string
	}{
		{"alias body", http.StatusOK, marker, http.StatusOK, `[]`},
		{"alias error body", http.StatusInternalServerError, marker, http.StatusOK, `[]`},
		{"index list error body", http.StatusNotFound, `{}`, http.StatusInternalServerError, marker},
		{"index list garbage body", http.StatusNotFound, `{}`, http.StatusOK, marker},
		{"index list content", http.StatusNotFound, `{}`, http.StatusOK, `[{"index":"` + marker + `"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGuardStub(t, tt.aliasStatus, tt.indicesCode, tt.indicesBody)
			g.aliasBody = tt.aliasBody
			err := freshClusterError(context.Background(), g.srv.URL)
			if err == nil {
				t.Fatal("freshClusterError = nil, want an error")
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error %q quotes the response body", err)
			}
		})
	}
}

func TestFreshClusterError_RefusesPort9200BeforeAnyRequest(t *testing.T) {
	// The hosts do not resolve (.invalid), so even a broken guard could not
	// reach the live cluster from this test; the sentinel proves the refusal
	// came from the port check and not from a failed request.
	for _, addr := range []string{
		"http://host.invalid:9200",
		"http://host.invalid:9200/",
		"https://host.invalid:9200",
		"http://user:pass@host.invalid:9200",
		"http://host.invalid:09200",
		"http://host.invalid:0009200",
	} {
		t.Run(addr, func(t *testing.T) {
			err := freshClusterError(context.Background(), addr)
			if !errors.Is(err, errRefusedPort) {
				t.Errorf("freshClusterError(%q) = %v, want errRefusedPort", addr, err)
			}
		})
	}
}

func TestFreshClusterError_RejectsInvalidURLs(t *testing.T) {
	for _, addr := range []string{"", "://", "127.0.0.1:1234", "ftp://127.0.0.1:1234", "http://", "http://%zz", "http://host.invalid:+9200", "http://host.invalid:-9200"} {
		t.Run(addr, func(t *testing.T) {
			if err := freshClusterError(context.Background(), addr); !errors.Is(err, errInvalidAddr) {
				t.Errorf("freshClusterError(%q) = %v, want errInvalidAddr", addr, err)
			}
		})
	}
}

func TestFreshClusterError_DoesNotFollowRedirects(t *testing.T) {
	// Without the redirect rule the guard would follow the 302 to a server
	// that answers like a fresh cluster and pass.
	for _, path := range []string{"/_alias/" + alias, "/_cat/indices/" + alias + "-*"} {
		t.Run(path, func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetHits.Add(1)
				if strings.HasPrefix(r.URL.Path, "/_alias/") {
					writeJSON(w, http.StatusNotFound, `{}`)
					return
				}
				writeJSON(w, http.StatusOK, `[]`)
			}))
			t.Cleanup(target.Close)
			g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == path:
					http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
				case strings.HasPrefix(r.URL.Path, "/_alias/"):
					writeJSON(w, http.StatusNotFound, `{}`)
				default:
					writeJSON(w, http.StatusOK, `[]`)
				}
			}))
			t.Cleanup(g.Close)
			if err := freshClusterError(context.Background(), g.URL); err == nil {
				t.Error("freshClusterError = nil, want an error for a redirect")
			}
			if n := targetHits.Load(); n != 0 {
				t.Errorf("the redirect target saw %d requests, want 0", n)
			}
		})
	}
}

func TestFreshClusterError_CapsTheBodyItReads(t *testing.T) {
	// A valid array padded past 1 MiB: only the truncated read makes it invalid.
	g := newGuardStub(t, http.StatusNotFound, http.StatusOK, "["+strings.Repeat(" ", 2<<20)+"]")
	if err := freshClusterError(context.Background(), g.srv.URL); err == nil {
		t.Fatal("freshClusterError = nil, want an error: the body is over the 1 MiB cap")
	}
}

func TestFreshClusterError_TransportErrorIsAnError(t *testing.T) {
	start := time.Now()
	err := freshClusterError(context.Background(), "http://"+closedPort(t))
	if err == nil {
		t.Fatal("freshClusterError = nil, want an error")
	}
	if d := time.Since(start); d > guardWait+time.Second {
		t.Errorf("took %v, want within %v", d, guardWait+time.Second)
	}
}

func TestIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip(shortSkipMessage)
	}
	// Ctrl-C or SIGTERM must reach the cleanups: without this handler Go kills
	// the test binary at once and the container is left behind.
	ctx, stop := notifyInterrupt(context.Background())
	t.Cleanup(stop)
	addr := startOpenSearch(t, ctx)
	if err := freshClusterError(ctx, addr); err != nil {
		t.Fatalf("live-cluster guard: %v", err)
	}

	dir := t.TempDir()
	apiBin := buildAPI(t, dir)
	proc := startGuarded(t, ctx, apiBin, "--addr", "127.0.0.1:0", "--opensearch", addr)
	base := "http://" + proc.waitAddr(t)
	client := &http.Client{Timeout: clientBound, Transport: &http.Transport{DisableKeepAlives: true}}

	t.Run("before ingest search, suggest, product and healthz are 503", func(t *testing.T) {
		for _, path := range []string{"/search?q=milk", "/suggest?q=mi", "/healthz", "/products/9991000000001"} {
			wantJSONReply(t, do(t, client, http.MethodGet, base+path), http.StatusServiceUnavailable, unavailableJSON)
		}
	})

	if !ingestIntegration(t, ctx, buildIngest(t, dir), addr) {
		return
	}
	integrationDataSubtests(t, client, base)
	searchIntegrationSubtests(t, client, base)
	facetsIntegrationSubtests(t, client, base)
	suggestIntegrationSubtests(t, client, base)
	productIntegrationSubtests(t, client, base)
}
