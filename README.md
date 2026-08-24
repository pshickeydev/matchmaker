# Matchmaker

Coordinate multiple independent [Crush](https://github.com/charmbracelet/crush)
agents across a fleet of projects to accomplish a goal.

Matchmaker runs one `crush server` per project, submits goals expressed as
dependency graphs of steps, supervises execution (permissions, timeouts,
retries), lets agents pass notes to one another mid-goal, and aggregates the
outcome into a per-goal report.

## Status

Early design. The architecture is specified in [docs/SPEC.md](docs/SPEC.md)
([diagram](docs/architecture.svg)); there is no implementation yet.

## Development

Regenerate the architecture diagram:

```sh
npx @mermaid-js/mermaid-cli -i docs/architecture.mmd -o docs/architecture.svg
```

## License

[MIT](LICENSE)
