package catalog

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Getter.Get when no product has the given id.
var ErrNotFound = errors.New("product not found")

// Getter is the read port for one product.
type Getter interface {
	// Get returns the product whose id is id, or an error that errors.Is
	// ErrNotFound when there is none. The HTTP layer validates id (non-empty,
	// at most 128 bytes, valid UTF-8, no control characters); adapters still
	// treat it as untrusted path data.
	Get(ctx context.Context, id string) (Product, error)
}
