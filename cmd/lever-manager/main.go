package main

import (
	"os"

	"github.com/stevegeek/lever/internal/cli"
)

func main() {
	if err := cli.NewManagerRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
