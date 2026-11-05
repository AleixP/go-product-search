---
name: agent-kit-go-performance-testing
description: Measures and verifies Go performance against a declared budget for both memory and execution time. Use when validating a change that processes variable-size input, when a performance envelope was declared, or when a speed or memory claim needs evidence. Covers benchmarks, allocation counts, peak memory of a built binary, budget tests and regression comparison.
---

# Go Performance Testing

A performance claim without a number is an opinion. This skill turns the performance envelope (see `agent-kit-go-performance`) into checks that pass or fail, covering memory AND time together.

## 1. Start from the budget

Take the declared envelope and turn it into numbers with headroom:

| Budget | Example derived from the envelope |
|---|---|
| Max input size | 50 MB |
| Peak memory | at most 2x the input size, or a fixed 64 MB for a streaming design |
| Wall time | at most 3 s for the max input on a normal laptop |
| Allocations per item | at most 1 per distinct key (hot loop) |

If no envelope was declared, do not invent one silently. Report "no budget declared", measure anyway, and state the numbers you found.

## 2. Pick the right measurement

| Question | Tool |
|---|---|
| How fast is this function, how many allocations? | `go test -bench . -benchmem -count=10 -run '^$'` with `b.ReportAllocs()` |
| Did a change regress? | Save old and new results to files, compare with `benchstat old.txt new.txt` (`go run golang.org/x/perf/cmd/benchstat@latest` if not installed) |
| How many allocations in one call? | `testing.AllocsPerRun(100, fn)` inside a normal test |
| Where does time or memory go? | `go test -cpuprofile cpu.out -memprofile mem.out`, then `go tool pprof -top cpu.out` |
| Peak memory of the whole program | Build the binary, run it under `/usr/bin/time -l` (macOS: "maximum resident set size", in bytes) or `/usr/bin/time -v` (Linux: "Maximum resident set size", in KB) |
| Wall time of the whole program | The same `time` output, or `time.Since` in a test around a binary run |

Heap statistics from `runtime.ReadMemStats` are fine for in-process comparisons (call `runtime.GC()` first), but peak RSS of the built binary is the figure that matches what users see.

## 3. Use realistic and adversarial inputs

Generate inputs inside the test (deterministic seed, `t.TempDir()`); never commit big fixtures. Cover at least:

1. **Large, few distinct keys.** Best case for streaming. Memory should stay near-flat.
2. **Large, many distinct keys.** Worst case for state. Memory grows only with the keys.
3. **One huge token / line** with no separators. Catches default scanner limits and quadratic scans.
4. **Empty and tiny input.** Catches fixed overhead.

If the memory in case 1 grows with the input size, the design holds the whole input: report it.

## 4. Budget tests

Keep them out of the default quick run so unit tests stay fast:

```go
func TestPeakMemoryBudget(t *testing.T) {
    if testing.Short() {
        t.Skip("performance budget test")
    }
    // 1. generate the large input in t.TempDir()
    // 2. build the binary into t.TempDir()
    // 3. run it under /usr/bin/time (-l on darwin, -v on linux), parse peak RSS and wall time
    // 4. assert both against the budget, and log the measured numbers
}
```

Rules for stable tests:
- Assert **memory** in absolute or ratio terms. It is stable between runs.
- Assert **time** with a generous limit (about 3x what you measure) so a noisy machine does not fail the test. Use `benchstat` for tight time regressions instead of a hard threshold.
- Always `t.Logf` the measured numbers, even when passing, so the trend is visible.
- Skip with a clear message when the platform tool is missing (`exec.LookPath("/usr/bin/time")`).

## 5. Validate the balance

Check both axes after every performance change:

| Result | Verdict |
|---|---|
| Memory within budget, time within budget | pass |
| Memory within budget, time over | reject: faster design needed |
| Time within budget, memory over | reject: lighter design needed |
| One axis improved, the other got clearly worse | reject, or ask the user which trade is wanted |

Report a table:

```
Input            Peak memory (budget)   Wall time (budget)   Allocs/op
50 MB, 10 words  7 MB (<= 100 MB)       0.6 s (<= 3 s)       4
50 MB, 5M words  310 MB (<= 400 MB)     1.9 s (<= 3 s)       ...
```

## 6. Verdicts

- A measured budget overrun is **REJECTED** with the exact numbers, the input used, and the command to reproduce.
- A missing budget is an **advisory**, not a failure, unless the task makes performance a requirement.
- Never "fix" performance by editing production code: report to the developer. You may add benchmarks and budget tests.

## Checklist

- [ ] Budget taken from the declared envelope, or marked as missing
- [ ] Inputs include large-few-keys, large-many-keys and one huge token
- [ ] Peak memory of the real binary measured, not only the in-process heap
- [ ] Wall time measured with a noise-tolerant threshold
- [ ] Both axes reported in one table
- [ ] Commands to reproduce are in the report
