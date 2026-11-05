# go-product-search

A product search service in Go on OpenSearch: full-text search, combinable
filters, disjunctive facets and autocomplete over a public product catalogue.

> **Status: work in progress.** Nothing here runs yet.

## What it will do

- Full-text search over product name, brand and category
- Filters that combine with AND across fields and OR within a field
- Disjunctive facets: each facet's counts ignore its own filter but apply the
  others, so users can widen a selection without it collapsing to zero
- Autocomplete on an edge n-gram field
- A bulk ingest command that loads a subset of Open Food Facts

## API (planned)

| Endpoint | What it returns |
| --- | --- |
| `GET /search?q=&brand=&category=&nutriscore=&page=&size=` | Results + facets with counts |
| `GET /suggest?q=` | Autocomplete suggestions |
| `GET /products/{id}` | One product |
| `GET /healthz` | Liveness, plus OpenSearch reachability |

## Stack

- Go 1.27 (standard `net/http` routing, `log/slog`, `errgroup`)
- OpenSearch 2.x via `opensearch-go`, run with Docker Compose
- testcontainers-go for integration tests
- GitHub Actions: `go vet`, golangci-lint, tests

## Design

Ports and adapters, not full DDD. The search engine is an adapter behind a
port; there are no aggregates or value objects where a struct does the job.

```
cmd/api/              HTTP server
cmd/ingest/           loads the dataset into OpenSearch
internal/catalog/     types (Product, Query, Facet) + ports (Searcher, Indexer)
internal/search/      query building, facet logic
internal/opensearch/  adapter (opensearch-go)
internal/httpapi/     handlers
testdata/             small fixture sample for tests
.claude/              agents and skills
```

Design decisions and the alternatives left out will be written up here as
the tickets land.

## Run it

Coming with T-1 and T-3. The goal: on a clean machine, `docker compose up`,
`make ingest` and a `curl` to `/search` work in under five minutes.

## How this repo is built

The code is written through an orchestrator + sub-agents workflow that lives
in `.claude/agents/`: orchestrator read a ticket, implement it, write
and run the tests, and prepare the commit. I review every commit. This
section will say what the agents did, what I changed by hand and where they
got it wrong.

Everything in this repo is written from scratch. It contains no code,
prompts, data or client material from any employer.

## Data and attribution

Product data comes from [Open Food Facts](https://world.openfoodfacts.org),
(c) Open Food Facts contributors, available under the
[Open Database Licence (ODbL)](https://opendatacommons.org/licenses/odbl/1-0/).
See the [terms of use](https://world.openfoodfacts.org/terms-of-use).

The `testdata/` subset is a derived database and carries its own ODbL notice,
separate from the code licence. Product photos are CC BY-SA and are not used.

## Licence

To be decided before the repo goes public. The code licence will not cover
`testdata/`.
