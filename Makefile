ARGS ?=
DATA_URL ?= https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz
GOLANGCI_LINT_VERSION := v2.15.0
GIT ?= git
REV ?= HEAD
# REV is untrusted input: it is exported and read as "$$REV" in recipes, never expanded as $(REV)
# inside shell text (that would let a backtick or quote run code). GIT is a trusted command prefix
# by design (tests set it to a wrapper); FORBIDDEN_RE and NOREPLY_RE are constants used only inside
# single quotes. No other make variable is expanded in the history-check or body-check recipes.
export REV
# Bracket classes keep this file free of the literal forbidden strings. Case-insensitive.
FORBIDDEN_RE := co-authored[-]by|claude[-]session|claude[.]ai/code/session_|(^|[^a-z])generated[ ]with
# Allowed identities: GitHub's own committer, or any GitHub noreply address: login or
# <digits>+login, login being [A-Za-z0-9-]+ with an optional [bot]. An all-digit local part
# (7648118@users.noreply...) matches the login form and is accepted; anything else is not
# (other domains, a non-numeric prefix before +, empty, spaces).
NOREPLY_RE := ^[0-9a-f]+ (noreply@github[.]com|([0-9]+[+])?[A-Za-z0-9-]+(\[bot\])?@users[.]noreply[.]github[.]com)$$

.DEFAULT_GOAL := help

.PHONY: help up down ingest data run test test-integration test-history-check lint history-check body-check

help: ## List the available targets
	@grep -h -E '^[A-Za-z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "%-17s %s\n", $$1, $$2}'

up: ## Start OpenSearch with docker compose and wait until healthy
	docker compose up -d --wait

down: ## Stop OpenSearch (keeps the data volume)
	docker compose down

ingest: ## Load the dataset into OpenSearch (pass flags with ARGS="...")
	go run ./cmd/ingest $(ARGS)

data: ## Download a local copy of the dataset to data/ (about 1.5 GB)
	mkdir -p data
	curl -fL --retry 3 -o data/products.csv.gz.part $(DATA_URL) || { rm -f data/products.csv.gz.part; exit 1; }
	mv data/products.csv.gz.part data/products.csv.gz

run: ## Start the search API (pass flags with ARGS="...")
	go run ./cmd/api $(ARGS)

test: ## Run the Go tests with the race detector (-short)
	go test -race -short ./...

test-integration: ## Run the OpenSearch integration test (needs Docker)
	go test -race -count=1 -timeout 15m -v -run '^TestIntegration$$' ./tests/e2e/

test-history-check: ## Prove the LC_ALL=C guards of history-check and body-check (GNU grep part SKIPs without it)
	@sh tests/history-check/locale_test.sh
	@sh tests/history-check/gnu_grep_test.sh

# Requires golangci-lint v2.15.0 (GOLANGCI_LINT_VERSION) installed locally (no auto-install).
lint: ## Run golangci-lint (needs the pinned local version)
	golangci-lint run ./...

# POSIX sh only (CI runs dash). Each listing is captured on its own so a failing git fails the target;
# only grep status 1 (no match) is tolerated. REV is resolved to a commit hash before it reaches git log.
# LC_ALL=C on the grep -v selection: it is the other path. GNU grep under a UTF-8 locale prints "binary file
# matches" to stderr, outputs nothing and exits 0 for a line with an invalid byte (fail open); BSD grep selects it.
# tests/history-check/gnu_grep_test.sh (make test-history-check) proves these guards under GNU grep.
# LC_ALL=C on cut: BSD cut under a UTF-8 locale prints "Illegal byte sequence" for a listing line with an
# invalid byte, outputs nothing and the pipeline goes on (fail open); proved by tests/history-check/locale_test.sh.
# sed and sort only see hex hashes after cut, so their LC_ALL=C is not locale-sensitive (kept for consistency).
history-check: ## Fail on attribution strings or non-noreply emails in commits reachable from REV (default HEAD)
	@set -e; \
	case "$$REV" in -*) echo "history-check: REV must not start with a dash"; exit 1;; esac; \
	sha=$$($(GIT) rev-parse --verify -q "$$REV^{commit}") || { echo "history-check: REV is not a commit"; exit 1; }; \
	msgs=$$(LC_ALL=C $(GIT) log --format=%h -i -E --grep='$(FORBIDDEN_RE)' "$$sha" --); \
	authors=$$($(GIT) log --format='%h %ae' "$$sha" --); \
	committers=$$($(GIT) log --format='%h %ce' "$$sha" --); \
	bad_a=$$(printf '%s\n' "$$authors" | LC_ALL=C grep -v -E '$(NOREPLY_RE)') || [ $$? -eq 1 ]; \
	bad_c=$$(printf '%s\n' "$$committers" | LC_ALL=C grep -v -E '$(NOREPLY_RE)') || [ $$? -eq 1 ]; \
	bad=$$(printf '%s\n%s\n%s\n' "$$msgs" "$$bad_a" "$$bad_c" | LC_ALL=C cut -d' ' -f1 | LC_ALL=C sed '/^$$/d' | LC_ALL=C sort -u); \
	if [ -n "$$bad" ]; then echo "history-check: offending commits (attribution string or non-noreply email):"; echo "$$bad"; exit 1; fi; \
	echo "history-check: ok"

# grep status: 0 match (fail), 1 no match (pass), anything else is a read error (fail closed).
# BSD grep (macOS) exits 1 on a read error but prints a message, so any stderr output also fails closed.
# LC_ALL=C on this match path: under a UTF-8 locale BSD grep skips lines with invalid bytes (a miss) and
# GNU grep cannot match [^a-z] against an invalid byte; matching bytewise avoids both.
body-check: ## Fail if the text on stdin has an attribution string (prints nothing from it)
	@err=$$(LC_ALL=C grep -q -i -E '$(FORBIDDEN_RE)' 2>&1); rc=$$?; \
	if [ -n "$$err" ] || { [ $$rc -ne 0 ] && [ $$rc -ne 1 ]; }; then echo "body-check: read error"; exit 1; fi; \
	if [ $$rc -eq 0 ]; then echo "body-check: attribution string found"; exit 1; fi; \
	echo "body-check: ok"
