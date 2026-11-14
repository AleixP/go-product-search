// Package opensearch is the OpenSearch adapter of the catalog.Indexer and
// catalog.Searcher ports.
package opensearch

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	osclient "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

const (
	// aliasName is the alias the search side reads; it always points at one index.
	aliasName = "products"
	// indexPrefix starts the name of every versioned index. Destructive calls
	// are limited to names with this prefix, so the alias name and unrelated
	// indices are never deleted.
	indexPrefix = "products-"
	// indexTimeFormat is lower case because index names must be.
	indexTimeFormat = "20060102t150405z"
	// pingTimeout bounds New so an unreachable cluster fails fast.
	pingTimeout = 10 * time.Second
)

// maxIdleConnsPerHost is the idle connections the client keeps to the cluster.
// The net/http default is 2: at 50 concurrent clients the connections churned
// and 4-6% of searches failed with EOF (QA, OpenSearch 2.19.7); with 64 there
// were 0 errors in 34,098 requests and p95 fell from 101 ms to 39 ms.
const maxIdleConnsPerHost = 64

// newTransport returns the client's own transport: a clone of
// http.DefaultTransport (so proxy, dialer and timeouts keep the standard
// values) with a larger idle pool. The shared default is never modified.
func newTransport() *http.Transport {
	var tr *http.Transport
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = def.Clone()
	} else {
		tr = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	tr.MaxIdleConnsPerHost = maxIdleConnsPerHost
	tr.MaxIdleConns = max(tr.MaxIdleConns, maxIdleConnsPerHost)
	return tr
}

// newAPIClient builds the API client; tests replace it to force a failure.
var newAPIClient = opensearchapi.NewClient

//go:embed mapping.json
var mappingJSON []byte

// Client is the OpenSearch implementation of the catalog.Indexer lifecycle
// methods. Bulk lives in bulk.go.
type Client struct {
	api *opensearchapi.Client
	now func() time.Time
	log *slog.Logger
	// analyzeSlots bounds the _analyze calls in flight (see maxAnalyzeInFlight).
	analyzeSlots chan struct{}
}

// New connects to the cluster at addr (an http or https URL) and pings it, so
// an unreachable address fails here and not in the middle of an ingest.
func New(ctx context.Context, addr string) (*Client, error) {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid OpenSearch address %q: want an http or https URL", redact(addr))
	}
	if u.User != nil {
		return nil, errors.New("invalid OpenSearch address: credentials in the URL are not supported")
	}
	api, err := newAPIClient(opensearchapi.Config{Client: osclient.Config{Addresses: []string{addr}, Transport: newTransport()}})
	if err != nil {
		// Static message plus scheme://host only: addr may carry a path or query with secrets.
		slog.Warn("create OpenSearch client failed", "address", redact(addr))
		return nil, fmt.Errorf("create OpenSearch client: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if resp, err := api.Ping(pctx, nil); err != nil {
		return nil, fmt.Errorf("ping OpenSearch at %s://%s: %w", u.Scheme, u.Host, withoutRequestURL(resp != nil, err))
	}
	return &Client{api: api, now: time.Now, log: slog.Default(), analyzeSlots: make(chan struct{}, analyzeBound)}, nil
}

// redact keeps only the scheme and host of a URL for error messages.
func redact(addr string) string {
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "(unparsable)"
}

// isNotFound reports whether an API call failed with HTTP 404.
func isNotFound(in opensearchapi.Inspect, err error) bool {
	return err != nil && in.Response != nil && in.Response.StatusCode == http.StatusNotFound
}

// CreateIndex creates a new versioned index with the embedded mapping and
// returns its name. It fails if a concrete index named "products" exists,
// because the alias of that name could then never be created.
func (c *Client) CreateIndex(ctx context.Context) (string, error) {
	got, err := c.api.Indices.Get(ctx, opensearchapi.IndicesGetReq{
		Indices: []string{aliasName},
		Params:  opensearchapi.IndicesGetParams{FilterPath: []string{"*.aliases"}},
	})
	switch {
	case err == nil:
		if _, concrete := (*got.IndicesGetRespData)[aliasName]; concrete {
			return "", fmt.Errorf("a concrete index named %q exists; remove it so it can become an alias", aliasName)
		}
	case !isNotFound(got.Inspect(), err):
		return "", fmt.Errorf("check for index %q: %w", aliasName, stripURL(got.Inspect(), err))
	}

	name := indexPrefix + c.now().UTC().Format(indexTimeFormat)
	if resp, err := c.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: name,
		Body:  bytes.NewReader(mappingJSON),
	}); err != nil {
		return "", fmt.Errorf("create index %s: %w", name, stripURL(resp.Inspect(), err))
	}
	return name, nil
}

// Finalize restores the normal refresh interval of index and refreshes it, so
// every indexed document becomes searchable.
func (c *Client) Finalize(ctx context.Context, index string) error {
	if resp, err := c.api.Indices.Settings.Put(ctx, opensearchapi.SettingsPutReq{
		Indices: []string{index},
		Body:    strings.NewReader(`{"index":{"refresh_interval":"1s"}}`),
	}); err != nil {
		return fmt.Errorf("set refresh interval of %s: %w", index, stripURL(resp.Inspect(), err))
	}
	if resp, err := c.api.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Index: []string{index}}); err != nil {
		return fmt.Errorf("refresh %s: %w", index, stripURL(resp.Inspect(), err))
	}
	return nil
}

