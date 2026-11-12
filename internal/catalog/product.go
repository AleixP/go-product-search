// Package catalog defines the product document and the port used to index it.
package catalog

import "context"

// Product is one searchable document. Its JSON form is the OpenSearch
// document body; ID is the document _id and is not part of the body.
type Product struct {
	// ID is the product code (barcode), used as the document _id.
	ID string `json:"-"`
	// Name is the product name.
	Name string `json:"name"`
	// Brands are the brand names as written in the source, deduplicated.
	Brands []string `json:"brands"`
	// Categories are English category slugs without the "en:" prefix.
	Categories []string `json:"categories"`
	// Nutriscore is the grade "a" to "e", or "" when unknown.
	Nutriscore string `json:"nutriscore"`
}

// BulkResult is the per-item outcome of one Bulk call.
type BulkResult struct {
	// Indexed is the number of documents accepted.
	Indexed int
	// Failed is the number of documents rejected.
	Failed int
	// FailureTypes counts failures by error type.
	FailureTypes map[string]int
	// FailedIDs holds a few sample IDs of failed documents (capped by the adapter).
	FailedIDs []string
}

// Indexer is the storage port for the ingest command.
type Indexer interface {
	// CreateIndex creates a new versioned index and returns its name.
	CreateIndex(ctx context.Context) (string, error)
	// Bulk indexes docs into index. A non-nil error means the whole request
	// failed; failures of single documents are reported in BulkResult.
	Bulk(ctx context.Context, index string, docs []Product) (BulkResult, error)
	// Finalize makes the documents of index searchable.
	Finalize(ctx context.Context, index string) error
	// Count returns the number of documents in index.
	Count(ctx context.Context, index string) (int, error)
	// Publish atomically points the products alias at index and removes
	// the previous products indices. Deleting the old indices is best-effort:
	// a failure there is logged and does not undo the alias swap.
	Publish(ctx context.Context, index string) error
	// Delete removes index.
	Delete(ctx context.Context, index string) error
}
