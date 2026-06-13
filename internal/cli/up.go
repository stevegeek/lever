package cli

import (
	"github.com/lever-to/lever/internal/backend"
	"github.com/spf13/cobra"
)

func newUpCmd(factory BackendFactory) *cobra.Command {
	var machine, tree string
	var allow []int
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Provision the jail (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b := factory(machine)
			if err := b.EnsureUp(cmd.Context(), backend.Config{MachineName: machine, ProjectTree: tree, AllowedPorts: allow}); err != nil {
				return err
			}
			cmd.Printf("jail %q up; DOCKER_HOST=%s; alias=%s\n", machine, b.DockerHost(), b.HostToolAlias())
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "lever-jail", "jail machine name")
	cmd.Flags().StringVar(&tree, "tree", "", "host project tree to mount (required)")
	cmd.Flags().IntSliceVar(&allow, "allow-port", nil, "host tool port to allowlist (repeatable)")
	_ = cmd.MarkFlagRequired("tree")
	return cmd
}
