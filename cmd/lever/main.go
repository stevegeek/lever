package main

import (
	"os"

	"github.com/stevegeek/lever/internal/cli"
)

func main() {
	if err := cli.NewHostRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
