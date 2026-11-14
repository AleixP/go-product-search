---
name: repo-add-filter
description: Use when adding a filter parameter to GET /search together with its disjunctive facet, or when changing an existing filter or facet (brands, categories, nutriscore) in this repo. End-to-end recipe from the mapping to the e2e stub, with the invariants to keep.
---

# Add or change a search filter and its facet

This skill is not preloaded in any agent. Read it on demand with Read when, and only when, a task adds or changes a filter or a facet. Everything cited here exists in the repo; if you change one of those places, refresh this file in the same task.

Today: `brands` and `categories` are free-text filters matched on a normalized keyword subfield; `nutriscore` is a fixed-set filter (a-e) on a plain keyword. Pick the closest one as the model: brands/categories for free text, nutriscore for a closed set.

## Rule of the design

- `q` goes in the `query`; every filter goes in `post_filter`; every facet is a `filter` aggregation holding the OTHER fields' clauses (disjunctive facets). A facet ignores only its own filter.
- Facet value = the folded key stored in the index. Sending that value back as a filter must match the same bucket (round trip).
- Filter values are never logged, never echoed in an error, never part of an error string.
- Exact match is case-sensitive on the key; free-text input is canonicalised by the engine first (step 6), not in Go.

## 1. Mapping (`internal/opensearch/mapping.json`)

- Add the keyword field. If values need case or accent folding, add a `norm` subfield: `{"type":"keyword","normalizer":"folded_keyword","ignore_above":256}` (see `brands` and `categories`; the normalizer `folded_keyword` is defined under `settings.analysis.normalizer`, filters `lowercase`, `asciifolding`). `dynamic` is `strict`, so an unmapped field is rejected at ingest.
- Tests in `internal/opensearch/mapping_test.go`: update the narrow ones (`TestMapping_Normalizer`, `TestMapping_Fields`, `TestMapping_TopLevelKeys`, `TestMapping_Analysis`, `TestMapping_IndexSettings`) AND the whole-document pin `TestMapping_GoldenDocument`. The golden pin exists because narrow tests missed edits to the unchanged parts; never skip it.
- `CreateIndex` sends mapping.json verbatim: a top-level `aliases` key would attach a new index to the live alias. Do not add one.
- A mapping change forces a re-ingest (`make ingest`): the live index built before the change lacks the field. Without it the new facet is empty and a filter on a normalized field gets 503 (the normalizer is missing).
- `ignore_above` counts UTF-16 code units of the raw value; validation counts runes. Values past the limit are absent from the subfield (known and accepted).

## 2. Catalog (`internal/catalog/search.go`)

- Add the field to `Query` (documented: validated by the HTTP layer; trimmed, non-empty, deduplicated, sorted; nil when absent). Add the list to `Facets` (`FacetValue{Value, Count}`); `Result.Facets` carries them.
- `Query` holds slices, so it is NOT comparable: tests compare with `reflect.DeepEqual`, never `==`.
- Fixed-set filters keep their allowed values as a constant, like `NutriscoreGrades`.

## 3. HTTP parameters (`internal/httpapi/params.go`)

- Allowlist: the `switch` in `checkKeys` (`case "brands", "categories", "nutriscore"`). Add the key there, to `allowedSuffix` (the generic and the named unknown-parameter texts both end with it), and to the `parseSearchParams` sequence.
- Unknown key: named in `parameter "<key>" is not allowed...` only when it matches `safeKey` (`[A-Za-z0-9_.\-\[\]]{1,32}`); any other key gets `unknown parameter` plus `allowedSuffix`. The first unknown key in sorted order wins. Never widen `safeKey`.
- Limits: `maxFilterValues` (20, counts every occurrence including empty ones, checked in `checkKeys`) and `maxFilterRunes` (256). Check order: malformed query string, keys, q, page, size, window, then brands, categories, nutriscore.
- Value handling: `parseFilter` (via `parseText`: UTF-8, trim, no control characters, rune limit), skip empties, then `sortedUnique`: sorted, deduplicated exactly, `nil` when empty. Fixed sets use `parseNutriscore`. Messages name the parameter, never the value.
- Tests: `internal/httpapi/params_test.go` (`TestParseSearchParams`, and `TestParseSearchParams_ErrorsNeverEchoInput`, the never-echo test: add your field and a value with markup/quotes to it) and `internal/httpapi/params_filters_test.go` (`TestParseSearchParams_Filters`, `TestParseSearchParams_UnknownKeyNamed`, `TestParseSearchParams_UnknownKeyFirstInSortedOrder`, `TestParseSearchParams_SlicesAreFresh`, `TestParseSearchParams_NeverEmptyValues`). Error messages are asserted as exact strings (they are public contract).

