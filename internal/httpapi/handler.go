// Package httpapi serves the search HTTP API.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// searchTimeout bounds one search, including the OpenSearch round trip.
const searchTimeout = 3 * time.Second

// internalErrorBody is the 500 answer used when encoding a response fails.
// It is a constant so that the failure path cannot fail again.
var internalErrorBody = []byte(`{"error":{"code":"internal_error","message":"internal error"}}` + "\n")

// encodeJSON encodes v as JSON without HTML escaping; tests replace it to
// force a failure.
var encodeJSON = func(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type searchResponse struct {
	Total int           `json:"total"`
	Page  int           `json:"page"`
	Size  int           `json:"size"`
	Items []productItem `json:"items"`
}

type productItem struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Brands     []string `json:"brands"`
	Categories []string `json:"categories"`
	Nutriscore string   `json:"nutriscore"`
}

type handler struct {
	searcher catalog.Searcher
	log      *slog.Logger
}

// New returns the HTTP handler of the API: only GET /search (HEAD also
// matches). Other paths and methods get the mux's plain 404 and 405.
func New(s catalog.Searcher, log *slog.Logger) http.Handler {
	h := &handler{searcher: s, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search", h.search)
	return mux
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	q, aerr := parseSearchParams(r.URL.RawQuery)
	if aerr != nil {
		h.writeError(w, aerr)
		return
	}

	// shortcut: no cap on in-flight searches. Measured at 32.3 MiB peak RSS
	// (whole run, 50 concurrent clients, size=100, real OpenSearch 2.19.7,
	// 100k documents), but a worst-case index of 198 KiB documents could
	// need about 78 MiB per request. Add a semaphore if the API is exposed
	// beyond local use.
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	res, err := h.searcher.Search(ctx, q)
	if err != nil {
		// The error may carry engine text: it goes to the log only, and q is never logged.
		h.log.Error("search failed", "error", err)
		h.writeError(w, searchUnavailable())
		return
	}

	items := make([]productItem, len(res.Products))
	for i, p := range res.Products {
		items[i] = productItem{ID: p.ID, Name: p.Name, Brands: nonNil(p.Brands), Categories: nonNil(p.Categories), Nutriscore: p.Nutriscore}
	}
	h.writeJSON(w, http.StatusOK, searchResponse{Total: res.Total, Page: q.Page, Size: q.Size, Items: items})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (h *handler) writeError(w http.ResponseWriter, e *apiError) {
	h.writeJSON(w, e.status, errorBody{Error: errorDetail{Code: e.code, Message: e.message}})
}

func (h *handler) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := encodeJSON(v)
	if err != nil {
		h.log.Error("encode response failed", "error", err)
		status, body = internalError().status, internalErrorBody
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
