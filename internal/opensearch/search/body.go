// Package search builds the OpenSearch query DSL for catalog queries.
package search

import (
	"encoding/json"

	"github.com/AleixP/go-product-search/internal/catalog"
)

type request struct {
	From           int   `json:"from"`
	Size           int   `json:"size"`
	TrackTotalHits bool  `json:"track_total_hits"`
	Query          query `json:"query"`
}

// query holds exactly one of its clauses; the other stays nil and is omitted.
type query struct {
	MatchAll   *struct{}   `json:"match_all,omitempty"`
	MultiMatch *multiMatch `json:"multi_match,omitempty"`
}

type multiMatch struct {
	Query         string   `json:"query"`
	Type          string   `json:"type"`
	Fields        []string `json:"fields"`
	Operator      string   `json:"operator"`
	Fuzziness     string   `json:"fuzziness"`
	PrefixLength  int      `json:"prefix_length"`
	MaxExpansions int      `json:"max_expansions"`
}

// Body returns the JSON body of the _search request for q. It trusts q to be
// valid (see catalog.Query). An empty Text matches all products; otherwise a
// fuzzy multi_match runs over name, brands and categories.
func Body(q catalog.Query) ([]byte, error) {
	r := request{
		From:           (q.Page - 1) * q.Size,
		Size:           q.Size,
		TrackTotalHits: true,
	}
	if q.Text == "" {
		r.Query.MatchAll = &struct{}{}
	} else {
		r.Query.MultiMatch = &multiMatch{
			Query:         q.Text,
			Type:          "best_fields",
			Fields:        []string{"name^3", "brands.text^2", "categories.text"},
			Operator:      "or",
			Fuzziness:     "AUTO",
			PrefixLength:  1,
			MaxExpansions: 10,
		}
	}
	return json.Marshal(r)
}
