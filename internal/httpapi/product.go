package httpapi

import (
	"context"
	"errors"
	"net/http"
	"unicode"
	"unicode/utf8"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// maxProductIDBytes equals maxCodeBytes in cmd/ingest/record.go: no ingested
// product code is longer.
const maxProductIDBytes = 128

// product serves GET /products/{id}: the same bare object as a /search item.
// An invalid id answers the same 404 as a missing product, with no engine call.
func (h *handler) product(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		h.writeError(w, noParameters())
		return
	}
	id := r.PathValue("id")
	if !validProductID(id) {
		h.writeError(w, notFound())
		return
	}

	// shortcut: no cap on in-flight gets; a worst-case 198 KiB document holds
	// about 0.6 MiB per request (estimate); add a semaphore if the API is
	// exposed beyond local use.
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	p, err := h.getter.Get(ctx, id)
	if errors.Is(err, catalog.ErrNotFound) {
		h.writeError(w, notFound())
		return
	}
	if err != nil {
		// The error may carry engine text: it goes to the log only; the id is never logged.
		h.log.Error("get product failed", "error", err)
		h.writeError(w, searchUnavailable())
		return
	}
	h.writeJSON(w, http.StatusOK, itemOf(p))
}

// validProductID reports whether id is non-empty, at most maxProductIDBytes,
// valid UTF-8 and free of control characters (C0, DEL and C1, as in the ingest).
func validProductID(id string) bool {
	if id == "" || len(id) > maxProductIDBytes || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
