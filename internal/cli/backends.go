package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
)

// newBackendsCmd lists every containment backend Lever can run and the
// guarantees it declares, straight from backend.Candidates (the same source the
// docs and config validation use). Roadmap and rejected backends are
// documentation: docs-site/_reference/backends.md. Read-only; no jail required.
func newBackendsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backends",
		Short: "List containment backends and their declared guarantees",
		Long: "Show every containment backend lever can run and the isolation guarantees\n" +
			"it declares. Roadmap and rejected backends are documented on the backends\n" +
			"reference page, not listed here.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKERNEL\tFS BOUND BY\tEGRESS AT\tFRAGILE")
			for _, c := range backend.Candidates {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\n",
					c.Name, kernelWord(c.Profile.SeparateKernel),
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
