package cli

import (
	"errors"
	"time"

	"github.com/lever-to/lever/internal/bridge"
	"github.com/spf13/cobra"
)

var errMissingEventsFile = errors.New("watch: --events-file is required")

func newWatchCmd(cf ClientFactory) *cobra.Command {
	var file string
	var interval int
	c := &cobra.Command{Use: "watch", Short: "Bridge Scion agent events to a file the manager Monitors",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errMissingEventsFile
			}
			b := bridge.New(cf(), file)
			cmd.Printf("Watching → %s (every %ds). Ctrl-C to stop.\n", file, interval)
			return b.Run(cmd.Context(), time.Duration(interval)*time.Second)
		}}
	c.Flags().StringVar(&file, "events-file", "", "path to append events to (required)")
	c.Flags().IntVar(&interval, "interval", 5, "seconds between polls")
	return c
}
