---
name: agent-kit-go-performance
description: Designs Go code to be both memory-efficient and fast. Use when planning or writing code that processes input of variable size, does I/O, builds large collections, or runs concurrently. Forces a declared performance envelope before choosing a design, then picks the simplest design that meets both the memory and the time budget.
---

# Go Performance

Memory and execution time are two axes of one budget. Optimizing one to the extreme (an in-memory cache the size of the input, or a loop that re-reads the file ten times) is a failure on the other. The goal is the simplest design that meets BOTH budgets.

## Step 0: declare the performance envelope

Before choosing a design, write down (in the proposal if you plan, in a comment or the report if you build):

| Item | Example |
|---|---|
| Typical input size | 1 MB |
| Maximum input size we must survive | 500 MB, or "unbounded stream" |
| Memory budget, as a growth class | O(1), O(distinct keys), O(n) |
| Time budget | linear in input, a p95 latency, a throughput target |
| Concurrency | single goroutine, or N callers |

If the task does not say, assume the smallest realistic envelope, state the assumption, and flag it for the user. Never leave the maximum input size unstated.

If a simpler design has a scale limit (memory or time grows with the input), that is a decision for the user, not for you. Name the limit with rough numbers and offer the alternative with its cost.

## Default budgets

- **Memory should grow with the state you must keep** (distinct keys, top-K, output), not with the size of the input. Holding the whole input in memory is only justified when the algorithm truly needs random access to it.
- **Time should be linear or near-linear** in the input: O(n), or O(n log k) for top-K. Quadratic work on input-sized data needs a stated reason.
- A design that is 10x over the input size in memory, or needs several passes over input-sized data, needs a stated reason.

## Design choices in Go

| Situation | Prefer | Avoid | Why |
|---|---|---|---|
| Reading input | `bufio.Reader` / `bufio.Scanner` streaming | `io.ReadAll`, `os.ReadFile` on unbounded input | Memory stays O(chunk), not O(input) |
| Scanner tokens | `scanner.Buffer(buf, max)` when tokens may be long | Default Scanner on arbitrary tokens | Default max token is 64 KiB and fails with `ErrTooLong` |
| Splitting text | Iterate with `ReadRune` / `bytes` and count as you go | `strings.Fields*` / `Split` over the whole input | They allocate a slice with one 16-byte header per word |
| Top-K of M items | Min-heap of size K (`container/heap`), O(M log K) | Sort all M items when K is small | Time and memory both drop |
| Counting keys | `map[string]int` sized with `make(map, hint)` when the hint is known | Growing from zero repeatedly | Avoids rehashing |
| Building slices | `make([]T, 0, n)` when n is known | Repeated `append` from nil | Avoids copy-on-grow |
| Building strings | `strings.Builder` (`Grow` if size is known) | `s += x` in a loop | Quadratic copying |
| Many small writes to stdout or a file | `bufio.Writer` + `Flush` | One `fmt.Fprintf` per line straight to `os.Stdout` | One syscall per write |
| Byte data | `[]byte` and `bytes` functions while parsing | Converting to `string` per item when unneeded | Each conversion copies |
| Per-item keys that repeat | Copy only when inserting a new key (`m[string(b)]` lookups do not allocate) | `string(b)` before every lookup | Avoids one allocation per item |
| Concurrency | Single goroutine until measurement shows a CPU-bound bottleneck | Goroutines "for speed" | Coordination costs more than it saves on small work |
| `sync.Pool`, manual arenas, unsafe | Only with a benchmark showing the gain | Anywhere by default | Complexity without evidence |

## Rules

1. **Measure, do not guess.** Any claim "this is faster / lighter" needs a number from the `agent-kit-go-performance-testing` skill (benchmarks, allocation counts, peak memory). Without a number, take the simpler code.
2. **Simplest design that meets the envelope.** Do not build a streaming pipeline for a 10 KB input, and do not use `ReadAll` for a stream of unknown size. The envelope decides.
3. **Record the shortcut.** If you take a design with a known limit, leave `shortcut: <the limit>, <when to upgrade>` in a code comment and name it in the report.
4. **Hot path first.** Optimize only what the profile shows. Leave cold paths simple.
5. **Do not trade correctness or safety.** Input validation, error handling and race safety stay, whatever the speedup.
6. **Check both axes before you finish.** After an optimization, confirm memory did not get worse because of a speed fix, and that time did not get worse because of a memory fix.

## Red flags

- Memory use scales with input size when the result does not.
- `ReadAll`, `ReadFile`, `Fields`, `Split`, `ioutil` on user-sized data.
- A full sort when only the top few results are needed.
- A new goroutine or channel with no measurement behind it.
- Allocation inside a per-item loop (`fmt.Sprintf`, `string(b)`, `append` from nil, `strings.ToLower` on every token when most are already lowercase).
- "Optimization" PR with no before/after numbers.

## By role

- **Planner / orchestrator:** put the declared envelope in the proposal, state the design chosen for it, and list any scale-limited shortcut as an open question with numbers. Say which measurements `qa-validator` must take.
- **Developer:** stay inside the envelope. If meeting it is impossible or unclear, stop and ask instead of deciding. Report the design choices made for performance.
- **QA:** verify the envelope with measurements, see `agent-kit-go-performance-testing`.
