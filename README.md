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

Early design. The architecture is specified in [docs/DESIGN.md](docs/DESIGN.md)
([diagram](docs/architecture.svg)); there is no implementation yet.
The threat model (scope, trust boundaries, STRIDE analysis) is in
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md).

## Development

Regenerate the architecture diagram:

```sh
npx @mermaid-js/mermaid-cli -i docs/architecture.mmd -o docs/architecture.svg
```

## License

[MIT](LICENSE)
