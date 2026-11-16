---
name: repo-add-integration-test
description: Use when adding or changing a case of the Docker-backed OpenSearch integration test (TestIntegration in tests/e2e), changing testdata/integration.tsv, or debugging that test or its CI job. Recipe from the fixture to the CI job, with the safety rules and the accepted limits.
---

# Add or change an integration test case

This skill is not preloaded in any agent. Read it on demand with Read when, and only when, a task touches `TestIntegration`, its fixture, its Make target or its CI job. Everything cited here exists in the repo; if you change one of those places, refresh this file in the same task.

## 1. When to use it

- A new text-search, filter, facet or suggest case on the real engine.
- A change to `testdata/integration.tsv` (rows, columns, brands, categories).
- A mapping or query change whose effect only the real engine shows (analyzers, normalizers, collapse, fuzziness).
- A failure of `TestIntegration` locally or in the `integration` CI job.

Do not use it for unit or stub tests: those are `tests/e2e/api_test.go` and `tests/e2e/ingest_test.go` (httptest stub, no Docker). For a new filter, `repo-add-filter` step 8 and this skill both apply.

## 2. Architecture

One run of `TestIntegration` (`tests/e2e/integration_test.go`), in this order:

1. Skip under `testing.Short()` (`shortSkipMessage`). `TestIntegration_SkipsUnderShort` pins that skip by running the test binary with `-test.short` and `INTEGRATION_HELPER=skip-check`.
2. `notifyInterrupt` (in `integration_proc_test.go`): the first SIGINT or SIGTERM cancels a context instead of killing the binary, so cleanups run. A second signal kills the process.
3. `startOpenSearch`: `dockerProblem` probe (bounded by `dockerProbeWait`), then `opensearch.Run` with image `integrationImage` (`opensearchproject/opensearch:2.19.7`, equal to docker-compose.yml, pinned by `TestIntegrationImage_MatchesDockerCompose`), env `OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m`, and ports 9200 and 9600 published on `127.0.0.1` with random host ports. It inspects the container and fails unless the env and the loopback bindings are as expected. `testcontainers.CleanupContainer` is called right after `Run`, before the error check; `checkContainerGone` runs after it and fails the test if the container still exists.
4. `freshClusterError` (the guard): refuses an invalid URL and port 9200 before any request; requires `GET /_alias/products` = 404 and `GET /_cat/indices/products-*?format=json` = `[]`. No redirects, body capped at 1 MiB, errors never quote a response body.
5. `buildAPI` and `buildIngest` build the real binaries into `t.TempDir()`. `startGuarded` starts `api` with `--addr 127.0.0.1:0 --opensearch <addr>`; `waitAddr` reads its listening line.
6. Subtest "before ingest search and suggest are 503": `/search?q=milk` and `/suggest?q=mi` give exactly `unavailableJSON` (the alias does not exist yet).
7. `ingestIntegration` runs the real `ingest` (through `runIngestGuarded`) with `--input <abs fixture> --opensearch <addr> --sample-every 1 --limit 0`, and checks the report against literals. If it fails, the rest is skipped (it returns false).
8. Sequential subtests on that one container, in this order: `integrationDataSubtests` (`integration_ingest_test.go`), `searchIntegrationSubtests` (`integration_search_test.go`), `facetsIntegrationSubtests` (`integration_facets_test.go`), `suggestIntegrationSubtests` (`integration_suggest_test.go`).

Both child processes run through `guardedScript`: the test process holds the write end of the child's stdin pipe, so when the test dies for any reason (including a `-test.timeout` panic or SIGKILL, where no `t.Cleanup` runs) the pipe closes and the wrapper sends SIGINT, then SIGKILL after `stopGrace` seconds. Do not start `api` or `ingest` with a plain `exec.Command` in this test.

Files (all in package `e2e`, `tests/e2e/`):

| File | Holds |
|---|---|
| `integration_test.go` | constants, `startOpenSearch`, `freshClusterError`, guard tests, `TestIntegration` |
| `integration_data_test.go` | `integrationFixture`, report literals, `integrationDocs`, `integrationDropped` |
| `integration_ingest_test.go` | `runIngestGuarded`, `ingestIntegration`, `getSearch`, `itemIDs`, data subtests |
| `integration_search_test.go` | `searchCases`, `wantFacets`, `docIDs`, `searchIntegrationSubtests` |
| `integration_facets_test.go` | `facetCases`, `roundTrip`, `facetsIntegrationSubtests` |
| `integration_suggest_test.go` | `suggestCases`, `suggestGroup`, `getSuggest`, `suggestIntegrationSubtests` |
| `integration_proc_test.go` | `guardedScript`, `startGuarded`, `notifyInterrupt`, `checkContainerGone`, `probeDocker` |
| `integration_probe_test.go`, `integration_ingest_unit_test.go`, `integration_proc_unit_test.go`, `integration_ingest_proc_test.go` | tests of the helpers above (no container); the last two run the test binary as a helper process |

