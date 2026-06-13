package cli

import "github.com/spf13/cobra"

const Version = "0.0.0-dev"

func NewRoot() *cobra.Command {
	root := &cobra.Command{Use: "lever", Short: "Jailed multi-agent orchestration"}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the lever version",
		Run:   func(cmd *cobra.Command, _ []string) { cmd.Println(Version) },
	})
	return root
}
