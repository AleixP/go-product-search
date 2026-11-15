package search

import (
	"encoding/json"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// Index fields used by the suggest query and its collapse and sort keys.
const (
	fieldNameAutocomplete = "name.autocomplete"
	fieldNameNorm         = "name.norm"
)

type suggestRequest struct {
	Size           int                  `json:"size"`
	TrackTotalHits bool                 `json:"track_total_hits"`
	Source         []string             `json:"_source"`
	Query          suggestQuery         `json:"query"`
	Collapse       suggestCollapse      `json:"collapse"`
	Sort           [2]map[string]string `json:"sort"`
}

type suggestQuery struct {
	Match map[string]suggestMatch `json:"match"`
}

type suggestMatch struct {
	Query    string `json:"query"`
	Operator string `json:"operator"`
}

type suggestCollapse struct {
	Field string `json:"field"`
}

// SuggestBody returns the JSON body of the OpenSearch request for q. The
// text only ever reaches the DSL as the value of match.query, and every name
// is collapsed on its folded form so suggestions are distinct.
func SuggestBody(q catalog.SuggestQuery) ([]byte, error) {
	return json.Marshal(suggestRequest{
		Size:           q.Size,
		TrackTotalHits: false,
		Source:         []string{"name"},
		Query: suggestQuery{Match: map[string]suggestMatch{
			fieldNameAutocomplete: {Query: q.Text, Operator: "and"},
		}},
		Collapse: suggestCollapse{Field: fieldNameNorm},
		Sort: [2]map[string]string{
			{"_score": "desc"},
			{fieldNameNorm: "asc"},
		},
	})
}
