package e2e

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// wantCremeFraiche is the exact answer for 9991000000013, written by hand
// from testdata/integration.tsv: name with accents kept, brand and categories
// as stored (categories without the en: prefix), grade lower case.
const wantCremeFraiche = `{"id":"9991000000013","name":"Crème Fraîche","brands":["Acme Foods"],"categories":["dairies","cheeses"],"nutriscore":"b"}` + "\n"

// productIntegrationSubtests checks /healthz and /products/{id} on the real
// engine after the ingest. It asserts outcomes only: /healthz answers 200 once
// the alias exists, indexed documents are served through the alias, and missing
// documents, bad ids and query strings get the exact 404 and 400 bodies.
// The engine's own statuses (HEAD /_alias/products, a missing document) were
// observed by hand on OpenSearch 2.19.7 and are not asserted here.
func productIntegrationSubtests(t *testing.T, client *http.Client, base string) {
	t.Run("healthz is 200 ok once the alias exists", func(t *testing.T) {
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/healthz"), http.StatusOK, wantHealthOK)
	})

	t.Run("one product is the exact bare object", func(t *testing.T) {
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/9991000000013"), http.StatusOK, wantCremeFraiche)
	})

	t.Run("every indexed product matches its fixture row and its search item", func(t *testing.T) {
		items := map[string]searchItem{}
		for _, it := range getSearch(t, client, base, "size=100").Items {
			items[it.ID] = it
		}
		for _, d := range integrationDocs {
			got := do(t, client, http.MethodGet, base+"/products/"+d.id)
			if got.status != http.StatusOK {
				t.Errorf("GET /products/%s status = %d, want 200\nbody: %s", d.id, got.status, got.body)
				continue
			}
			var p searchItem
			if err := json.Unmarshal([]byte(got.body), &p); err != nil {
				t.Errorf("GET /products/%s: bad JSON: %v\nbody: %s", d.id, err, got.body)
				continue
			}
			want := searchItem{ID: d.id, Name: d.name, Brands: d.brands, Categories: d.categories, Nutriscore: d.grade}
			if !reflect.DeepEqual(p, want) {
				t.Errorf("GET /products/%s = %+v, want %+v", d.id, p, want)
			}
			if !reflect.DeepEqual(p, items[d.id]) {
				t.Errorf("GET /products/%s = %+v, differs from its /search item %+v", d.id, p, items[d.id])
			}
		}
	})

	t.Run("dropped and unknown ids are the fixed 404", func(t *testing.T) {
		for _, id := range append([]string{"9991000000999"}, integrationDropped...) {
			wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/"+id), http.StatusNotFound, wantNotFound)
		}
	})

	t.Run("an id over 128 bytes is the fixed 404", func(t *testing.T) {
		wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/"+strings.Repeat("9", 129)), http.StatusNotFound, wantNotFound)
	})

	t.Run("an escaped slash or question mark stays part of the id", func(t *testing.T) {
		// Unescaped, both would fetch document 9991000000001, which exists.
		for _, id := range []string{"9991000000001%2F", "9991000000001%3F"} {
			wantJSONReply(t, do(t, client, http.MethodGet, base+"/products/"+id), http.StatusNotFound, wantNotFound)
		}
	})

	t.Run("a query string is 400 on both endpoints", func(t *testing.T) {
		for _, p := range []string{"/products/9991000000013?x=1", "/healthz?x=1"} {
			wantJSONReply(t, do(t, client, http.MethodGet, base+p), http.StatusBadRequest, wantNoParams)
		}
	})
}
