# go-product-search

[![CI](https://github.com/AleixP/go-product-search/actions/workflows/ci.yml/badge.svg)](https://github.com/AleixP/go-product-search/actions/workflows/ci.yml)

A product search service in Go on OpenSearch: full-text search, combinable
filters, disjunctive facets and autocomplete over a public product catalogue.

## What it does

- Full-text search over product name, brands and categories
- Filters that combine with AND across fields and OR within a field
- Disjunctive facets: each facet's counts ignore its own filter but apply the
  others, so users can widen a selection without it collapsing to zero
- Autocomplete of up to 20 distinct product names, on an edge n-gram field
- A streaming CSV ingest of Open Food Facts with a country filter, required
  fields, deterministic sampling and an atomic alias swap

## Run it

### Prerequisites

- Docker with Compose v2 (`make up` uses `docker compose up -d --wait`; the
  minimum version is not checked) and about 1 GiB of memory for the OpenSearch
  container (measured peak 1.06 GiB during the full 100,000-product ingest, 30
  `docker stats` samples about 2 s apart; heap 512 MB)
- Go 1.27.3, or any Go 1.21 or newer with `GOTOOLCHAIN=auto` (the default),
  which downloads the toolchain `go.mod` asks for (download size not measured)
- `make` and `curl`; `jq` is optional
- Tested on: macOS, arm64

### Quickstart

```sh
git clone https://github.com/AleixP/go-product-search
cd go-product-search
make up          # OpenSearch 2.19.7 in Docker, waits until healthy
make ingest      # downloads and indexes the default sample
make run         # API on 127.0.0.1:8080; run the next line in a second terminal
curl -s 'http://127.0.0.1:8080/search?q=chocolate&size=2'
```

The default `make ingest` streams the official export
(`https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz`,
1,536,486,783 bytes on 2026-11-16), keeps products sold in `en:united-states`
that have a brand, a category and a Nutri-Score, keeps one in 2 of them and stops
at 100,000 products, after reading about 66% of the file. It ends with a
`key=value` report on stdout. The keys to look at are `indexed=100000`,
`failed=0`, `doc_count=100000`, `index=products-<timestamp>` and
`published=true`; `coverage_pct` says how much of the file was read. A WARN line
on stderr, "stopped by --limit before reading the whole input", is expected.

### How long it takes

Measured on 2026-11-16 on the machine above, then projected to a 100 Mbit/s
link (12.5 MB/s). Downlink D = 19.9 MB/s, the mean of two `curl -L` downloads of
the export capped at 20 s (22.65 and 17.07 MB/s). The projection swaps the time
the ingest spends reading the file at D for the time at 12.5 MB/s.

| Step | Measured here | At 100 Mbit/s | How |
| --- | --- | --- | --- |
| `git clone` | not measured over the network | 0.12 s | transfer term only: 1.47 MiB packed repo at 12.5 MB/s; a local copy took 0.3 s; GitHub latency not measured |
| Image pull | not run | 63 s (arm64), 83 s (amd64) | config and layer bytes from `docker manifest inspect -v` (789,335,885 and 1,040,156,347) divided by 12.5 MB/s |
| `make up` until healthy | 6.95 s | 12 s | `docker run` with the same image, env and ulimits as `docker-compose.yml`, plus 5 s for the compose health-check interval |
| `make ingest`, cold Go caches | 59 s | 89 s | 58.95 s wall with empty build and module caches, minus 51.1 s of download at D, plus 81.2 s at 12.5 MB/s (about 1.01e9 bytes read; the figure varies by run); 105 s if the whole export is read (1,536,486,783 bytes) |
| `make run` until "listening" | 1.63 s | 1.63 s | measured |
| First `curl` | 0.16 s | 0.16 s | measured |
| Total | | about 166 s (arm64), 186 s (amd64) | 182 s and 202 s with the 105 s ingest |

The target is under 5 minutes. A slower toolchain download, an older Go or a
slower disk adds to this. The ingest process peaked at 27.0 MiB RSS (prebuilt
binary) and the container at 1.06 GiB (30 `docker stats` samples about 2 s
apart, full ingest); this differs from the 818-883 MiB of an earlier
integration-test run, which loads a small fixture.

### Configuration

Flags go through `ARGS`, for example
`make ingest ARGS="--limit 20000 --sample-every 4"`.

`cmd/ingest`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--input` | the official export URL | local path or https URL of the CSV (gzip is detected) |
| `--opensearch` | `http://localhost:9200` | OpenSearch address, http or https, no credentials |
| `--countries` | `en:united-states` | comma-separated country tags to keep; empty keeps all |
| `--require` | `brand,category,nutriscore` | fields a product must have; empty requires none |
| `--limit` | `100000` | maximum products to index; `0` means no limit |
| `--sample-every` | `2` | keep one eligible product in every N (N >= 1) |
| `--dry-run` | `false` | read and count the input without touching OpenSearch |

`cmd/api`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8080` | address to listen on |
| `--opensearch` | `http://localhost:9200` | OpenSearch address, http or https |

To work offline after one download: `make data` (about 1.5 GB, into the
git-ignored `data/`), then `make ingest ARGS="--input data/products.csv.gz"`.

### Local use only

The compose file turns the security plugin and TLS off and publishes
OpenSearch on `127.0.0.1:9200` only. The API has no authentication and listens
on `127.0.0.1:8080` by default; it logs a WARN if you bind it to all
interfaces. Do not expose either to a network.

### Tests

- `make test`: unit tests with the race detector (`-short`)
- `make test-integration`: starts OpenSearch with testcontainers, ingests
  `testdata/integration.tsv` and runs real queries; needs Docker; about 11.6-13.6 s
  with the image cached
- `make lint`: golangci-lint, which must be installed locally at v2.15.0

### Stop

`make down` stops OpenSearch and keeps the data volume.
`docker compose down -v` also deletes the volume.

## API

The service listens on `http://127.0.0.1:8080`. Every answer from `/search`,
`/suggest`, `/products/{id}` and `/healthz`, success or error, is JSON.

| Endpoint | What it returns |
| --- | --- |
| `GET /search?q=&brands=&categories=&nutriscore=&page=&size=` | A page of products, the exact total and facet counts |
| `GET /suggest?q=&size=` | Up to 20 distinct product names for the typed text |
| `GET /products/{id}` | One product, the same object as a `/search` item |
| `GET /healthz` | 200 `{"status":"ok"}` when OpenSearch answers and the `products` alias exists, else 503 `search_unavailable` |

`HEAD` behaves like `GET` on every endpoint. Other methods get a plain 405 and
unknown paths a plain 404 (both `text/plain`, not JSON).

### Parameters

| Parameter | Rule |
| --- | --- |
| `q` | trimmed, at most 100 characters, valid UTF-8, no control characters; empty or absent on `/search` matches every product |
| `page` | integer >= 1 (default 1) |
| `size` | `/search`: 1 to 100 (default 20); `/suggest`: 1 to 20 (default 10) |
| window | `page * size` must be at most 10,000, otherwise `page_out_of_range` |
| `brands`, `categories` | repeated keys (`brands=a&brands=b`), at most 20 values, each at most 256 runes (Unicode code points); matched after case and accent folding |
| `nutriscore` | repeated key, each value one of `a` to `e` (blank values ignored, surrounding spaces trimmed) |
| `id` | the `/products/{id}` path segment: at most 128 bytes, valid UTF-8, no control characters; an id that breaks a rule gets the same 404 `not_found` as a missing product |

A key that is not listed, a repeated `q`, `page` or `size`, and any value that
breaks a rule get a 400. On `/suggest` only `q` and `size` exist, and a `q` under
2 characters gives `{"suggestions":[]}`. `/products/{id}` and `/healthz` take no
query parameters: any query string gets 400 `invalid_parameter`.

### Responses

`/search` returns `total`, `page`, `size`, `items` and `facets`:

```json
{"total": 0, "page": 1, "size": 20,
 "items": [{"id": "", "name": "", "brands": [], "categories": [], "nutriscore": ""}],
 "facets": {"brands": [{"value": "", "count": 0}], "categories": [], "nutriscore": []}}
```

- Facet values are folded keys (lower case, no accents), not display names.
- `brands` and `categories` list the 10 most frequent values, ties broken by key,
  plus any selected value that is not among them. `nutriscore` always lists `a`
  to `e`.
- Each facet's counts apply every filter except its own field's, so selecting a
  brand does not remove the other brands from the brands facet.

`/suggest` returns `{"suggestions":[{"text":"..."}]}`.

`/products/{id}` returns the bare item object of `/search`; `/healthz` returns `{"status":"ok"}`.

Errors have one shape, `{"error":{"code":"...","message":"..."}}`. Messages are
fixed text. They name a parameter or a limit and never repeat a value; an unknown
`/search` key is named only if it is 1 to 32 characters from letters, digits and
`_.-[]`. The codes are `invalid_parameter` and `page_out_of_range` (400),
`not_found` (404, unknown or invalid product id), `search_unavailable` (503,
OpenSearch failed or timed out, or the `products` alias does not exist) and
`internal_error` (500).

### Examples

Captured on 2026-11-16 against a fresh default ingest of the official export
(`Last-Modified: Sun, 15 Nov 2026 10:13:37 GMT`). The export is rebuilt daily, so
totals and ranks change. The `| jq ...` part only shortens the output; drop it
for the full JSON.

A text search:

```sh
curl -s 'http://127.0.0.1:8080/search?q=chocolate&size=3' \
  | jq -c '{total, items: [.items[] | {name, brands, nutriscore}], top_brands: .facets.brands[:3], nutriscore: .facets.nutriscore}'
```

```json
{"total":6724,"items":[{"name":"Truffles","brands":["Chocolate Chocolate Chocolate"],"nutriscore":"e"},{"name":"Chocolate chocolate chocolate, milk chocolate nonpareils","brands":["Chocolate Chocolate Chocolate"],"nutriscore":"e"},{"name":"Chocolate chocolate chocolate, dark chocolate christmas nonpareils","brands":["Chocolate Chocolate Chocolate"],"nutriscore":"e"}],"top_brands":[{"value":"russell stover","count":152},{"value":"lindt","count":109},{"value":"ghirardelli chocolate company","count":89}],"nutriscore":[{"value":"a","count":73},{"value":"b","count":87},{"value":"c","count":353},{"value":"d","count":1203},{"value":"e","count":5008}]}
```

The same search filtered by brand and Nutri-Score. The brands facet ignores the
brand filter, so other brands are still listed (counted among Nutri-Score `d`),
with the selected `lindt` added at the end:

```sh
curl -s 'http://127.0.0.1:8080/search?q=chocolate&size=3&brands=Lindt&nutriscore=d' \
  | jq -c '{total, items: [.items[] | {name, brands, nutriscore}], top_brands: .facets.brands[:3], lindt: (.facets.brands[] | select(.value == "lindt")), nutriscore: .facets.nutriscore}'
```

```json
{"total":4,"items":[{"name":"100% Cocoa Unsweetened Chocolate","brands":["Lindt"],"nutriscore":"d"},{"name":"Reduced Sugar Dark Chocolate","brands":["Lindt"],"nutriscore":"d"},{"name":"Excellence 78% Cocoa Dark Chocolate","brands":["Lindt"],"nutriscore":"d"}],"top_brands":[{"value":"kind","count":36},{"value":"clif","count":31},{"value":"atkins","count":16}],"lindt":{"value":"lindt","count":4},"nutriscore":[{"value":"a","count":0},{"value":"b","count":0},{"value":"c","count":1},{"value":"d","count":4},{"value":"e","count":104}]}
```

Autocomplete:

```sh
curl -s 'http://127.0.0.1:8080/suggest?q=choc&size=5' | jq -c '.suggestions[].text'
```

```
"Chocolate chocolate chocolate, milk chocolate nonpareils"
"Chocolate chocolate chocolate, dark chocolate christmas nonpareils"
"Chocolate, Chocolate Vanilla Swirl Pudding, Chocolate, Chocolate Vanilla Swirl"
"Chocolate cremes sandwich cookies, chocolate, chocolate"
"Chocodate, Choco-Date With Almonds Assorted Chocolates"
```

A rejected request (`size` above 100), with the HTTP status on the last line:

```sh
curl -s -w '\n%{http_code}\n' 'http://127.0.0.1:8080/search?q=chocolate&size=500'
```

```
{"error":{"code":"invalid_parameter","message":"size must be an integer between 1 and 100"}}

400
```

One product by id, a missing id, and the health check (captured on 2026-11-16 on
the same index):

```sh
curl -s 'http://127.0.0.1:8080/products/0041250691600'
curl -s 'http://127.0.0.1:8080/products/0000000000000'
curl -s -w '%{http_code}\n' 'http://127.0.0.1:8080/healthz'
```

```
{"id":"0041250691600","name":"Chocolate","brands":["Meijer"],"categories":["desserts"],"nutriscore":"e"}
{"error":{"code":"not_found","message":"product not found"}}
{"status":"ok"}
200
```

The example data is Open Food Facts data, available under the ODbL; the attribution and links are in "Data and attribution".

## Design decisions

### Stack

- Go 1.27.3 (`go.mod`) with the standard `net/http` router (`http.NewServeMux`),
  `log/slog` for logs and `errgroup` (`golang.org/x/sync` v0.23.0) for the ingest
  worker pool
- OpenSearch 2.19.7 (`docker-compose.yml`) through `opensearch-go` v4.8.1, run
  with Docker Compose
- `testcontainers-go` v0.45.0 and its OpenSearch module for the integration test
- GitHub Actions (`.github/workflows/ci.yml`): `lint` (golangci-lint v2.15.0),
  `test` (`go vet`, `go test -race -short`) and `integration` (the OpenSearch
  integration test)

### Layout

```
cmd/api/                      HTTP server
cmd/ingest/                   loads the dataset into OpenSearch
internal/catalog/             types (Product, BulkResult, Query, Result, FacetValue, Facets, SuggestQuery, Suggestion) and ports (Searcher, Suggester, Getter, HealthChecker, Indexer)
internal/httpapi/             handlers, parameter validation, error shape
internal/opensearch/          adapter (opensearch-go) and mapping.json
internal/opensearch/search/   query builders, with golden files in testdata/
tests/e2e/                    end-to-end tests: stubbed OpenSearch, and the real-OpenSearch integration test
testdata/                     fixtures: products.tsv (Open Food Facts sample, ODbL) and integration.tsv (synthetic)
.github/workflows/            CI
.claude/                      agents and skills
```

### Decisions

Each one says what was chosen, what was left out and why.

1. Ports and adapters.
   - Chosen: `catalog.Searcher`, `catalog.Suggester`, `catalog.Getter`,
     `catalog.HealthChecker` and `catalog.Indexer` are
     small interfaces; the OpenSearch code is an adapter in `internal/opensearch`;
     the query builders live in `internal/opensearch/search` and are pinned by
     golden JSON files (regenerated with `-update`).
   - Left out: full DDD, with no aggregates or value objects where a struct does
     the job.
   - Why: the search engine is one adapter behind a port, and the handler tests
     run against fakes of the ports.
2. Disjunctive facets.
   - Chosen: the text query goes in `query`, the filters in `post_filter`, and each
     facet is a `filter` aggregation that applies the other fields' filters. A
     facet lists its top 10 values by count then key, plus any selected value
     (count 0 if nothing matches); `nutriscore` always lists `a` to `e`.
   - Left out: a `selected` flag in the response and a "missing" bucket.
   - Why: a facet that ignores its own filter lets users widen a selection
     without it collapsing to zero, and the hits and the facets come from one
     search request.
3. Folded keys for filters and facets.
   - Chosen: a `folded_keyword` normalizer (lower case plus accent folding) on
     `brands.norm` and `categories.norm`; facet values and filters use those
     keys. The adapter canonicalises the filter values of a request with one
     `_analyze` call, at most 8 in flight per process, because the engine's
     analyze queue holds 16.
   - Left out: folding the keys in Go.
   - Why: filter values go through the same normalizer that built the index
     keys. The bound exists because, with 50 concurrent mixed clients, 0.3-0.4%
     of requests failed with a `429` from the analyze queue before it.
4. Mapping and ranking.
   - Chosen: a strict `mapping.json` (unknown fields are rejected) with one shard,
     so facet counts are exact; `keyword` facet fields; `/search` is a
     `multi_match` of type `best_fields` over `name^3`, `brands.text^2` and
     `categories.text`, operator OR, fuzziness AUTO. `/suggest` uses an edge
     n-gram field `name.autocomplete` (2 to 15 characters) with its own search
     analyzer; every word of 2 or more characters must match as a prefix, and
     `name.norm` plus `collapse` makes the names distinct.
   - Left out on `/suggest`: fuzziness, filters, popularity ranking and
     highlighting.
   - Why: facet counts need one shard to be exact; the cost of the suggest design
     is under "Measured" (p95 14 ms at 50 clients) and its trade-offs under
     "Known limits".
5. Ingest.
   - Chosen: the CSV export (1.53 GB compressed) read as a stream, so memory does
     not grow with the input. A country filter instead of a language filter
     because the CSV has no language column. Sampling on a hash of the product
     `code`. A versioned index (`products-<timestamp>`) published by an atomic
     alias swap. A run is published when at most 1% of the products it passed
     failed. Lines over 1 MiB are skipped and counted.
   - Left out: JSONL (13.4 GB compressed, it has a language field) and Parquet (an
     extra dependency); sampling every k-th line; refusing to publish after any
     failed item; a 16 MiB line limit.
   - Why: the CSV is 1.53 GB against 13.4 GB; with a hash the sample stays the
     same when the export gains a row, where every k-th line would shift the whole
     sample; the longest real row is 17,944 bytes, and on an adversarial mix of
     lines a 16 MiB limit peaked at 81.5 MB, above the 64 MB budget, against
     51.0 MB at 1 MiB.
6. HTTP.
   - Chosen: standard `net/http` routing; fixed error messages that never repeat a
     value; a 3 s timeout per engine call and server read, write and idle timeouts;
     the OpenSearch client has its own transport with 64 idle connections per
     host. There is no cap on in-flight requests, a documented shortcut in the
     handlers.
   - Left out: an in-flight cap, rate limiting and authentication.
   - Why: with the default transport (2 idle connections per host) 4-6% of
     `/search` requests at 50 concurrent clients got a 503; with 64, none of
     34,098 did.

Also left out of v1: a frontend, vector or hybrid search, and relevance
evaluation.

### Known limits

- No cap on in-flight requests.
- `/healthz` checks that the `products` alias exists, not that its index answers
  searches.
- An id of `.` or `..` (sent as `%2E`/`%2E%2E`) always gets 404; a literal `/products/..` is redirected by the server.
- At most 8 `_analyze` calls in flight per process (the engine's analyze queue
  holds 16).
- Filter values are at most 256 runes (Unicode code points); the index's `ignore_above` counts
  UTF-16 units, not characters.
- `/suggest` ignores words of 1 character, and a word over 15 characters matches
  on its first 15.
- A suggestion is distinct after case and accent folding; names that differ only
  in whitespace stay separate (14 keys, 0.275% of documents).
- 7 documents (0.007%) cannot be suggested: 1-character names and names made of
  single CJK ideographs.
- After a mapping change a re-ingest is required; an older index answers 503 on
  `/suggest`.

## Measured

One machine, single-node OpenSearch 2.19.7. Every row ran against that real cluster except the dry-run, which uses a fake OpenSearch. "100k"
is the default index of 100,000 products; "262k" is every product that passes the
default filters (`--limit 0 --sample-every 1`); the exact count differs slightly
between runs and exports (262,373 to 262,512 below).

| What | Result |
| --- | --- |
| Full-export dry-run (T-3) | 4,908,651 rows, 262,373 pass the default filters at `--sample-every 1`; about 130 s; peak RSS about 23 MB (fake OpenSearch) |
| Default ingest, 100k (T-6) | 88 s, index 22.5 MB, peak RSS 27.6 MiB |
| Ingest, 262k (T-6) | 262,512 documents in 132 s, peak RSS 28.0 MiB |
| `/search` (T-5), 50 clients, `size=100`, filters and facets, 60 s | 0 non-200; p95 91 ms (100k) and 221 ms (262k); API peak RSS 32.1 and 30.2 MiB; at 200 clients p95 335 ms |
| `/suggest` (T-6), 50 clients, 60 s | 0 non-200; p95 14 ms; API peak RSS 31 MiB alone, 33.1 MiB mixed with `/search`; 1 client: p50 1.7 ms, p95 2.8 ms |
| API startup (T-4) | 0.6 s |
| `make test-integration` (T-7), image cached | 11.6-13.6 s wall (3 runs); container peak 818-883 MiB (4 samples) |

The ingest time is dominated by reading the export, so it depends on the link: the
quickstart table above measured 59 s on a 19.9 MB/s link.

## How this repo is built

This repo is built through an agent workflow defined in [`CLAUDE.md`](CLAUDE.md) and
[`.claude/agents/`](.claude/agents/). The main session is `coordinator`; three subagents work:

| Agent | Model | Role |
| --- | --- | --- |
| `orchestrator` | `claude-opus-5-5` | read-only; researches a ticket and writes a proposal split into small items |
| `go-developer` | `claude-sonnet-5-5` | implements one item at a time, test first |
| `qa-validator` | `claude-haiku-5-5` | reviews the item, validates tests, use cases and security, runs the end-to-end test |

The loop: a task goes to `orchestrator`, I accept or change its proposal, then each item goes to
`go-developer` and then `qa-validator`. A rejection sends the error back to `go-developer`. When
the last item is done, `qa-validator` runs the end-to-end test and I give the final approval.
Nothing is committed unless I ask. The agents use 17 skills under `.claude/skills/agent-kit-*`:
12 skills and the reference file `definition-of-done.md` from addyosmani/agent-skills, 1 skill
(`agent-kit-ponytail`) from DietrichGebert/ponytail (both MIT), and 4 written for the kit (origin
and notices in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)), plus 2 repo skills
(`repo-add-filter`, `repo-add-integration-test`). Each ticket keeps a private proposal with the
decisions and a rejection log, which this section summarises. Those proposals and logs are not
published, so the counts below are my own tally and can't be checked from this repo. QA ran
mutation sweeps on the tests and measurements on a real OpenSearch. The agents wrote all of the
code, tests, configuration, CI, this README and the `LICENSE`, plus the proposals and the
measurements.

### What I did

I made no hand edits to the code. I reviewed the proposals and decided their open questions. For
example, in the logs of T-2 to T-7:

- T-3: lowered the line limit from 16 MiB to 1 MiB after QA measured 81.5 MB peak memory against
  a 64 MB budget, applied at normal load; allowed one more round on a repeatedly rejected item
  with a closure rule; chose how an ambiguous publish is handled.
- T-4: allowed one more round on tests that hang; gave the OpenSearch client its own transport
  after 4-6% of searches returned 503 under 50 clients.
- T-5: chose one analyze call per request, at most 8 in flight. T-7: reduced the QA scope of
  one item and dropped a timing range that could not be traced.

### Where they got it wrong

`REJECTED` log lines, T-2 to T-7 (T-1 has no log; T-8 onwards are not counted): T-2 2, T-3 19, T-4 7,
T-5 7, T-6 4, T-7 9, in total 48; end-to-end rejections 0. A line can cover several rejections, and some T-4
approvals were not logged. Every `REJECTED` line is a `qa-validator` verdict (`CLAUDE.md`, work
loop step 4), and a measurement with the real adapter caught what a fake-based probe
under-counted (T-3). The kinds, with one example each:

- Untested branches: 16 mutants of the index adapter survived (T-3).
- Memory over budget (a 17 MiB line peaked at 85.4 MB against 64 MB) and a sampler that kept 60
  of 187,984 valid rows on real data (T-3).
- A cleanup path that could delete the live index after an ambiguous alias swap, and error
  messages that leaked the server address path (T-3).
- Tests that hang instead of failing on a shutdown regression (T-4).
- 503s under load from the default HTTP transport (T-4) and 429s from the analyze queue (T-5).
- False statements in a skill, untraceable figures in the notes (T-5, T-7), and wrong
  coordinator instructions such as asking for "6 runs" of figures that had 3 (T-7).
- A QA subagent touching the running local cluster against the rule; it only read (T-7).

Everything in this repo was written for it, except
`testdata/products.tsv`, which is ODbL as a whole file (12 Open Food Facts rows and 3
synthetic rows written for this repo), and the third-party agent-kit files listed in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
Nothing comes from any employer: no code, prompts, data or client material.

## Data and attribution

Product data comes from [Open Food Facts](https://world.openfoodfacts.org),
(c) Open Food Facts contributors, available under the
[Open Database Licence (ODbL)](https://opendatacommons.org/licenses/odbl/1-0/).
See the [terms of use](https://world.openfoodfacts.org/terms-of-use).

- The example outputs in this README contain Open Food Facts data and carry this
  attribution notice (ODbL 4.3: a notice associated with the Produced Work, reasonably
  calculated to make any person exposed to it aware that the data comes from Open Food
  Facts and is available under the ODbL). They are minimal excerpts of a few lines.
- v1 uses no other dataset and no product photos (photos are CC BY-SA).
- `testdata/products.tsv` holds 12 Open Food Facts rows (a derived database) and 3
  synthetic rows. The whole file carries the ODbL notice in
  [`testdata/LICENSE`](testdata/LICENSE), separate from the code licence.
- A Produced Work is a work resulting from using the whole or a Substantial part of the
  Contents via a search or other query (ODbL 1.0).
- The index built by `make ingest` is a derived database of Open Food Facts and is under
  the ODbL. Keep the notice if you share it (ODbL 4.2(b)-(c) apply when you Publicly
  Convey the database).
- This project is for local use and demonstration. Publishing the index or exposing the
  API publicly would require offering the index under the ODbL, with a copy of it (ODbL
  4.4(a), 4.4(c) and 4.6), and the notice under ODbL 4.3. This is not legal advice.

## Licence

The code is under the [MIT licence](LICENSE).

Not covered by it:

- The whole file `testdata/products.tsv`: ODbL 1.0, notice in
  [`testdata/LICENSE`](testdata/LICENSE), which applies to the whole file. 12 rows are
  Open Food Facts data and 3 are synthetic.
- The third-party agent-kit files (12 skills from addyosmani/agent-skills,
  `agent-kit-ponytail` and `.claude/references/definition-of-done.md`): their own MIT
  notices are in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
- The data the ingest downloads and the index it builds: ODbL. Keep the notice if you
  Publicly Convey them (ODbL 4.2).

`testdata/integration.tsv` is synthetic, written for this project, and is under MIT.
