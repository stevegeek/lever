package main

import (
	"os"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/cli/manager"
)

// main runs through cli.Execute, as the host binary does: errors are
// sanitized (their text can come from the jail) and an ExitError's code is
// the exit status.
func main() {
	os.Exit(cli.Execute(manager.NewRoot(), os.Stderr))
}
