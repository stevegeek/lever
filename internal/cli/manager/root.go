// Package manager implements the `lever-manager` binary, which runs inside
// the agent container. It holds only the container's own identity and
// bootstrap and talks to the broker over the wire contract; it must never
// link host-side code (backends, apply, brokerctl, remoteproxy, provision).
package manager

import (
	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/cli"
)

// NewRoot builds the in-jail CLI, talking to the broker over mTLS from the
// container's bootstrap and identity.
func NewRoot() *cobra.Command { return newRoot(newMTLSCaller()) }

func newRoot(c brokerCaller) *cobra.Command {
	root := &cobra.Command{Use: "lever-manager", Short: "In-jail worker orchestration",
		Version: agent.BuildLine("lever-manager", cli.Version)}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(newVersionCmd(c), newAgentCmd(c), newMsgCmd(c), newWatchCmd(c))
	return root
}
