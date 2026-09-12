// Package main is the single matchmaker binary entrypoint.
// All modes (daemon, CLI, TUI, MCP coordination server) are subcommands of
// this one binary (DESIGN §10.2).
package main

import (
	"os"

	"github.com/pshickeydev/matchmaker/internal/cli"
)

func main() {
	if err := cli.Execute(os.Args[1:]); err != nil {
		os.Exit(1)
	}
}
