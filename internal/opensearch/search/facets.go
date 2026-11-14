package search

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/AleixP/go-product-search/internal/catalog"
)

// The shapes below mirror the aggregations Body writes: per facet a filter
// aggregation holding "top" (terms), "selected" and "grades" (anonymous
// filters, whose buckets come back as an array in filter order).
type aggsResponse struct {
	Brands     *facetResponse `json:"brands"`
	Categories *facetResponse `json:"categories"`
	Nutriscore *facetResponse `json:"nutriscore"`
}

type facetResponse struct {
	Top      *bucketList `json:"top"`
	Selected *bucketList `json:"selected"`
	Grades   *bucketList `json:"grades"`
}

type bucketList struct {
	Buckets []bucket `json:"buckets"`
}

// bucket is one bucket; Key is empty for the anonymous filters buckets.
// DocCount is a pointer so that a missing doc_count is detected.
type bucket struct {
	Key      string `json:"key"`
	DocCount *int64 `json:"doc_count"`
}

// Errors carry fixed text only: nothing from the response is ever echoed.
var (
	errMissingAggs = errors.New("missing aggregations")
	errDecodeAggs  = errors.New("decode aggregations")
)

// ParseFacets decodes the aggregations of a search response made with
// Body(q) into facets. q must hold the same canonical filter values that Body
// received. Brands and categories are the top buckets plus the selected values
// missing from them, ordered by count descending then value ascending;
// nutriscore always has the five grades in order.
//
// A doc_count that is missing, negative or above math.MaxInt32 is a decode
// error: no index holds that many products, so such a value is a corrupt
// response and must not reach clients or overflow an int.
func ParseFacets(aggs json.RawMessage, q catalog.Query) (catalog.Facets, error) {
	if len(bytes.TrimSpace(aggs)) == 0 || bytes.Equal(bytes.TrimSpace(aggs), []byte("null")) {
		return catalog.Facets{}, errMissingAggs
	}
	var r aggsResponse
	if err := json.Unmarshal(aggs, &r); err != nil {
		return catalog.Facets{}, errDecodeAggs
	}
	var f catalog.Facets
	var err error
	if f.Brands, err = termsFacetValues(aggBrands, r.Brands, q.Brands); err != nil {
		return catalog.Facets{}, err
	}
	if f.Categories, err = termsFacetValues(aggCategories, r.Categories, q.Categories); err != nil {
		return catalog.Facets{}, err
	}
	if f.Nutriscore, err = gradeValues(r.Nutriscore); err != nil {
		return catalog.Facets{}, err
	}
	return f, nil
}

// count validates a doc_count.
func count(b bucket) (int, error) {
	if b.DocCount == nil || *b.DocCount < 0 || *b.DocCount > math.MaxInt32 {
		return 0, errDecodeAggs
	}
	return int(*b.DocCount), nil
}

// termsFacetValues merges the top buckets with the selected values. A
// conforming engine returns at most facetSize top buckets, ordered by count
// descending then key ascending, so only the first facetSize are kept: a larger
// response cannot grow the output. Selected values are appended after the
// truncation, so the output holds at most facetSize + len(values) entries.
// Keys are compared exactly (case-sensitive).
func termsFacetValues(name string, f *facetResponse, values []string) ([]catalog.FacetValue, error) {
	if f == nil || f.Top == nil {
		return nil, fmt.Errorf("missing aggregation %s", name)
	}
	var selected []bucket
	if f.Selected != nil {
		selected = f.Selected.Buckets
	}
	if len(selected) != len(values) {
		return nil, fmt.Errorf("aggregation %s: %d selected buckets for %d values", name, len(selected), len(values))
	}
	top := f.Top.Buckets
	if len(top) > facetSize {
		top = top[:facetSize]
	}
	out := make([]catalog.FacetValue, 0, len(top)+len(values))
	seen := make(map[string]struct{}, cap(out))
	for _, b := range top {
		n, err := count(b)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[b.Key]; dup {
			continue
		}
		seen[b.Key] = struct{}{}
		out = append(out, catalog.FacetValue{Value: b.Key, Count: n})
	}
	for i, v := range values {
		n, err := count(selected[i])
		if err != nil {
			return nil, err
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, catalog.FacetValue{Value: v, Count: n})
	}
	slices.SortStableFunc(out, func(a, b catalog.FacetValue) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Value, b.Value))
	})
	return out, nil
}

func gradeValues(f *facetResponse) ([]catalog.FacetValue, error) {
	if f == nil || f.Grades == nil {
		return nil, fmt.Errorf("missing aggregation %s", aggNutriscore)
	}
	grades := f.Grades.Buckets
	if len(grades) != len(catalog.NutriscoreGrades) {
		return nil, fmt.Errorf("aggregation %s: %d grade buckets for %d grades", aggNutriscore, len(grades), len(catalog.NutriscoreGrades))
	}
	out := make([]catalog.FacetValue, len(grades))
	for i, b := range grades {
		n, err := count(b)
		if err != nil {
			return nil, err
		}
		out[i] = catalog.FacetValue{Value: catalog.NutriscoreGrades[i : i+1], Count: n}
	}
	return out, nil
}
