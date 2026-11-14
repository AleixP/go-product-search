// Package search builds the OpenSearch query DSL for catalog queries.
package search

import (
	"encoding/json"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// Index field names used by filters and facets, and the facet aggregation
// names. The ".norm" subfields are the normalized keywords of the mapping.
const (
	fieldBrands     = "brands.norm"
	fieldCategories = "categories.norm"
	fieldNutriscore = "nutriscore"

	aggBrands     = "brands"
	aggCategories = "categories"
	aggNutriscore = "nutriscore"

	// facetSize is the number of top buckets of the brands and categories facets.
	facetSize = 10
)

type request struct {
	From           int          `json:"from"`
	Size           int          `json:"size"`
	TrackTotalHits bool         `json:"track_total_hits"`
	Query          query        `json:"query"`
	PostFilter     *filterQuery `json:"post_filter,omitempty"`
	Aggs           aggs         `json:"aggs"`
}

// filterQuery holds exactly one of its clauses; the other stays nil and is omitted.
type filterQuery struct {
	MatchAll *struct{}   `json:"match_all,omitempty"`
	Bool     *boolFilter `json:"bool,omitempty"`
}

type boolFilter struct {
	Filter []termsClause `json:"filter"`
}

// termsClause is a single-entry terms query; the map key is the field name.
type termsClause struct {
	Terms map[string][]string `json:"terms"`
}

type aggs struct {
	Brands     termsFacet  `json:"brands"`
	Categories termsFacet  `json:"categories"`
	Nutriscore gradesFacet `json:"nutriscore"`
}

type termsFacet struct {
	Filter filterQuery  `json:"filter"`
	Aggs   termsSubAggs `json:"aggs"`
}

type termsSubAggs struct {
	Top      termsAgg    `json:"top"`
	Selected *filtersAgg `json:"selected,omitempty"`
}

type termsAgg struct {
	Terms termsSpec `json:"terms"`
}

type termsSpec struct {
	Field       string    `json:"field"`
	Size        int       `json:"size"`
	MinDocCount int       `json:"min_doc_count"`
	Order       []orderBy `json:"order"`
}

type orderBy struct {
	Count string `json:"_count,omitempty"`
	Key   string `json:"_key,omitempty"`
}

type gradesFacet struct {
	Filter filterQuery `json:"filter"`
	Aggs   struct {
		Grades filtersAgg `json:"grades"`
	} `json:"aggs"`
}

type filtersAgg struct {
	Filters struct {
		Filters []termFilter `json:"filters"`
	} `json:"filters"`
}

type termFilter struct {
	Term map[string]string `json:"term"`
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
	r.PostFilter = postFilter(q)
	r.Aggs = facetAggs(q)
	return json.Marshal(r)
}

// filterField is one filter field: its aggregation name, index field and
// selected values.
type filterField struct {
	agg, field string
	values     []string
}

func filterFields(q catalog.Query) []filterField {
	return []filterField{
		{aggBrands, fieldBrands, q.Brands},
		{aggCategories, fieldCategories, q.Categories},
		{aggNutriscore, fieldNutriscore, q.Nutriscore},
	}
}

// clauses returns one terms clause per non-empty filter field, skipping the
// field whose aggregation name is exclude.
func clauses(q catalog.Query, exclude string) []termsClause {
	var out []termsClause
	for _, f := range filterFields(q) {
		if f.agg != exclude && len(f.values) > 0 {
			out = append(out, termsClause{Terms: map[string][]string{f.field: f.values}})
		}
	}
	return out
}

// postFilter applies every filter to the hits; nil when there are none.
func postFilter(q catalog.Query) *filterQuery {
	c := clauses(q, "")
	if len(c) == 0 {
		return nil
	}
	return &filterQuery{Bool: &boolFilter{Filter: c}}
}

// facetFilter is the filter of one facet: the clauses of the OTHER fields, so
// the facet ignores its own selection (disjunctive facets).
func facetFilter(q catalog.Query, exclude string) filterQuery {
	c := clauses(q, exclude)
	if len(c) == 0 {
		return filterQuery{MatchAll: &struct{}{}}
	}
	return filterQuery{Bool: &boolFilter{Filter: c}}
}

func termsFacetFor(q catalog.Query, agg, field string, selected []string) termsFacet {
	f := termsFacet{Filter: facetFilter(q, agg)}
	f.Aggs.Top = termsAgg{Terms: termsSpec{
		Field:       field,
		Size:        facetSize,
		MinDocCount: 1,
		Order:       []orderBy{{Count: "desc"}, {Key: "asc"}},
	}}
	if len(selected) > 0 {
		f.Aggs.Selected = termFilters(field, selected)
	}
	return f
}

func termFilters(field string, values []string) *filtersAgg {
	var a filtersAgg
	a.Filters.Filters = make([]termFilter, len(values))
	for i, v := range values {
		a.Filters.Filters[i] = termFilter{Term: map[string]string{field: v}}
	}
	return &a
}

func facetAggs(q catalog.Query) aggs {
	grades := make([]string, len(catalog.NutriscoreGrades))
	for i := range grades {
		grades[i] = catalog.NutriscoreGrades[i : i+1]
	}
	n := gradesFacet{Filter: facetFilter(q, aggNutriscore)}
	n.Aggs.Grades = *termFilters(fieldNutriscore, grades)
	return aggs{
		Brands:     termsFacetFor(q, aggBrands, fieldBrands, q.Brands),
		Categories: termsFacetFor(q, aggCategories, fieldCategories, q.Categories),
		Nutriscore: n,
	}
}
