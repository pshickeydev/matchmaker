# AGENTS.md

## Project

Matchmaker coordinates multiple independent Crush agents across a fleet of
projects. Go, single static binary, embedded SQLite store. The architecture
and its non-negotiable invariants live in
[docs/DESIGN.md](docs/DESIGN.md); the security analysis lives in
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md). Both are normative: when code
and docs disagree, stop and surface the conflict.

Doc comments throughout the tree cite DESIGN/THREAT_MODEL sections
(e.g. `DESIGN §5.3`). Those citations are contracts: preserve them when
editing, and read the cited section before changing the behavior it
governs. Several comments also record facts verified against the pinned
Crush v0.94.1 source; treat those as ground truth for the pinned version.

## Commands

- Build: `go build ./...`
- Vet: `go vet ./...`
- Test: `go test ./...`
- Format: `gofmt -w .`
- Regenerate architecture diagram:
  `npx @mermaid-js/mermaid-cli -i docs/architecture.mmd -o docs/architecture.svg`
- Real-fleet smoke test: `scripts/smoke-real-crush.sh <api-path> <web-path>`
  (requires a local `crush` v0.94.1 release build and a working provider
  credential; see the script header and README)

## Architecture and package map

One `matchmaker` binary (`cmd/matchmaker`): daemon, CLI, TUI, and the
coordination MCP server are all subcommands/modes (DESIGN §10.2). The
daemon is the only writer of the SQLite store and the only client of the
fleet; CLI/TUI invocations are short-lived clients over a local Unix-socket
RPC. Flow: CLI -> `rpc` -> daemon -> (`goalvalidate`, `dispatch`,
`supervise`, `aggregate`, `fleet`, `notes`/`coordination`) -> `crushapi`
-> per-project `crush server` processes. Everything under `internal/`:

| Package | Role |
|---|---|
| `daemon` | Wiring and lifecycle: owns store, reconciler, dispatcher, supervisor, coordination server, RPC server (DESIGN §3) |
| `model` | Core types and persisted state machines for goals, steps, target executions, runs, instances, notes (§4) |
| `store` | SQLite task store (modernc.org/sqlite, pure Go); versioned forward-only migrations; all transitions validate through `model` (§5.2, §9.2) |
| `config` | TOML fleet + matchmaker config; all operator-tunable defaults and bounds live here |
| `goalvalidate` | Atomic goal-submission validation; any error rejects everything (§5.2) |
| `dispatch` | Drives ready steps: frozen targets, per-instance serialization, prompt rendering, note injection (§5.3) |
| `supervise` | Consumes SSE streams: permissions, questions, run completion, cancellation, recovery (§5.3) |
| `fleet` | Reconciliation loop (observe/compare/converge) plus supervised crush child processes (§5.1) |
| `crushapi` | Hand-rolled allowlist-scoped typed client for the Crush REST API (§2, §9.6) |
| `sse` | Stdlib SSE frame reader used by `crushapi` |
| `notes` | Note passing: audience expansion, durable cursors, at-least-once reads (§5.4) |
| `coordination` | The one multiplexed coordination MCP server (`note_send`, `note_read`, `result_read`) (§5.4, §9.4) |
| `onboarding` | Installs the Matchmaker-owned crushrc registration block per project (§5.4) |
| `aggregate` | Step/goal rollups and per-goal reports (§5.5) |
| `rpc` | Operator-only local Unix-socket RPC with idempotency keys (§3) |
| `cli` | Cobra command surface; commands are RPC clients of the daemon |
| `render` | The single terminal sanitizer for all agent-controlled strings (§5.5) |
| `logging` | Structured logging rules for agent content (§5.5, THREAT_MODEL §5.4) |
| `daemonlock` | OS-level exclusive lock on the state directory (§3) |
| `crushtest` | Shared fake Crush server for tests; stdlib only so any package can import it without cycles |
| `planner`, `tui` | Deferred stubs: functions panic with `not implemented` (planning runs, dashboard, prune/gc are the post-MVP milestones) |

## Non-obvious invariants and gotchas

- **Crush is pinned to v0.94.1** (`config.PinnedCrushVersion`); bumping the
  pin requires re-running the threat-model review (DESIGN §9.3). Note the
  `build_id` instability for unflagged builds documented there.
