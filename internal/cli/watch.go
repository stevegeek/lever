package cli

import (
	"context"
	"errors"
	"time"

	"github.com/lever-to/lever/internal/bridge"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

var errMissingEventsFile = errors.New("watch: --events-file is required")

// brokerInboxer adapts the manager's broker msg-call seam (msgCallFn) to
// bridge.Inboxer, replacing the old in-container scion client. project="" is
// the manager's own/operator inbox (bridge always polls with unread=false,
// project="" — see bridge.go PollOnce — the full inbox, deduped by id there).
type brokerInboxer struct{}

func newBrokerInboxer() brokerInboxer { return brokerInboxer{} }

func (brokerInboxer) Inbox(ctx context.Context, unread bool, project string) ([]scion.Event, error) {
	raw, err := msgCallFn(ctx, "/msg/list", map[string]any{"all": !unread, "grove": project})
	if err != nil {
		return nil, err
	}
	return decodeMsgEvents(raw), nil
}

// compile-time proof brokerInboxer satisfies bridge.Inboxer.
var _ bridge.Inboxer = brokerInboxer{}

func newWatchCmd() *cobra.Command {
	var file string
	var interval int
	c := &cobra.Command{Use: "watch", Short: "Bridge Scion agent events to a file the manager Monitors",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errMissingEventsFile
			}
			b := bridge.New(newBrokerInboxer(), file)
			cmd.Printf("Watching → %s (every %ds). Ctrl-C to stop.\n", file, interval)
			return b.Run(cmd.Context(), time.Duration(interval)*time.Second)
		}}
	c.Flags().StringVar(&file, "events-file", "", "path to append events to (required)")
	c.Flags().IntVar(&interval, "interval", 5, "seconds between polls")
	return c
}
