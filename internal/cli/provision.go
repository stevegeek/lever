package cli

import (
	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
)

func newProvisionCmd(factory BackendFactory) *cobra.Command {
	var machine, tree, backendName string
	var allow []int
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Provision the jail only (low-level; idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := factory(backendName, machine)
			if err != nil {
				return err
			}
			if err := b.EnsureUp(cmd.Context(), backend.Config{MachineName: machine, ProjectTree: tree, AllowedPorts: allow}); err != nil {
				return err
			}
			cmd.Printf("jail %q up; DOCKER_HOST=%s; alias=%s\n", machine, b.DockerHost(), b.HostToolAlias())
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "lever-jail", "jail machine name")
	cmd.Flags().StringVar(&tree, "tree", "", "host project tree to mount (required)")
	cmd.Flags().StringVar(&backendName, "backend", registry.Default, "containment backend")
	cmd.Flags().IntSliceVar(&allow, "allow-port", nil, "host tool port to allowlist (repeatable)")
	_ = cmd.MarkFlagRequired("tree")
	return cmd
}