- **`crushapi` is allowlist-scoped.** It must never grow wrappers for the
  unauthenticated shell endpoint, `permissions/skip`, or config-mutation
  endpoints (DESIGN C6). Workspace API responses embed provider API keys:
  never log or persist them raw.
- **Every agent-controlled string shown in a terminal goes through
  `internal/render`**; no output path may bypass it. Logs never interpolate
  agent text into message strings (use quoted, length-bounded fields via
  `render.NormalizeField`); raw Crush payloads, prompts, note bodies, and
  run output are never logged.
- **Permission decisions are recorded transactionally before responding**
  to Crush; if the record fails, the answer is `deny`. Never grant an
  unaudited request (§5.3).
- **One dedicated Crush session per run attempt**; permission events carry
  no RunID, so session correlation is the only safe binding (§5.3).
- **Runs in `unknown` state are never auto-resubmitted**; they block their
  workspace serialization until reconciled from durable session IDs or
  explicitly abandoned (§5.3).
- **State changes that gate actions commit in one transaction** with the
  gated action (goal acceptance + target expansion, note-cursor advance +
  prompt submission, permission record + decision).
- Child crush processes receive an allowlisted environment only (plus
  operator-declared `pass_env` names), and their idle timeout is raised to
  1h because Matchmaker owns instance lifecycle (§5.1).
- Editing `examples/*.toml` or `examples/*.json` affects tests:
  `internal/config/examples_test.go` and
  `internal/goalvalidate/examples_test.go` parse and validate them.

## Testing

- `go test ./...` runs the full suite against the fake Crush fleet in
  `internal/crushtest` (scriptable: permission requests, questions,
  completions, crashes, stream drops, MCP client for note scenarios).
  Add new fake-server behaviors to `crushtest` rather than standing up
  ad-hoc mocks.
- The suite covers mid-goal coordination and crash-restart recovery end
  to end; real-Crush verification is the manual smoke script, not CI.
- For bug fixes: write the failing test first, observe it fail, then fix
  and observe it pass.

## Starter rules

- Avoid magic numbers and strings by extracting recurring or meaningful values into descriptive constants (const) or enums. Keep self-explanatory, one-off values inline to avoid clutter. If a value comes from a spec (e.g. HTTP 200 OK), use a constant regardless.

- Reduce code indentation. Avoid Arrow Anti-Pattern. Leverage early return and continue.

- Keep function names short. Less than 30 characters.

- Use enums instead of booleans for function parameters.

- Treat member visibility changes as a breaking design shift. Keep all fields and functions private unless external access is strictly required by the design. Prompt the user for explicit approval before changing any access modifier from private to internal or public.

- Program to levels of abstraction. Lower-level mechanics (e.g., raw hardware I/O, sector parsing, direct socket streams) must be encapsulated in a dedicated driver/abstraction layer. Expose clean, high-level APIs to the rest of the application so calling code works with domain concepts, not raw implementation details.

- Don't touch blocks of code unrelated to the feature you implement. e.g. Don't add comments to a block of code if you did not create it or modify it. As much as possible try to minimize the number of changed lines when implementing a feature.

- Strictly adhere to the layered boundary hierarchy: each layer may only communicate with its immediate neighbor directly below it. Never "punch holes" through layers (e.g., controllers or UI components must never directly call database queries, raw hardware drivers, or low-level network clients; always route through the intermediate service/abstraction layer).

- Commits MUST be prefixed with a type: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, etc. An optional scope MAY be added in parentheses: `feat(parser):`. A breaking change MUST be indicated with `!` before the colon, or a `BREAKING CHANGE:` footer. The description MUST follow the type/scope prefix and colon-space.

- A longer body MAY follow after a blank line, explaining what and why. One or more footers MAY follow after a blank line (e.g. `Reviewed-by:`, `Refs:`). Keep the subject line under 50 characters and wrap body lines at 72 characters.

- If the prompt indicates that a bug is being fixed, don't write the fix right away. First write the test. Observe it failing. Then write the fix. And observe the test passing.

- Any updates to AGENTS.md that are relevant to users must also be reflected in README.md.
