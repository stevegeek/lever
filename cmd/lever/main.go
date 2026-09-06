package main

import (
	"os"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/cli/host"
)

func main() {
	os.Exit(cli.Execute(host.NewRoot(), os.Stderr))
}
