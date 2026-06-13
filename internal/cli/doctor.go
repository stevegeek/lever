package cli

import "github.com/spf13/cobra"

func newDoctorCmd(factory BackendFactory) *cobra.Command {
	var machine string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Show the backend containment profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println(factory(machine).Profile().Summary())
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "lever-jail", "jail machine name")
	return cmd
}
