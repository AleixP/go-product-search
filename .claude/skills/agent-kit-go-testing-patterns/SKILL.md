---
name: agent-kit-go-testing-patterns
description: Patterns for writing Go tests that are fast, readable and trustworthy - table-driven tests, subtests, helpers, fakes instead of mocks, httptest for handlers, golden files, fuzzing, integration tests with containers, and flaky-test traps. Use when writing or reviewing Go tests, and when validating that tests would fail if the code were wrong.
---

# Go Testing Patterns

Go-specific companion to `agent-kit-test-driven-development` (the red, green, refactor loop) and `agent-kit-go-performance-testing` (memory and time budgets). This skill covers how a Go test is written. Use the standard `testing` package first; add a dependency only if the project already has it.

## Structure and naming

- A test file sits next to the code as `x_test.go`. Use package `foo_test` (black box) for the public behavior and package `foo` (white box) only to reach unexported logic that has no other seam.
- Name tests `TestType_Method_Condition` or `TestFunction_Condition`; a reader should know what broke from the name alone.
- Keep Arrange, Act, Assert visible, in that order, separated by a blank line.
- One behavior per test case. A test that needs "and" in its name is two tests.

## Table-driven tests with subtests

Use a table when cases share a shape; keep a case readable on its own.

```go
func TestParseSize(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{name: "plain number", in: "10", want: 10},
		{name: "empty", in: "", wantErr: true},
		{name: "negative", in: "-1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSize(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseSize(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
```

- Failure messages say what was called, what was got and what was wanted: `f(x) = got, want want`.
- `t.Fatalf` when the rest of the test cannot continue, `t.Errorf` to keep checking.
- Since Go 1.22 each loop iteration has its own variable, so `t.Parallel()` inside `t.Run` is safe without copying `tt`. Add `t.Parallel()` only when cases are independent and slow enough to matter.
- Compare errors with `errors.Is` / `errors.As`, never by message text, unless the message is the contract.

## Helpers and cleanup

- Mark helpers with `t.Helper()` so failures point at the caller.
- Use `t.TempDir()` for files, `t.Setenv` for environment variables and `t.Cleanup` for teardown. Do not write to the working directory or the real home.
- Build test data with small constructors, not shared mutable globals.

## Fakes over mocks

- Prefer a small hand-written fake or an in-memory implementation of the interface the code consumes. Assert on behavior and results, not on the order of internal calls.
- Mock only at boundaries you do not own (network, clock, filesystem, third-party clients). Inject the dependency through a field or parameter.
- Inject time (`func() time.Time` or a clock interface) instead of sleeping or reading `time.Now()` in logic you want to test.
- For readers and writers use `strings.NewReader`, `bytes.Buffer`, and `testing/iotest` (`ErrReader`, `OneByteReader`, `TimeoutReader`) to exercise failure and short-read paths.

## HTTP handlers

- Test handlers with `net/http/httptest`: `httptest.NewRecorder` plus a request from `httptest.NewRequest`, or `httptest.NewServer` when the real client path matters.
- Assert the status code, the headers that matter (`Content-Type`, `Location`) and the decoded body. Include the failure paths: bad input, missing resource, method not allowed, body too large.
- Test the handler through its public routing (the `http.Handler` you serve), so route patterns and method matching are covered.

## Golden files and fuzzing

- For large or structured output, keep expected output in `testdata/` and compare to it. Offer an `-update` flag that rewrites the file, and review its diff like code.
- Add a fuzz test (`func FuzzX(f *testing.F)`) to anything that parses untrusted input. Seed it with real examples, assert invariants (no panic, round trip, output within bounds), and commit crashers found under `testdata/fuzz/`.

## Integration tests with containers

- Put them behind `testing.Short()` or a build tag so the fast loop stays fast, and skip with a clear message when the container runtime is missing.
- Start the dependency once per package in `TestMain` or per test with `t.Cleanup` terminating it. Wait for readiness with a bounded timeout, never a fixed sleep.
- Isolate data per test (unique index, schema or prefix) so tests can run in any order.

## Flaky-test traps

- No `time.Sleep` to wait for something: use channels, `sync.WaitGroup`, polling with a deadline, or a `context` with timeout.
- No dependence on map iteration order, test order, wall-clock time, the network or a random seed that is not logged.
- Run `go test -race` and `go test -count=20` on anything concurrent before approving it.
- A test that passes without the production change proves nothing: see it fail first.

## Anti-patterns

| Anti-pattern | Problem | Instead |
|---|---|---|
| Asserting on internal call order or private state | Breaks on every refactor | Assert on results and observable effects |
| One huge test with many unrelated checks | First failure hides the rest | Table cases or separate tests |
| Shared mutable fixtures | Tests affect each other | Fresh data per test, `t.Cleanup` |
| `t.Skip` left in for "later" | Dead coverage | Fix it or delete it |
| Asserting on error text | Brittle wording | `errors.Is` / `errors.As` |
| Network or real clock in unit tests | Slow and flaky | Fakes and injected time |
| Coverage as the goal | Executed lines are not verified behavior | Check that a wrong implementation would fail |

## Checklist

- [ ] Each behavior and each failure path has a case
- [ ] Failure messages show input, got and want
- [ ] Helpers call `t.Helper()`, files use `t.TempDir()`, teardown uses `t.Cleanup`
- [ ] No sleeps, no order or time dependence
- [ ] Heavy and container tests skip under `-short`
- [ ] `go test -race -short ./...` is clean
