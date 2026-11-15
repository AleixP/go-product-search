package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

var _ catalog.Suggester = (*Client)(nil)

// Suggest runs q against the products alias with one search call. Distinctness
// comes from the collapse on name.norm in search.SuggestBody; this adapter does
// not dedupe. Size is validated upstream (1..20); a non-positive Size here
// yields an empty list instead of a panic. The caller owns the deadline.
// Errors follow Search: never the request URL or a response body, only status
// and type for an engine error object.
func (c *Client) Suggest(ctx context.Context, q catalog.SuggestQuery) ([]catalog.Suggestion, error) {
	body, err := search.SuggestBody(q)
	if err != nil {
		return nil, fmt.Errorf("search products: build query: %w", err)
	}
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{aliasName},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return nil, searchError(resp.Inspect(), err)
	}
	if f := resp.Shards.Failed; f > 0 {
		return nil, fmt.Errorf("search products: %d of %d shards failed", f, resp.Shards.Total)
	}
	if resp.Timeout {
		return nil, errors.New("search products: timed out")
	}
	hits := resp.Hits.Hits
	if n := max(q.Size, 0); len(hits) > n {
		hits = hits[:n]
	}
	out := make([]catalog.Suggestion, len(hits))
	for i, hit := range hits {
		var src struct {
			Name string `json:"name"`
		}
		// The error is dropped on purpose so no source data can reach logs.
		if json.Unmarshal(hit.Source, &src) != nil || src.Name == "" {
			return nil, fmt.Errorf("search products: decode hit %d", i)
		}
		out[i].Text = src.Name
	}
	return out, nil
}
