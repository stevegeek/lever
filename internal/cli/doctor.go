package cli

import "github.com/spf13/cobra"

func newDoctorCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Show the backend containment profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := machineFromFlagOrConfig(machine)
			if err != nil {
				return err
			}
			b, err := factory(backendFromFlagOrConfig(backendFlag), m)
			if err != nil {
				return err
			}
			cmd.Println(b.Profile().Summary())
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "", "jail machine name (default: lever-<name> from config)")
	cmd.Flags().StringVar(&backendFlag, "backend", "", "containment backend (default: config's backend, else the registry default)")
	return cmd
}