`integration_test.go` is close to the 500-line limit: new cases go in their own file, never in it, except the one call line in `TestIntegration`.

## 3. How to add a case

Pick the file by kind: text search in `integration_search_test.go`, filters and facets in `integration_facets_test.go`, suggest in `integration_suggest_test.go`.

- Table row: a `searchCase{name, q, ids, why}`, a `facetCase{name, query, ids, brands, categories, grades}` (queries built with `vals(...)`, which sets `size=100`), or a `suggestCase{name, q, want}`.
- Ids are the last three digits of the fixture code (`9991000000NNN`); `docIDs` expands them.
- A new kind of check is a new function `func xIntegrationSubtests(t *testing.T, client *http.Client, base string)` in a new file, plus one call at the end of `TestIntegration`, after `suggestIntegrationSubtests`. Use `getSearch(t, client, base, query)` or `getSuggest(...)`, never raw `client.Get`.
- Expectations are literals derived by hand from `testdata/integration.tsv` and the engine rules (analyzers in `internal/opensearch/mapping.json`, query in `internal/opensearch/search/`). Never compute an expected value with the code under test, and never copy a value from a first run: derive it, write it, see it fail or pass for the right reason. Put the derivation in a comment or in `why`.
- Order policy: hits are compared as a set of ids plus the total; order is never asserted for hits or suggestions. Facet lists are compared exactly and in order: `wantFacets` compares the three lists (brands, categories, nutriscore), and the order (count descending, then key ascending, top 10, then selected values missing from it with their real count, for example `zenith bakery` with 1) is produced by `termsFacetValues` in `internal/opensearch/search/facets.go`. A selected value gets count 0 when it matches nothing under the other active constraints. Suggestions are sets of groups (`checkGroups`, `checkSubset`): a suggestion matches a group when its text is any of the raw spellings that fold to one key.
- Facet values are folded keys. A selected value outside the top 10 is listed after the top 10 with its count.
- Keep a case that needs a fuzzy typo or an accent honest: say in `why` whether fuzziness alone would also find the row (see the `Crèmé` case, which is the one that proves folding).
- No `time.Sleep`, no unbounded request: `client` already has the `clientBound` timeout.
- Subtests share one index (the alias `products` is hard-coded in `internal/opensearch/index.go` as `aliasName`). A subtest that changes the index (a second ingest, a delete) must restore it, or be the last one. None exists today.

## 4. Changing the fixture

`testdata/integration.tsv` is synthetic: 8 columns (`url`, `nutriscore_grade`, `brands`, `code`, `quantity`, `categories_tags`, `product_name`, `countries_tags`), 29 data rows, codes `9991000000001` to `9991000000025` (indexed) and `9991000000101` to `9991000000104` (each dropped by one ingest filter), LF line ends. The ingest reads columns by name and ignores the others.

If you add, remove or edit a row, change together, by hand:

- `integrationDocs` in `integration_data_test.go` (id, name, brands as stored, categories without `en:`, lower-case grade) and `integrationDropped`.
- The report literals: `wantRowsRead`, `wantPassed`, `wantIndexed`, `wantDocCount`, `wantCountry`, `wantBrand`, `wantCategory`, `wantNutriscore`. The drop precedence is country, brand, category, nutriscore.
- Every literal that depends on the rows: `searchCases`, `acmeCase` and `facetCases` (counts, order, the top-10 cut), `allBrands`, `allCategories`, `allGrades` and `roundTrip` in `integration_facets_test.go` (including the counts above the table), `suggestCases`, `suggestCh12` and the `g*` groups, and the pagination arithmetic (it uses `len(integrationDocs)`).
- Keep these properties, which cases rely on: three brand spellings that fold to one key (`Acme Foods`, `ACME FOODS`, `Acmé Foods`); pairwise different grade counts; at least 12 distinct categories with a tie at place 10 (`sauces` and `soups`); one brand outside the top 10 (`zenith bakery`); an accented name group (`Crème Fraîche`); a word longer than 15 letters; more than 10 names under the prefix `ch`.
- `testdata/LICENSE` says `testdata/integration.tsv` is synthetic and not Open Food Facts data. Keep it true, and keep the strings that `TestFixture_LicenseNoticeNamesTheSourceAndTheLicences` pins.
- `testdata/products.tsv` is pinned by nine Go test files (eight in `cmd/ingest`, plus `tests/e2e/ingest_test.go`). Never edit it for this test.

