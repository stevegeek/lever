package cli

import (
	"os"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/registry"
	"github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

const Version = "0.0.0-dev"

// BackendFactory builds a backend for a given machine name.
type BackendFactory func(machine string) backend.Backend

// ClientFactory builds a scion client.
type ClientFactory func() *scion.Client

// defaultFactory builds the default backend via the registry. It selects the
// registry Default because the only selectable backend is orbstack and config
// validation guarantees a config never asks for anything else. When a second
// backend is implemented, thread the configured app.Backend name in here (and
// through the call sites) — TestExactlyOneSelectableBackend is the tripwire that
// fails until that happens, so this cannot silently substitute.
func defaultFactory(machine string) backend.Backend {
	b, err := registry.Select("", exec.RealRunner{}, machine)
	if err != nil {
		panic("registry: default backend must always be constructible: " + err.Error())
	}
	return b
}

func defaultClientFactory() *scion.Client {
	home, _ := os.UserHomeDir()
	return scion.Default(exec.RealRunner{}, home)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { c.Println(Version) }}
}

// NewHostRoot builds the host control-plane CLI (`lever`): provisioning only.
func NewHostRoot() *cobra.Command { return newHostRootWith(defaultFactory) }

// NewRootWithBackend is the host root with an injected backend (test seam).
func NewRootWithBackend(bf BackendFactory) *cobra.Command { return newHostRootWith(bf) }

func newHostRootWith(bf BackendFactory) *cobra.Command {
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration (host control plane)"}
	root.AddCommand(versionCmd())
	root.AddCommand(newProvisionCmd(bf), newDownCmd(bf), newDoctorCmd(bf), newApplyCmd(bf), newUpCmd(bf), newBrokerCmd(), newRevokeCmd(), newAcceptanceCmd(bf), newBackendsCmd())
	return root
}

// NewManagerRoot builds the in-jail orchestration CLI (`lever-manager`).
func NewManagerRoot() *cobra.Command { return newManagerRootWith(defaultClientFactory) }

func newManagerRootWith(cf ClientFactory) *cobra.Command {
	root := &cobra.Command{Use: "lever-manager", Short: "In-jail grove orchestration"}
	root.AddCommand(versionCmd())
	root.AddCommand(newAgentCmd(), newMsgCmd(cf), newWatchCmd(cf))
	return root
}