## 4. Query DSL (`internal/opensearch/search/body.go`)

- Constants: field names `fieldBrands`, `fieldCategories`, `fieldNutriscore`, aggregation names `aggBrands`, ..., and `facetSize`. Add yours, add it to `filterFields` (this order is the `post_filter` clause order), to the `aggs` struct (JSON field order is part of the golden files) and attach its facet in `facetAggs`, which builds the `aggs` value `Body` writes.
- `post_filter`: `{"bool":{"filter":[{"terms":{field:[values]}}...]}}`, present only when some filter is set (`postFilter`). Disjunctive rule: `facetFilter(q, exclude)` returns the clauses of the other fields, or `match_all` when there are none (`clauses`).
- Free-text facet (`termsFacetFor`): a `top` terms aggregation (`size` `facetSize`, `min_doc_count` 1, `order` `[{"_count":"desc"},{"_key":"asc"}]`) plus, when the field has values, a `selected` `filters` aggregation with one `term` per value in query order (so selected values appear with count 0). Fixed-set facet (`gradesFacet`): a `filters` aggregation, one `term` per allowed value.
- Do NOT use `include` on a terms aggregation: the engine does not normalize it (measured), so it would not match folded keys.
- `Body` trusts `q`: values arrive canonical (step 6).
- Golden files: `internal/opensearch/search/testdata/*.json`, driven by `TestBody_Golden` in `body_test.go` (add a row per new case, for example `filter_all.json`). Write the expected file by hand first, see the test fail, then `go test ./internal/opensearch/search -update` and review the diff by hand, line by line: the update flag blesses whatever the code does.
- `TestBody_T4BodiesUnchangedExceptAggs` pins that the pre-filter bodies differ from the T-4 ones only by `aggs`; `TestBody_TopLevelKeyOrder`, `TestBody_FilterStructure` (post_filter absent iff no filters, each facet holds exactly the other fields, `selected` iff values, no `include`) and `TestBody_SpecialCharactersRoundTrip` (values with `"`, `\`, `<`, `&`) in `body_filters_test.go`: extend them, do not weaken them.

## 5. Facet decoding (`internal/opensearch/search/facets.go`)

- `ParseFacets(aggs, q)` reads exactly what `Body` wrote. `termsFacetValues`: keep at most `facetSize` top buckets (a larger response cannot grow the output), append the selected values missing from them (exact, case-sensitive key match), sort count descending then key ascending, bytewise (`slices.SortStableFunc`). `gradeValues`: always the five grades in order, zeros included.
- Add the field to the per-field `aggsResponse` struct (next to `Brands`, `Categories`, `Nutriscore`): without its bucket `ParseFacets` fails with `missing aggregation <name>` and the handler answers 503.
- Validate the response: `count` rejects a missing, negative or over `math.MaxInt32` doc_count; bucket counts and selected counts must match the request. Errors are fixed strings (`errMissingAggs`, `errDecodeAggs`, `missing aggregation <name>`); nothing from the response text is echoed (`TestParseFacets_ErrorsNeverLeakResponseText`).
- Tests in `facets_test.go`: `TestParseFacets_Brands`, `TestParseFacets_MaxEntries`, `TestParseFacets_TopBucketsAreCappedAtFacetSize`, `TestParseFacets_SelectedValuesMergeByExactKey`, `TestParseFacets_ListsNeverNil`, `TestParseFacets_Errors`, and `TestParseFacets_ReadsWhatBodyWrites` (the contract between step 4 and 5: keep it green when you touch either side).

## 6. Adapter (`internal/opensearch/search.go`, `internal/opensearch/normalize.go`)

- `Client.Search` copies `q`, canonicalises the free-text filters with `normalize`, builds the body, searches, then runs `ParseFacets` last (an error in an earlier step wins; `TestSearch_ErrorOrderKeepsAggregationsLast`).
- `normalize` makes ONE `_analyze` call per request (normalizer `normalizerName` = `folded_keyword`, brands values then categories values in one `text` array, tokens split back by count) and returns each field deduplicated and sorted bytewise (`canonical`). No call when there is nothing to normalize. A token count mismatch or an empty token is an error. A new free-text filter joins this same call: extend the split, do not add a second call. A keyword filter WITHOUT a normalizer (exact match, like `nutriscore`) stays out of it: `Client.Search` passes only brands and categories to `normalize`, so such values skip `_analyze` and reach `Body` raw, already validated and sorted by the HTTP layer (`sortedUnique`). Do not add them to the `text` array: the token split is by count per field.
- `analyze` waits for a slot of `analyzeSlots` (size `maxAnalyzeInFlight` = 8, held in `analyzeBound`): the engine's analyze pool has `queue_size` 16 and answered 429 beyond it under 50 clients. The wait honours the request context. Slots are released on every path (`TestSearch_EverySlotIsReleasedOnEveryPath`).
- Errors: `searchError` and `normalizeError` reduce an engine `StructError` to `status N, type T`: its reason can quote a filter value. No request URL (`stripURL`), no body. Tests: `TestNormalize_ErrorsNameTheCallAndNeverCarryValues`, `TestSearch_SearchBadRequestKeepsOnlyStatusAndType`, `TestSearch_AnalyzeBadRequestNamesTheTypeWithoutValues`.
- Any adapter error reaches the client as the fixed 503 `search_unavailable` (`searchUnavailable` in `internal/httpapi/errors.go`), after the handler logs the error. Other tests: `normalize_test.go`, `normalize_limit_test.go`, `search_filters_test.go`.

## 7. Handler response (`internal/httpapi/handler.go`)

- Shape: `facets: {brands:[{value,count}], categories:[...], nutriscore:[...]}` after `items` (`facetsBody`, `facetEntry`, `facetsOf`, `facetList`). Lists are never `null` (`facetList` allocates). No `selected` flag; values are not HTML-escaped (`encodeJSON`).
- The `h.log.Error("search failed", ...)` call logs the error only; never log `q` or filter values. Tests: `handler_facets_test.go` (`TestSearch_FacetsExactBodyAndFieldOrder`, `TestSearch_NilFacetListsAreEmptyArrays`, `TestSearch_SearcherReceivesFilters`, `TestSearch_SearcherErrorWithFiltersIsFixed503AndLogsNoFilterValue`, `TestSearch_NutriscoreValueNeverLogged`; add a never-logged test for your field with a marker value).

## 8. End to end (`tests/e2e/api_test.go`, `tests/e2e/api_filters_test.go`)

- The stub `stubSearch` serves the real binary: `_search` (aggregations built per request by `searchBody`, which reads the number of `selected` filters of each facet from the request so `ParseFacets` gets the bucket counts it requires), `_analyze` (`stubSearch.analyze`, one token per text, `strings.ToLower` only: deterministic, not the engine's folding) and the node-stats / cluster-health polls the client sends in the background (the `_nodes` branch; any other request fails the test).
- Switches: `noAggs` (answer without aggregations: must be 503 and the API must recover), `analyzeFail` (400 quoting the request text: must be 503 with no value in body or stderr), `missing` (index not found). Add a switch only for a new failure mode.
- Subfunction `filterSubtests` (called from `TestAPI_EndToEnd`) asserts: values normalized once and reaching the search as `post_filter`; one `_analyze` call for both fields in order; no call without brand/category filters; invalid filters give 400 and reach neither `_analyze` nor `_search`; the failure subtests; HEAD. For a new terms facet the stub needs its selected counts: `selectedAggs` (the request shape it reads), `termsAgg` (answers one facet) and `selectedCount` (the doc_count of the i-th selected bucket) in `api_filters_test.go`; add a `termsAgg(req, "yourfield")` call in `searchBody`, or `ParseFacets` fails with `missing aggregation` and the test sees 503. `brandMarker` is a value that must appear nowhere in a response or log. `TestStubSearch_AnswersClientBackgroundPolls` keeps the API alive across a poll interval. Wait with a bound, never a bare sleep.

## 9. Checks on the real cluster

No cluster is needed for the gate. Before approving a mapping or DSL change run these (the default cluster is `make up`):
- Re-ingest after a mapping change (`make ingest`, flags with `ARGS=`); check the live index mapping equals the `mappings` of mapping.json (`jq -S` both sides).
- Probe on a scratch index (for example `t5-probe`), PUT with mapping.json, and DELETE it at the end (a GET must return 404). Never touch the `products` alias or its index for a probe, except in the declared re-ingest steps.
- Brute force: the scripts of the T-5 QA run (brute force and probes) were scratch and are NOT in the repo; redo them from this recipe:
  1. Dump `_source` of every document with scroll.
  2. Build the key map: `_analyze` (`{"normalizer":"folded_keyword","text":[...]}`) on the distinct raw values only, in batches.
  3. Pick seeded random filter combinations (take values from real documents, mix cases).
  4. Count in memory: AND across fields, OR within one; each facet counts under the OTHER fields' filters (disjunctive); top 10 by count then key; selected values added with count 0.
  5. Compare the API's total and every facet list. The engine defines the rule; only counting is re-implemented. Also check `_analyze(key) == key` for every key (round trip) and that `hits.total` counts the post_filtered hits.
- Load: 50 concurrent clients at `size=100`, half the requests with filters, 60 s: 0 non-200 and API peak RSS <= 64 MB (`/usr/bin/time -l`). Build the binaries outside the repo.

## 10. Test discipline

- TDD: write the failing test first and report red then green.
- Mutation sweep: write a script in your scratchpad, one line per mutant (name, exact old text, exact new text), applied to a COPY of the tree (never the repo), compile check first, `go test` per mutant. Every survivor is either killed by a new test or listed as an equivalent with the reason. Cover at least: clause order, the excluded field of a facet, `min_doc_count`, the order array, `facetSize`, limits (20, 256), `safeKey`, the check order, sort/dedupe, nil-versus-empty, error text.
- Bounded waits: no `time.Sleep` to wait for something; poll with a deadline like `waitFor` in `normalize_limit_test.go` (5 s), or use channels and contexts. Heavy tests skip under `testing.Short()`.

## 11. Gate and invariants

```bash
gofmt -l . && go vet ./... && go build -o /dev/null ./... && go test -race -short ./...
golangci-lint run ./...        # make lint, needs the pinned v2.15.0
go test ./internal/opensearch/search -update   # only after writing the golden files by hand; review the diff
go test ./...                  # full run once, before the final approval
```

Checklist before returning:
- A facet ignores only its own filter; `post_filter` absent when no filters.
- Facet value is the folded key and filtering by it returns that bucket's count; selected values are always listed (count 0 when empty); nutriscore always has 5 entries.
- Filter values appear in no log line, error string, or 400 message; the only echoed input is a safe unknown key name.
- Exact match is case-sensitive; normalization happens only through the engine's `_analyze`.
- Facet keys that cannot round-trip (over 256 runes after folding, or with control characters) are kept in the facet, and filtering by one gives 400 (Decision 7, round two). Do not drop or truncate them.
- Spellings that fold to one key collide on purpose (for example brand spellings "Meijer" and "meijer" under the folded key "meijer"): the facet value is the folded key, never one of the raw spellings (Decision 2).
- User decisions of T-5 to keep: plural names `brands`, `categories` (`nutriscore` singular), repeated keys for multiple values (not comma lists), 20 values and 256 runes limits, unknown parameter named in a 400 `invalid_parameter`, facet size 10, no missing bucket, filter on an unknown value is 200 with total 0, a response without aggregations is 503.
- Public changes (new parameter, message, response field) are contract: update the golden files, the e2e expectations and this file together.
