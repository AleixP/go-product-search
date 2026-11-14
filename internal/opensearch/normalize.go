package opensearch

import (
	"context"
	"errors"
	"fmt"
	"slices"

	osclient "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// normalizerName is the keyword normalizer mapping.json defines and the
// brands.norm and categories.norm subfields use.
const normalizerName = "folded_keyword"

// maxAnalyzeInFlight bounds the _analyze requests one Client has in flight.
// The engine runs _analyze on a pool with queue_size 16 and answers 429
// (rejected_execution_exception) beyond it; 50 concurrent clients on a mixed
// workload measured 0.3-0.4% of 429s without a bound. Half the queue leaves
// room for other clients of the same engine.
// shortcut: a Client-wide limit, one engine node's queue is assumed; when
// filter keys must be folded without the engine, move the folding into Go.
const maxAnalyzeInFlight = 8

// analyzeBound is the bound New gives a Client; a variable only so tests can
// shrink it.
var analyzeBound = maxAnalyzeInFlight

// normalize returns the canonical keys of the brands and categories filter
// values: the engine's own normalizer output, so the keys match the indexed
// terms exactly. One _analyze call carries the brands values followed by the
// categories values (no call when both are empty); the tokens are split back
// by count. Each field comes back deduplicated and sorted bytewise. The
// caller owns the deadline; values are never part of an error.
func (c *Client) normalize(ctx context.Context, brands, categories []string) (brandKeys, categoryKeys []string, err error) {
	if len(brands)+len(categories) == 0 {
		return nil, nil, nil
	}
	text := make([]string, 0, len(brands)+len(categories))
	text = append(append(text, brands...), categories...)
	tokens, err := c.analyze(ctx, text)
	if err != nil {
		return nil, nil, err
	}
	for i, tk := range tokens {
		if tk == "" {
			return nil, nil, fmt.Errorf("search products: normalize %s: empty token", fieldOf(i, len(brands)))
		}
	}
	if len(brands) > 0 {
		brandKeys = canonical(tokens[:len(brands)])
	}
	if len(categories) > 0 {
		categoryKeys = canonical(tokens[len(brands):])
	}
	return brandKeys, categoryKeys, nil
}

// fieldOf names the field of the i-th value of the combined text.
func fieldOf(i, brands int) string {
	if i < brands {
		return "brands"
	}
	return "categories"
}

// canonical sorts bytewise and removes the duplicates, in place.
func canonical(keys []string) []string {
	slices.Sort(keys)
	return slices.Compact(keys)
}

// analyze runs one _analyze call for text, after waiting for a slot, and
// returns one token per value. The slot is held for the call only.
func (c *Client) analyze(ctx context.Context, text []string) ([]string, error) {
	select {
	case c.analyzeSlots <- struct{}{}:
		defer func() { <-c.analyzeSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("search products: normalize %s: waiting for an analyze slot: %w", filtersLabel, ctx.Err())
	}
	resp, err := c.api.Indices.Analyze(ctx, opensearchapi.IndicesAnalyzeReq{
		Index: aliasName,
		Body:  opensearchapi.IndicesAnalyzeBody{Normalizer: normalizerName, Text: text},
	})
	if err != nil {
		return nil, normalizeError(resp.Inspect(), err)
	}
	if len(resp.Tokens) != len(text) {
		return nil, fmt.Errorf("search products: normalize %s: got %d tokens for %d values", filtersLabel, len(resp.Tokens), len(text))
	}
	tokens := make([]string, len(resp.Tokens))
	for i, tk := range resp.Tokens {
		tokens[i] = tk.Token
	}
	return tokens, nil
}

// filtersLabel names the combined brands and categories call in errors.
const filtersLabel = "filters"

// normalizeError wraps a failed _analyze call. Engine error reasons can quote
// the request text, so a StructError is reduced to its status and type, and
// any other answer to its HTTP status. Only errors without an answer (the
// transport, the context) keep their cause, without the request URL.
func normalizeError(in opensearchapi.Inspect, err error) error {
	prefix := "search products: normalize " + filtersLabel + ": "
	var structErr *osclient.StructError
	if errors.As(err, &structErr) {
		return fmt.Errorf("%sstatus %d, type %s", prefix, structErr.Status, structErr.Err.Type)
	}
	if in.Response != nil {
		return fmt.Errorf("%sundecodable response (status %d)", prefix, in.Response.StatusCode)
	}
	if errors.Is(err, osclient.ErrJSONUnmarshalBody) {
		// A 2xx body that does not decode: the client drops the response.
		return errors.New(prefix + "undecodable response")
	}
	return fmt.Errorf("%s%w", prefix, stripURL(in, err))
}
