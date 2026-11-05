---
name: agent-kit-effective-go
description: Idiomatic Go checklist for writing, reviewing and refactoring Go code - formatting, naming, errors, interfaces, concurrency, resource handling and package layout. Use whenever Go code is planned, written or validated, so the result reads like standard Go.
---

# Idiomatic Go

A working checklist based on the official guides: [Effective Go](https://go.dev/doc/effective_go) and the [Code Review Comments](https://go.dev/wiki/CodeReviewComments). When this checklist and the project's own conventions disagree, the project wins.

## Formatting and naming

- Run `gofmt` on everything. A formatting diff is never a review comment.
- Exported names are `MixedCaps`, unexported are `mixedCaps`. No underscores in Go names, no `Get` prefix on getters (`Name()`, not `GetName()`).
- Package names are short, lower case, one word, and not repeated in the names they export (`http.Server`, not `http.HTTPServer`).
- Initialisms keep one case: `ID`, `URL`, `HTTP`, `userID`.
- Single-method interfaces are named after the method plus `er`: `Reader`, `Searcher`.
- Every exported identifier has a doc comment that starts with its name.

## Errors

- Return errors; reserve `panic` for programmer mistakes that cannot be handled.
- Never ignore an error silently. If it is truly irrelevant, say why in a comment.
- Add context when passing an error up, and keep the cause: `fmt.Errorf("load config: %w", err)`. Check with `errors.Is` and `errors.As`, not string comparison.
- Error strings are lower case and have no final punctuation.
- Handle the error first and return early; keep the happy path unindented.

## Types and interfaces

- Make the zero value useful, so callers do not need a constructor for simple types.
- Accept interfaces, return concrete types. Define an interface where it is consumed, and keep it small.
- Do not add an interface until a second implementation or a test boundary needs one.
- Use pointer receivers when the method mutates the value or the value is large; do not mix receiver kinds on one type.
- Use composite literals with field names; avoid positional struct literals outside the defining package.

## Concurrency

- Share memory by communicating: pass ownership of data over channels, or protect shared state with one clearly scoped mutex.
- Every goroutine has a known way to stop. Pass a `context.Context` as the first parameter of anything that blocks or does I/O.
- The sender closes a channel, never the receiver.
- Prove goroutine code with `go test -race`.

## Resources and control flow

- Pair acquire and release in the same function: `defer f.Close()` right after a successful open. Check the error of a `Close` that can lose data (writers).
- `defer` in a loop runs at function exit, not per iteration; wrap the body in a function when that matters.
- Prefer `switch` over long `if/else` chains, and `for range` over manual index loops.
- Slices and maps: preallocate with `make(T, 0, n)` when the size is known, and remember that a sub-slice keeps its whole backing array alive.

## Layout and tests

- Keep packages small and cohesive; avoid `util` and `common`. Put internal code under `internal/`.
- Tests live next to the code (`x_test.go`), are table-driven when cases share a shape, and use `t.Helper()` in helpers.
- Compare with `cmp.Diff`-style output or clear messages: say what was got and what was wanted.

## Review checklist

- [ ] `gofmt` and `go vet` clean
- [ ] Every error handled or deliberately dropped with a reason
- [ ] No exported name without a doc comment
- [ ] Interfaces are small and defined by the consumer
- [ ] Every goroutine can stop; `-race` is clean
- [ ] Every opened resource is closed
