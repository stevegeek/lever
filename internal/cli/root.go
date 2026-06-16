package cli

import (
	"os"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/orbstack"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

const Version = "0.0.0-dev"

// BackendFactory builds a backend for a given machine name.
type BackendFactory func(machine string) backend.Backend

// ClientFactory builds a scion client.
type ClientFactory func() *scion.Client

func defaultFactory(machine string) backend.Backend {
	return orbstack.New(exec.RealRunner{}, machine)
}

func defaultClientFactory() *scion.Client {
	home, _ := os.UserHomeDir()
	return scion.Default(exec.RealRunner{}, home)
}

func NewRoot() *cobra.Command { return newRootWith(defaultFactory, defaultClientFactory) }

func NewRootWithBackend(factory BackendFactory) *cobra.Command {
	return newRootWith(factory, defaultClientFactory)
}

func newRootWith(bf BackendFactory, cf ClientFactory) *cobra.Command {
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration"}
	root.AddCommand(&cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println(Version) }})
	root.AddCommand(newUpCmd(bf), newDownCmd(bf), newDoctorCmd(bf), newApplyCmd(bf))
	root.AddCommand(newAgentCmd(cf), newMsgCmd(cf), newWatchCmd(cf))
	return root
}
