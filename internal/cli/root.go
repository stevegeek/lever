package cli

import (
	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/orbstack"
	"github.com/lever-to/lever/internal/exec"
	"github.com/spf13/cobra"
)

const Version = "0.0.0-dev"

// BackendFactory builds a backend for a given machine name.
type BackendFactory func(machine string) backend.Backend

func defaultFactory(machine string) backend.Backend {
	return orbstack.New(exec.RealRunner{}, machine)
}

func NewRoot() *cobra.Command { return NewRootWithBackend(defaultFactory) }

func NewRootWithBackend(factory BackendFactory) *cobra.Command {
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration"}
	root.AddCommand(&cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println(Version) }})
	root.AddCommand(newUpCmd(factory), newDownCmd(factory), newDoctorCmd(factory))
	return root
}
