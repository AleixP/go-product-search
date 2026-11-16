package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	osclient "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/AleixP/go-product-search/internal/catalog"
)

var _ catalog.Getter = (*Client)(nil)

// Get returns the product whose document _id is id. The caller owns the
// deadline. Errors never carry the id, the request URL or a response body: an
// engine error object is reduced to its status and type, and a missing
// document is the bare catalog.ErrNotFound.
func (c *Client) Get(ctx context.Context, id string) (catalog.Product, error) {
	// url.PathEscape leaves dot segments as they are, and an intermediary could
	// normalize /products/_doc/.. so these ids are never sent. A product whose
	// code is "." or ".." cannot be fetched.
	if id == "" || id == "." || id == ".." {
		return catalog.Product{}, catalog.ErrNotFound
	}
	resp, err := c.api.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: aliasName, DocumentID: id})
	if err != nil {
		return catalog.Product{}, getError(resp.Inspect(), err)
	}
	if !resp.Found {
		return catalog.Product{}, catalog.ErrNotFound
	}
	var p catalog.Product
	// The error is dropped on purpose so no source data can reach logs.
	if json.Unmarshal(resp.Source, &p) != nil {
		return catalog.Product{}, errors.New("get product: decode source")
	}
	p.ID = resp.ID
	if p.Brands == nil {
		p.Brands = []string{}
	}
	if p.Categories == nil {
		p.Categories = []string{}
	}
	return p, nil
}

// getError maps a failed document GET. A 404 that carries found:false is a
// missing product; any other failure keeps only status and type.
func getError(in opensearchapi.Inspect, err error) error {
	if in.Response == nil || in.Response.StatusCode != http.StatusNotFound {
		return engineError("get product", in, err)
	}
	var structErr *osclient.StructError
	if errors.As(err, &structErr) {
		// For example index_not_found_exception: the alias is missing.
		return engineError("get product", in, err)
	}
	// ParseError puts the body back as an in-memory reader.
	var doc struct {
		Found *bool `json:"found"`
	}
	body, rerr := io.ReadAll(in.Response.Body)
	if rerr == nil && json.Unmarshal(body, &doc) == nil && doc.Found != nil && !*doc.Found {
		// Never wrap err: its StringError carries the whole body, including the _id.
		return catalog.ErrNotFound
	}
	return fmt.Errorf("get product: undecodable response (status %d)", in.Response.StatusCode)
}
