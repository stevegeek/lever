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
	silenceUsageOnRunErrors(root)
	return root
}

// silenceUsageOnRunErrors wraps every RunE under c so a returned error (a
// broker refusal, say) is not followed by cobra's usage text, which would
// read as a flag problem. Cobra's own flag and argument checks run before
// RunE and keep it.
func silenceUsageOnRunErrors(c *cobra.Command) {
	if run := c.RunE; run != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			err := run(cmd, args)
			if err != nil {
				cmd.SilenceUsage = true
			}
			return err
		}
	}
	for _, sub := range c.Commands() {
		silenceUsageOnRunErrors(sub)
	}
}
