package opensearch

import (
	"context"
	"fmt"
	"net/http"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/AleixP/go-product-search/internal/catalog"
)

var _ catalog.HealthChecker = (*Client)(nil)

// CheckHealth sends one HEAD /_alias/products. An answer of 200 proves both
// that the engine responds and that the products alias exists, so no separate
// ping is made. The caller owns the deadline. A HEAD answer has no body, so
// none is read or quoted; a failure with no response keeps its cause and drops
// the request URL.
func (c *Client) CheckHealth(ctx context.Context) error {
	resp, err := c.api.Indices.Alias.Exists(ctx, opensearchapi.AliasExistsReq{Alias: []string{aliasName}})
	if err == nil {
		return nil
	}
	if resp == nil {
		return fmt.Errorf("check health: %w", withoutRequestURL(false, err))
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("check health: alias %s does not exist", aliasName)
	}
	return fmt.Errorf("check health: status %d", resp.StatusCode)
}