## 5. Timeouts and skip or fail policy

| What | Bound | Where |
|---|---|---|
| Container start (image pull, start, readiness) | 5 min (`containerStartWait`) | `startOpenSearch` |
| Docker health probe | 30 s (`dockerProbeWait`) | `probeDocker` |
| One guard request | 10 s (`guardWait`) | `freshClusterError` |
| Container stop at cleanup | 60 s (`teardownWait`) | `CleanupContainer` |
| Grace for a signalled container start | 30 s (`interruptGrace`) | `startOpenSearch` |
| Container gone check | 10 s (`goneWait`, poll `gonePoll`) | `checkContainerGone` |
| Real ingest | 1 min (`ingestBound`) | `runIngestGuarded` |
| `api` listening line | 10 s (`listenWait`) | `waitAddr` |
| One HTTP request | 10 s (`clientBound`) | the test client |
| Child stop after SIGINT | 5 s (`stopGrace`) before SIGKILL | `guardedScript` |
| Whole run | `-timeout 15m` | Make target and CI |

Policy: no Docker (or a daemon that does not answer in 30 s) skips the test locally with `integration test needs a running Docker daemon: ...` and fails it when the `CI` environment variable is non-empty. The library panics when it finds no Docker host; `probeDocker` recovers it. A start error, a failed guard or a failed ingest is always a failure.

## 6. Commands and CI

```bash
make test-integration     # go test -race -count=1 -timeout 15m -v -run '^TestIntegration$' ./tests/e2e/
go test -count=1 -v -run '^TestIntegration' ./tests/e2e/   # matches only TestIntegration, TestIntegrationImage_MatchesDockerCompose, TestIntegration_SkipsUnderShort
go test -count=1 ./tests/e2e/                               # the whole package: also the helper and guard tests, and TestIntegration (needs Docker)
go test -race -short ./...                                  # the gate: TestIntegration is skipped
```

- `-count=1` is mandatory when you need a real run: go test caching tracks neither Docker state nor the cmd/ sources that the test builds with `go build`, so a plain run may report a cached PASS.
- A plain `go test ./...` (without `-short`) runs `TestIntegration`.
- Typical timing with the image cached: container start about 7 s, ingest under 1 s, queries about 0.1 s, whole test about 10 s. The first run pulls the image.
- CI: job `integration` in `.github/workflows/ci.yml`, parallel to `lint` and `test`, same triggers, `runs-on: ubuntu-latest`, `timeout-minutes: 15`, `actions/checkout@v7`, `actions/setup-go@v7` with `go-version-file: go.mod`, and the same `go test` command as the Make target. No services, no secrets, no image cache. `lint` and `test` stay untouched.
- If you change the command in one place, change the other, and keep `help` aligned (`%-17s` in the Makefile).

## 7. Mutation discipline

- Write a script in your scratchpad, one line per mutant (name, exact old text, exact new text). Apply each mutant to a COPY of the tree outside the repo, compile-check first (a compile failure is "compile error", not killed), then run `go test -count=1 -run '^TestIntegration$' ./tests/e2e/` inside the copy (the binaries are built from the copy).
- Two kinds: production mutants (mapping.json, `internal/opensearch/search/body.go`, `cmd/ingest/record.go`, `internal/opensearch/index.go`: the integration test must fail) and expectation mutants (change one literal; the test must fail).
- Each mutant costs one container start (about 10 s locally, more in CI), so keep the list to about 10 per task. Every survivor is killed by a new case or listed as an equivalent with the reason.
- The mutants of earlier rounds worth repeating: `catPrefix` not stripped, brands not trimmed or deduplicated, grade not lower-cased, `Finalize` without the refresh, fuzziness 0, operator `and`, `asciifolding` removed from `folded`, `facetSize` 9, `post_filter` dropped, `brands.norm` `ignore_above` 5, `collapse` removed from the suggest body, `autocomplete_truncate` length 14, `min_gram` 3.

## 8. Live-cluster safety

- No test, script or agent talks to `127.0.0.1:9200` (the compose cluster from `make up`). The address under test comes only from the container (`startOpenSearch`), never from an environment variable or a flag; there is no switch to reuse a running cluster.
- `freshClusterError` runs before any binary starts and refuses port 9200 and any cluster that already has the `products` alias or a `products-*` index. Do not weaken it, and do not make it follow redirects or print bodies (each has a test).
- Scratch containers for your own checks: `docker run --rm -d -p 127.0.0.1::9200 ...` (random loopback port), removed at the end. Never `-p 9200:9200`.
- Never run `make ingest` or `go run ./cmd/ingest` for this test: the integration test starts its own binaries on its own cluster.
- The security plugin is off in the test container (no auth, no TLS); binding to `127.0.0.1` keeps it off the network during the run.

