package httpapi

import (
	"context"
	"net/http"
)

// healthBody is the 200 answer of GET /healthz.
type healthBody struct {
	Status string `json:"status"`
}

// healthz serves GET /healthz: 200 only when the catalog can serve reads, else
// 503 in the same envelope as /search.
func (h *handler) healthz(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		h.writeError(w, noParameters())
		return
	}

	// shortcut: no cap on in-flight health checks; each probe costs one
	// engine HEAD request; add a semaphore if the API is exposed beyond
	// local use.
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	if err := h.health.CheckHealth(ctx); err != nil {
		// The error may carry engine text: it goes to the log only.
		h.log.Error("health check failed", "error", err)
		h.writeError(w, searchUnavailable())
		return
	}
	h.writeJSON(w, http.StatusOK, healthBody{Status: "ok"})
}
