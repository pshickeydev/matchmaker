# AGENTS.md

## Project

Matchmaker coordinates multiple independent Crush agents across a fleet of
projects. Go, single static binary, embedded SQLite store. The architecture
and its non-negotiable invariants live in
[docs/DESIGN.md](docs/DESIGN.md); the security analysis lives in
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md). Both are normative: when code
and docs disagree, stop and surface the conflict.

The Go tree is a function-level scaffold: signatures and design-referencing
doc comments are contracts. Preserve the cited DESIGN/THREAT_MODEL section
references when editing.

## Commands

- Build: `go build ./...`
- Vet: `go vet ./...`
- Test: `go test ./...`
- Format: `gofmt -w .`
- Regenerate architecture diagram:
  `npx @mermaid-js/mermaid-cli -i docs/architecture.mmd -o docs/architecture.svg`

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