## 9. Known limits (accepted, do not "fix" without a decision)

- One shared index: the alias is hard-coded, so there is no per-test isolation. Index names are per UTC second (`products-<second>`), so two ingests in one second collide. Two test runs at the same time are safe because each has its own container and random ports.
- Collapse spelling is not deterministic: inside a collapse group the shown spelling is the top hit's, and with tied scores it can be any of the spellings that fold to one key. Never assert one spelling; use a group.
- The `categories.norm` normalizer is not observable with this fixture (only lower-case ASCII category tags, and the ingest does not lowercase them). Index-time asciifolding of the suggest field is not observable either (no accent-only name). Both are noted in the test files.
- Ryuk (the library's reaper container) lingers about 10 s after the test, and its port is published on `0.0.0.0` (library-controlled). A leftover `org.testcontainers` container right after a run can be Ryuk; check again after about 30 s.
- A SIGKILL, or a second signal, before the library registers with Ryuk (the first second of the start) leaves a container in state Created or running that no reaper removes. A signal during an image pull that cancels the start after the grace can do the same. This is the `shortcut:` comment above `startOpenSearch`. Remove such containers with: `docker rm -f $(docker ps -aq --filter label=org.testcontainers)`.
- The first Ctrl-C during the container start keeps a 30 s grace (`interruptGrace`): cancelling the library at once while it creates Ryuk leaves Ryuk in state Created. A second Ctrl-C kills the process.
- A Docker Desktop with the daemon absent was verified only through a hung socket (the probe gives a skip, or a failure under `CI`, at 30 s). `DOCKER_HOST=unix:///nonexistent.sock` does not reproduce "no daemon": the library falls back to `/var/run/docker.sock`.
- Facet counts and the top-10 cut depend on the whole fixture: a new row changes many literals (section 4).
- The test needs Docker Hub access for the first pull (anonymous pull limits could fail a CI run) and memory for the container (measured peak about 818-883 MiB against a 1.5 GiB budget).

## 10. Troubleshooting

- SKIP with `integration test needs a running Docker daemon`: start Docker. Under `CI=1` the same condition fails. Check the host the library will use: `docker context ls`, `echo $DOCKER_HOST`.
- `Cannot connect to the Docker daemon` or a wrong socket: set `DOCKER_HOST` (for example Colima or rootless Docker) before running.
- Leftover containers after a crash:
  - `docker ps -a --filter label=org.testcontainers` lists them (Ryuk removes the normal ones within about 10 s).
  - `docker rm -f $(docker ps -aq --filter label=org.testcontainers)` removes them by hand (it errors with "requires at least 1 argument" when nothing matches: that means nothing is left).
  - A Created Ryuk or OpenSearch container left by SIGKILL is the accepted window of section 9.
- A leftover child: `pgrep -f` with the temp binary path; the guarded wrapper should have ended it within `stopGrace` seconds.
- `guard: ...` failure in `TestIntegration`: the cluster at that address already has the alias or an index; with a fresh container this should not happen, so check for a stale container or a changed address source.
- Container does not start in time: the 5 min `containerStartWait` includes the image pull; check Docker Hub reachability and disk space.
- A passing run that took 0.00 s is a cached result: rerun with `-count=1`.
- A new case fails but the literal looks right: query the engine by hand on your own scratch container (never port 9200), with `_analyze` for analyzers (`folded`, `autocomplete_search`) and normalizers (`folded_keyword`), and compare with your derivation before touching the code.

## 11. Gate before returning

```bash
gofmt -l . && go vet ./... && go build -o /dev/null ./... && go test -race -short ./...
make test-integration          # needs Docker; at least one real run, no SKIP line
golangci-lint run ./...        # make lint, needs the pinned v2.15.0
```

Checklist:

- Every literal derived by hand from the fixture, no order asserted for hits or suggestions, facet lists exact and in order.
- Fixture, `integrationDocs`, report literals, LICENSE and all dependent tables changed together.
- Mutation sweep run on a tree copy, survivors killed or explained.
- No `time.Sleep` to wait, every wait bounded, `-count=1` on every real run.
- Nothing talked to `127.0.0.1:9200`; no container with label `org.testcontainers` remains about 30 s after the run.
- This skill and `.github/workflows/ci.yml` and the Makefile target still match what they describe.
