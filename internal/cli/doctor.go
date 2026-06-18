package cli

import "github.com/spf13/cobra"

func newDoctorCmd(factory BackendFactory) *cobra.Command {
	var machine string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Show the backend containment profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := machineFromFlagOrConfig(machine)
			if err != nil {
				return err
			}
			cmd.Println(factory(m).Profile().Summary())
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	return cmd
}
