package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/AleixP/go-product-search/internal/catalog"
)

var _ catalog.Indexer = (*Client)(nil)

const (
	// maxFailureTypes caps the distinct error types kept per call; later new
	// types are counted under otherFailureType, so FailureTypes has at most
	// 32 named types plus "other" (33 keys).
	maxFailureTypes  = 32
	otherFailureType = "other"
	// maxFailedIDs caps the sample of failed document ids per call.
	maxFailedIDs = 10
	// missingFailureType is the failure type of a document whose result is
	// absent or unreadable in the bulk response.
	missingFailureType = "missing from bulk response"
	// unknownFailureType is used when a failed item carries no error object.
	unknownFailureType = "unknown"
)

// bulkTimeout bounds one Bulk call; tests shorten it.
var bulkTimeout = 60 * time.Second

// bulkAction is the action line that precedes each document: "index" so a
// repeated code overwrites the earlier document.
type bulkAction struct {
	Index struct {
		Index string `json:"_index"`
		ID    string `json:"_id"`
	} `json:"index"`
}

// Bulk indexes docs into index with one _bulk request. A non-nil error means
// the whole request failed (transport, non-2xx, timeout, cancellation) and is
// fatal for the caller; failures of single documents are counted in the
// result. An item whose status is outside 200-299, or is 0 or missing,
// counts as failed. Only products-* indices are accepted.
func (c *Client) Bulk(ctx context.Context, index string, docs []catalog.Product) (catalog.BulkResult, error) {
	if !strings.HasPrefix(index, indexPrefix) || strings.ContainsAny(index, "*,?/ ") {
		return catalog.BulkResult{}, fmt.Errorf("refusing to bulk into %q: not a %s* index", index, indexPrefix)
	}
	if len(docs) == 0 {
		return catalog.BulkResult{}, nil
	}
	body, err := bulkBody(index, docs)
	if err != nil {
		return catalog.BulkResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	// The transport buffers a body that has no GetBody to retry it; a
	// bytes.Reader only saves that copy.
	resp, err := c.api.Bulk(ctx, opensearchapi.BulkReq{Index: index, Body: bytes.NewReader(body)})
	if err != nil {
		// A partial-bulk error comes with a fully populated response and means
		// only some items failed: account for them below. Anything else is a
		// whole-request failure.
		var partial *opensearchapi.PartialBulkError
		if !errors.As(err, &partial) {
			return catalog.BulkResult{}, fmt.Errorf("bulk index %s: %w", index, withoutRequestURL(resp.Inspect().Response != nil, err))
		}
	}
	return c.account(resp, docs), nil
}

// bulkBody builds the NDJSON request body: for each document an action line
// and a document line, every line ending in "\n".
func bulkBody(index string, docs []catalog.Product) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // Encode ends each value with "\n"
	enc.SetEscapeHTML(false)
	for i, d := range docs {
		if strings.TrimSpace(d.ID) == "" {
			// OpenSearch would generate an id and a re-run would duplicate the product.
			// A blank id is refused; any other id is sent untrimmed. The error names
			// the position, not the value.
			return nil, fmt.Errorf("bulk document %d has an empty or blank ID", i)
		}
		var act bulkAction
		act.Index.Index, act.Index.ID = index, d.ID
		if err := enc.Encode(act); err != nil {
			return nil, fmt.Errorf("encode bulk action: %w", err)
		}
		if err := enc.Encode(d); err != nil {
			return nil, fmt.Errorf("encode bulk document: %w", err)
		}
	}
	return buf.Bytes(), nil
}

// account turns the response items into per-document counts. Item i answers
// docs[i]; documents without an item count as failed, and surplus items are
// ignored so that Indexed + Failed always equals len(docs).
func (c *Client) account(resp *opensearchapi.BulkResp, docs []catalog.Product) catalog.BulkResult {
	res := catalog.BulkResult{FailureTypes: map[string]int{}}
	fail := func(id, typ string) {
		res.Failed++
		if _, known := res.FailureTypes[typ]; !known && len(res.FailureTypes) >= maxFailureTypes {
			typ = otherFailureType
		}
		res.FailureTypes[typ]++
		if len(res.FailedIDs) < maxFailedIDs {
			res.FailedIDs = append(res.FailedIDs, id)
		}
	}

	if len(resp.Items) > len(docs) {
		c.log.Warn("bulk response has more items than documents sent; extra items ignored", "items", len(resp.Items), "docs", len(docs))
	}
	for i, d := range docs {
		var item opensearchapi.BulkRespItem
		ok := false
		if i < len(resp.Items) && len(resp.Items[i]) == 1 {
			for _, it := range resp.Items[i] {
				item, ok = it, true
			}
		}
		switch {
		case !ok:
			fail(d.ID, missingFailureType)
		case item.Status < 200 || item.Status >= 300:
			typ := unknownFailureType
			if item.Error != nil && item.Error.Type != "" {
				typ = item.Error.Type
			}
			fail(d.ID, typ)
		default:
			res.Indexed++
		}
	}
	return res
}
