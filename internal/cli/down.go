package cli

import "github.com/spf13/cobra"

func newDownCmd(factory BackendFactory) *cobra.Command {
	var machine string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Tear down the jail",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := factory(machine).Teardown(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("jail %q down\n", machine)
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "lever-jail", "jail machine name")
	return cmd
}
