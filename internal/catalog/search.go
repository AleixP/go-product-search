package catalog

import "context"

// MaxWindow is the deepest result reachable (page*size), equal to
// OpenSearch's default index.max_result_window.
const MaxWindow = 10000

// Query is one search request. The HTTP layer validates it; adapters trust it.
type Query struct {
	// Text is the user text, already trimmed. "" means all products.
	Text string
	// Page is the 1-based page number, >= 1.
	Page int
	// Size is the page size, >= 1. Page*Size must not exceed MaxWindow.
	Size int
}

// Result is one page of search results.
type Result struct {
	// Total is the exact number of matching products.
	Total int
	// Products is the requested page; never nil.
	Products []Product
}

// Searcher is the storage port for the search API.
type Searcher interface {
	// Search returns the page of products matching q.
	Search(ctx context.Context, q Query) (Result, error)
}
