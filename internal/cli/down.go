package cli

import "github.com/spf13/cobra"

func newDownCmd(factory BackendFactory) *cobra.Command {
	var machine string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Tear down the jail",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := machineFromFlagOrConfig(machine)
			if err != nil {
				return err
			}
			if err := factory(m).Teardown(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("jail %q down\n", m)
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	return cmd
}
