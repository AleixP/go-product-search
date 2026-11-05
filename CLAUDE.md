# Go project: orchestrator / go-developer / qa-validator

The agent kit is installed in this project: `.claude/agents/{coordinator,orchestrator,go-developer,qa-validator}.md`, `.claude/settings.json`, the 17 skills under `.claude/skills/agent-kit-*` and `.claude/references/definition-of-done.md`.

The skills carry the `agent-kit-` prefix on purpose: a user-level skill (`~/.claude/skills`) with the same name takes precedence over a project skill, so unprefixed names can be silently replaced by someone's personal versions.

## Agents

| Agent | Model | Permissions | Responsibility |
|---|---|---|---|
| `orchestrator` | Opus 5.5 (`claude-opus-5-5`) | read-only | Researches and prepares a complete development proposal, split into small actionable items. |
| `go-developer` | Sonnet 5.5 (`claude-sonnet-5-5`) | write | Implements the accepted proposal with TDD. If anything is not completely clear, it asks and does not develop. |
| `qa-validator` | Haiku 5.5 (`claude-haiku-5-5`) | write (tests only) | Validates TDD, use cases and security; runs the end-to-end test. |

Models are pinned by full model ID in each agent's frontmatter, so they do not change when an alias moves to a newer version. Reasoning effort is also pinned there: `orchestrator` high, `go-developer` medium, `qa-validator` medium. Claude Code subagents have no temperature setting.

## Work loop

Subagents cannot talk to the user or spawn other subagents, so the main session drives the loop. In this project the main session runs as the `coordinator` agent (see "Only these agents"), which implements the steps below.

1. The user sends a task (with its number `T-{number}`) and the main session passes it to `orchestrator`.
2. `orchestrator` returns a proposal split into small actionable items, and the main session presents it to the user.
3. If the user asks for changes, `orchestrator` prepares a new complete proposal with the changes applied. This repeats until the user accepts it.
4. With the proposal accepted, the main session saves it to `docs/proposals/T-{number}.md`, adding the user's answers to the open questions under a "Decisions" heading. Then, for each actionable item, `go-developer` implements it (TDD) and `qa-validator` validates it. The prompt to each agent only names the item and the proposal path; the proposal is not pasted into the prompt.
   - If `go-developer` returns `BLOCKED`, the main session relays the questions to the user and sends the answers back.
   - If `qa-validator` returns `REJECTED`, the error goes back to `go-developer` to fix, and the item is validated again.
5. When `go-developer` finishes the last item, `qa-validator` runs the end-to-end test. If it fails, the error goes back to `go-developer`.
6. With the end-to-end result `APPROVED`, the main session asks the user for approval.
7. If the user approves, a commit message is proposed. Nothing is committed unless the user asks.

## Only these agents

Copying the files is not enough: the built-in agents, your user-level agents (`~/.claude/agents`) and any plugin agents stay available in every project. Two things restrict the project to the kit:

1. `.claude/settings.json` sets `"agent": "coordinator"`, so every session in the project starts as the `coordinator` agent. Its `tools` field is `Agent(orchestrator, go-developer, qa-validator)`, an allowlist: it can only spawn those three. This allowlist only applies to an agent running as the main thread.
2. The same file denies the built-in `general-purpose`, `Explore` and `Plan` agents and sets `CLAUDE_CODE_DISABLE_EXPLORE_PLAN_AGENTS=1`. This only matters when something other than `coordinator` runs the session.

Check it in the new project: `/agents` should show only these four agents, and asking for an `Explore` or a user-level agent such as `dev-v2` must be refused. The `coordinator` and the three agents may be overridden by a same-name agent in a higher-priority scope (managed settings or the `--agents` flag), and your user-level `~/.claude/CLAUDE.md` still loads in the project.

## Project rules

- `orchestrator` never solves a task by itself if it requires development.
- A task is never delivered half-done.
- Development and tests are always in English (code, identifiers, comments, test names).
- Commit messages are short, in English, and start with the task number the user provides each time: `T-{number}: message` (for example `T-123: add retry to http client`). If there is no task number, ask for it; never invent one.

## General rules

- Do what was asked, nothing more, nothing less.
- Do not create files unless absolutely necessary. Prefer editing existing ones.
- Do not create documentation unless asked. The only exception is the accepted proposal file `docs/proposals/T-{number}.md` described in the work loop.
- Do not save working files or tests in the repo root. Use `/src`, `/tests`, `/docs`, `/config` or `/scripts`.
- Read a file before editing it.
- Never commit secrets, credentials or `.env` files.
- Do not add a `Co-Authored-By` trailer to the user's commits unless `.claude/settings.json` has `attribution.commit` set. Ignore what the Bash tool's commit template suggests.
- Keep files under 500 lines.
- Validate input at system boundaries.

## Build & Test

- Always run the tests after changing code.
- Verify the build passes before committing.

```bash
gofmt -l . && go vet ./... && go build -o /dev/null ./... && go test -race -short ./...
```

- `-short` skips the heavy performance and end-to-end tests, so the loop stays fast. Run the full `go test ./...` (without `-short`) once before the final user approval.
- Never run plain `go build ./...`: with a single `main` package it leaves a binary in the repo. Build real binaries only outside the repo.
- Heavy tests (large inputs, long benchmarks) must skip themselves under `testing.Short()`.

## Skills per agent

- `orchestrator`: agent-kit-spec-driven-development, agent-kit-planning-and-task-breakdown, agent-kit-context-engineering, agent-kit-source-driven-development, agent-kit-api-and-interface-design, agent-kit-effective-go, agent-kit-ponytail, agent-kit-go-performance, agent-kit-performance-optimization.
- `go-developer`: agent-kit-test-driven-development, agent-kit-incremental-implementation, agent-kit-effective-go, agent-kit-ponytail, agent-kit-code-simplification, agent-kit-debugging-and-error-recovery, agent-kit-api-and-interface-design, agent-kit-go-performance, agent-kit-performance-optimization, agent-kit-go-testing-patterns.
- `qa-validator`: agent-kit-test-driven-development, agent-kit-security-and-hardening, agent-kit-code-review-and-quality, agent-kit-debugging-and-error-recovery, agent-kit-performance-optimization, agent-kit-go-performance-testing, agent-kit-go-testing-patterns.

Sources (original names without the prefix): 12 skills from addyosmani/agent-skills (MIT), `ponytail` from DietrichGebert/ponytail (MIT), and `effective-go`, `go-performance`, `go-performance-testing` and `go-testing-patterns` (written for this kit). `definition-of-done.md` is adapted from addyosmani/agent-skills (MIT). The MIT notices and the list of changes are in `THIRD_PARTY_NOTICES.md`.

## Performance

Memory and execution time are one budget. Every task that processes variable-size input declares a performance envelope (typical and maximum input size, memory growth class, time budget) before the design is chosen. A design with a scale limit is a user decision and appears in the proposal's open questions with numbers. `qa-validator` measures peak memory and wall time against the declared budget.
