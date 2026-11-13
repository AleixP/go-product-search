package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	osclient "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/AleixP/go-product-search/internal/catalog"
	"github.com/AleixP/go-product-search/internal/opensearch/search"
)

var _ catalog.Searcher = (*Client)(nil)

// Search runs q against the products alias. The caller owns the deadline.
// Errors never carry the request URL or a response body: OpenSearch error
// objects (status, type, reason) are kept, any other unparsable response is
// reduced to its HTTP status.
func (c *Client) Search(ctx context.Context, q catalog.Query) (catalog.Result, error) {
	body, err := search.Body(q)
	if err != nil {
		return catalog.Result{}, fmt.Errorf("search products: build query: %w", err)
	}
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{aliasName},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return catalog.Result{}, searchError(resp.Inspect(), err)
	}
	if f := resp.Shards.Failed; f > 0 {
		return catalog.Result{}, fmt.Errorf("search products: %d of %d shards failed", f, resp.Shards.Total)
	}
	if resp.Timeout {
		return catalog.Result{}, errors.New("search products: timed out")
	}
	products := make([]catalog.Product, len(resp.Hits.Hits))
	for i, hit := range resp.Hits.Hits {
		p := &products[i]
		// The error is dropped on purpose so no source data can reach logs.
		if json.Unmarshal(hit.Source, p) != nil {
			return catalog.Result{}, fmt.Errorf("search products: decode hit %d", i)
		}
		p.ID = hit.ID
		if p.Brands == nil {
			p.Brands = []string{}
		}
		if p.Categories == nil {
			p.Categories = []string{}
		}
	}
	return catalog.Result{Total: resp.Hits.Total.Value, Products: products}, nil
}

// searchError wraps a failed search call without the request URL or any
// response body text.
func searchError(in opensearchapi.Inspect, err error) error {
	var strErr *osclient.StringError
	if errors.Is(err, osclient.ErrJSONUnmarshalBody) || errors.Is(err, osclient.ErrUnknownOpensearchError) || errors.As(err, &strErr) {
		// The API client drops the response when a 2xx body does not decode,
		// so the status is only known for error responses.
		if in.Response == nil {
			return errors.New("search products: undecodable response")
		}
		return fmt.Errorf("search products: undecodable response (status %d)", in.Response.StatusCode)
	}
	return fmt.Errorf("search products: %w", stripURL(in, err))
}
