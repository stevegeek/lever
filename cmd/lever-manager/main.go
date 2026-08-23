package main

import (
	"os"

	"github.com/stevegeek/lever/internal/cli/manager"
)

func main() {
	if err := manager.NewRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
