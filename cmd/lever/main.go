package main

import (
	"os"

	"github.com/stevegeek/lever/internal/cli/host"
)

func main() {
	if err := host.NewRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
