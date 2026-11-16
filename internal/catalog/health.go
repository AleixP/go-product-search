package catalog

import "context"

// HealthChecker reports whether the catalog can serve reads.
type HealthChecker interface {
	// CheckHealth returns nil when the catalog can serve reads, that is, the
	// search engine answers and the product collection exists. Any other
	// state is an error.
	CheckHealth(ctx context.Context) error
}
