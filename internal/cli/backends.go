package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/lever-to/lever/internal/backend"
	"github.com/spf13/cobra"
)

// newBackendsCmd lists every containment backend Lever knows about and the
// guarantees it declares, straight from backend.Candidates (the same source the
// docs and config validation use). Read-only; no jail required.
func newBackendsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backends",
		Short: "List containment backends and their declared guarantees",
		Long: "Show every containment backend and the isolation guarantees it declares.\n" +
			"Only 'implemented' backends can be selected by a config; 'planned' and\n" +
			"'experimental' entries state their TARGET guarantees so the roadmap is visible.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tKERNEL\tFS BOUND BY\tEGRESS AT\tFRAGILE")
			for _, c := range backend.Candidates {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\n",
					c.Name, c.Status, kernelWord(c.Profile.SeparateKernel),
					c.Profile.FSBoundedBy, c.Profile.EgressEnforcedAt, c.Profile.VersionFragile)
			}
			w.Flush()
		},
	}
}

func kernelWord(separate bool) string {
	if separate {
		return "separate"
	}
	return "shared"
}
