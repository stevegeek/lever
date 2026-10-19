package manager

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/wire"
)

const versionTimeout = 15 * time.Second

// newVersionCmd is `lever-manager version`: this binary's build, then the
// host lever's (GET /version) and whether the two match. `--version` on the
// root prints the first line only, without asking the broker.
func newVersionCmd(c brokerCaller) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print this image's lever build and Jail ABI, and the host lever's",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println(agent.BuildLine("lever-manager", cli.Version))
			ctx, cancel := context.WithTimeout(cmd.Context(), versionTimeout)
			defer cancel()
			var host wire.VersionResponse
			err := c.Call(ctx, wire.PathVersion, nil, &host)
			report, err := agent.VersionReport(cli.Version, host, err, os.Getenv(bootstrapEnv) != "")
			if err != nil {
				return fmt.Errorf("ask the broker for the host version: %w", err)
			}
			cmd.Println(report)
			return nil
		},
	}
}
