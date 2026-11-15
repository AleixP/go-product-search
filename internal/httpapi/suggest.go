package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"unicode/utf8"

	"github.com/AleixP/go-product-search/internal/catalog"
)

const (
	defaultSuggestSize = 10
	maxSuggestSize     = 20
	// minSuggestRunes is the shortest q that reaches the engine; the handler
	// answers shorter text with an empty list (not a parse error).
	minSuggestRunes = 2
)

type suggestResponse struct {
	Suggestions []suggestionItem `json:"suggestions"`
}

type suggestionItem struct {
	Text string `json:"text"`
}

// suggest serves GET /suggest. Text shorter than minSuggestRunes is answered
// with an empty list and never reaches the engine.
func (h *handler) suggest(w http.ResponseWriter, r *http.Request) {
	q, aerr := parseSuggestParams(r.URL.RawQuery)
	if aerr != nil {
		h.writeError(w, aerr)
		return
	}
	if utf8.RuneCountInString(q.Text) < minSuggestRunes {
		h.writeJSON(w, http.StatusOK, suggestResponse{Suggestions: []suggestionItem{}})
		return
	}

	// shortcut: no cap on in-flight suggests; measured at 33.1 MiB peak RSS (whole run, 50 concurrent clients, mixed suggest and search, real OpenSearch 2.19.7, 100k and 262k documents); add a semaphore if exposed beyond local use
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	res, err := h.suggester.Suggest(ctx, q)
	if err != nil {
		// The error may carry engine text: it goes to the log only; q is never logged.
		h.log.Error("suggest failed", "error", err)
		h.writeError(w, searchUnavailable())
		return
	}

	items := make([]suggestionItem, len(res))
	for i, s := range res {
		items[i] = suggestionItem{Text: s.Text}
	}
	h.writeJSON(w, http.StatusOK, suggestResponse{Suggestions: items})
}

// parseSuggestParams validates the raw query string of GET /suggest. The
// first error found wins: malformed query string, keys in sorted order (an
// unknown key, then a repeated q or size), q, then size. An unknown key is
// never named in the message.
func parseSuggestParams(rawQuery string) (catalog.SuggestQuery, *apiError) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return catalog.SuggestQuery{}, invalidParameter("malformed query string")
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		switch k {
		case "q", "size":
			if len(values[k]) > 1 {
				return catalog.SuggestQuery{}, invalidParameter("parameter " + k + " must appear at most once")
			}
		default:
			return catalog.SuggestQuery{}, invalidParameter("unknown parameter; allowed parameters are q and size")
		}
	}

	text, aerr := parseText("q", values.Get("q"), maxQueryRunes)
	if aerr != nil {
		return catalog.SuggestQuery{}, aerr
	}

	size, ok := parseInt(values.Get("size"), defaultSuggestSize)
	if !ok || size < 1 || size > maxSuggestSize {
		return catalog.SuggestQuery{}, invalidParameter("size must be an integer between 1 and 20")
	}
	return catalog.SuggestQuery{Text: text, Size: int(size)}, nil
}
