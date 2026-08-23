// Package manager implements the `lever-manager` binary, which runs inside
// the agent container. It holds only the container's own identity and
// bootstrap and talks to the broker over the wire contract; it must never
// link host-side code (backends, apply, brokerctl, remoteproxy, provision).
package manager

import (
	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/cli"
)

// NewRoot builds the in-jail orchestration CLI (`lever-manager`).
func NewRoot() *cobra.Command {
	root := &cobra.Command{Use: "lever-manager", Short: "In-jail worker orchestration"}
	root.AddCommand(cli.VersionCmd())
	root.AddCommand(newAgentCmd(), newMsgCmd(), newWatchCmd())
	return root
}
