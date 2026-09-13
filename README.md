# Matchmaker

Coordinate multiple independent [Crush](https://github.com/charmbracelet/crush)
agents across a fleet of projects to accomplish a goal.

Matchmaker runs one `crush server` per project, submits goals expressed as
dependency graphs of steps, supervises execution (permissions, timeouts,
retries), lets agents pass notes to one another mid-goal, and aggregates the
outcome into a per-goal report. Goals are authored directly by the operator or
drafted from a natural-language objective by a planning run executed on a fleet
instance.

## Status

The MVP is implemented and tested end to end: against a fake Crush fleet
in the test suite (mid-goal coordination over the coordination MCP server,
crash-restart recovery) and against real Crush v0.94.1 servers via the
smoke test below. The architecture is specified in
[docs/DESIGN.md](docs/DESIGN.md) ([diagram](docs/architecture.svg)); the
threat model (scope, trust boundaries, STRIDE analysis) is in
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).

Still deferred (packages keep their stubs): planning runs (`goal plan`),
the TUI dashboard, and prune/gc.

## Usage

Build and run the daemon, then drive it with the CLI:

```sh
go build ./cmd/matchmaker
./matchmaker daemon --fleet examples/fleet.toml --config examples/matchmaker.toml
./matchmaker onboard --approve api        # register the coordination MCP server
./matchmaker goal submit my-goal.json     # examples/note-passing-goal.json shows the schema
./matchmaker goal status <goal-id>
./matchmaker goal report <goal-id> --export report.json
./matchmaker shutdown
```

A scripted real-Crush smoke test (two projects, a two-step goal, a
mid-goal note) is in [scripts/smoke-real-crush.sh](scripts/smoke-real-crush.sh);
it requires a local `crush` v0.94.1 and a working provider credential.
If your global crushrc guards credentials with `${VAR:?...}` or uses
`crush login` (OAuth), declare the variable names in `pass_env` (see
[examples/matchmaker.toml](examples/matchmaker.toml)) and consider
`inherit_data_dir` to share the user's Crush data dir (see
[examples/fleet.toml](examples/fleet.toml)); for the smoke script, set
`SMOKE_PASS_ENV` to the comma-separated variable names.

## Development

Build, test, and format:

```sh
go build ./...
go vet ./...
go test ./...
gofmt -w .
```

Regenerate the architecture diagram:

```sh
npx @mermaid-js/mermaid-cli -i docs/architecture.mmd -o docs/architecture.svg
```

## License

[MIT](LICENSE)