// Count returns the number of documents in index.
func (c *Client) Count(ctx context.Context, index string) (int, error) {
	resp, err := c.api.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: []string{index}})
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", index, stripURL(resp.Inspect(), err))
	}
	return resp.Count, nil
}

// Publish points the products alias at index in one atomic request, then
// deletes every other products-* index. Deleting the old indices is
// best-effort: a failure there is logged and does not undo the swap.
//
// If the swap request fails, the alias is re-read. An alias that contains
// index counts as success (nil), even next to other indices: those are then
// deleted as stale. An alias still on the old indices is a plain failure. Any
// other state, or a re-read that fails, returns an error wrapping
// ErrPublishStateUnknown and deletes nothing.
func (c *Client) Publish(ctx context.Context, index string) error {
	var actions []map[string]any
	var oldNames []string

	old, err := c.api.Indices.Alias.Get(ctx, opensearchapi.AliasGetReq{Alias: []string{aliasName}})
	switch {
	case err == nil:
		oldNames = slices.Sorted(maps.Keys(old.GetIndices()))
		for _, name := range oldNames {
			actions = append(actions, map[string]any{"remove": map[string]string{"index": name, "alias": aliasName}})
		}
	case !isNotFound(old.Inspect(), err):
		return fmt.Errorf("read alias %s: %w", aliasName, stripURL(old.Inspect(), err))
	}
	actions = append(actions, map[string]any{"add": map[string]string{"index": index, "alias": aliasName}})

	body, err := json.Marshal(map[string]any{"actions": actions})
	if err != nil {
		return fmt.Errorf("encode alias actions: %w", err)
	}
	if resp, err := c.api.Aliases(ctx, opensearchapi.AliasesReq{Body: bytes.NewReader(body)}); err != nil {
		err = withoutRequestURL(resp != nil && resp.Inspect().Response != nil, err)
		if err := c.resolveFailedSwap(ctx, index, oldNames, err); err != nil {
			return err
		}
	}

	c.deleteStale(ctx, index)
	return nil
}

// resolveFailedSwap decides what a failed _aliases call means. The server may
// have applied the swap before the answer was lost, and the transport's retry
// then fails because the alias no longer points at the old indices. So it
// re-reads the alias: pointing at index means the swap stands (nil); still
// pointing at exactly the old indices (or nowhere on a first run) means a plain
// failure; anything else, or a failed re-read, wraps ErrPublishStateUnknown.
func (c *Client) resolveFailedSwap(ctx context.Context, index string, oldNames []string, swapErr error) error {
	plain := fmt.Errorf("swap alias %s to %s: %w", aliasName, index, swapErr)
	unknown := func(why string) error {
		return fmt.Errorf("%w: swap alias %s to %s failed and %s; check the %s alias before retrying: %w",
			ErrPublishStateUnknown, aliasName, index, why, aliasName, swapErr)
	}
	// The caller's context may be what failed the swap; the re-read is a short
	// control call that must not be skipped for it.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pingTimeout)
	defer cancel()
	got, err := c.api.Indices.Alias.Get(rctx, opensearchapi.AliasGetReq{Alias: []string{aliasName}})
	switch {
	case err == nil:
		now := slices.Sorted(maps.Keys(got.GetIndices()))
		switch {
		case slices.Contains(now, index):
			return nil
		case slices.Equal(now, oldNames):
			return plain
		}
		return unknown("the alias points at other indices")
	case isNotFound(got.Inspect(), err):
		if len(oldNames) == 0 {
			return plain
		}
		return unknown("the alias no longer exists")
	}
	return unknown("the alias could not be read back")
}

// deleteStale deletes every products-* index except keep, logging failures.
func (c *Client) deleteStale(ctx context.Context, keep string) {
	all, err := c.api.Indices.Get(ctx, opensearchapi.IndicesGetReq{
		Indices: []string{indexPrefix + "*"},
		Params:  opensearchapi.IndicesGetParams{FilterPath: []string{"*.aliases"}},
	})
	if err != nil {
		c.log.Warn("list old indices failed; they are left in place", "error", stripURL(all.Inspect(), err))
		return
	}
	for _, name := range slices.Sorted(maps.Keys(*all.IndicesGetRespData)) {
		if name == keep {
			continue
		}
		if err := c.Delete(ctx, name); err != nil {
			c.log.Warn("delete old index failed; the alias swap stands", "index", name, "error", err)
		}
	}
}

// Delete removes index. Only concrete names starting with "products-" are
// accepted, so wildcards, lists and the alias name can never be deleted.
func (c *Client) Delete(ctx context.Context, index string) error {
	if !strings.HasPrefix(index, indexPrefix) || strings.ContainsAny(index, "*,?/ ") {
		return fmt.Errorf("refusing to delete %q: not a %s* index", index, indexPrefix)
	}
	if resp, err := c.api.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{index}}); err != nil {
		return fmt.Errorf("delete index %s: %w", index, stripURL(resp.Inspect(), err))
	}
	return nil
}
