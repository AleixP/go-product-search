package catalog

import "context"

// MaxWindow is the deepest result reachable (page*size), equal to
// OpenSearch's default index.max_result_window.
const MaxWindow = 10000

// NutriscoreGrades lists the Nutri-Score grades in display order.
const NutriscoreGrades = "abcde"

// Query is one search request. The HTTP layer validates it; adapters trust it.
type Query struct {
	// Text is the user text, already trimmed. "" means all products.
	Text string
	// Page is the 1-based page number, >= 1.
	Page int
	// Size is the page size, >= 1. Page*Size must not exceed MaxWindow.
	Size int
	// Brands, Categories and Nutriscore are the selected filter values. The
	// HTTP layer validates them: trimmed, non-empty, deduplicated and sorted;
	// nil when absent. Adapters normalize Brands and Categories themselves.
	Brands     []string
	Categories []string
	Nutriscore []string
}

// Result is one page of search results.
type Result struct {
	// Total is the exact number of matching products.
	Total int
	// Products is the requested page; never nil.
	Products []Product
	// Facets are the facet counts for the query.
	Facets Facets
}

// FacetValue is one facet entry: a value and its product count.
type FacetValue struct {
	Value string
	Count int
}

// Facets holds the facet counts of a search.
type Facets struct {
	Brands     []FacetValue
	Categories []FacetValue
	Nutriscore []FacetValue
}

// Searcher is the storage port for the search API.
type Searcher interface {
	// Search returns the page of products matching q.
	Search(ctx context.Context, q Query) (Result, error)
}
