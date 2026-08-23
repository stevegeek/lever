package manager

import (
	"context"
	"errors"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/bridge"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

var errMissingEventsFile = errors.New("watch: --events-file is required")

// brokerInboxer adapts the manager's broker msg call to bridge.Inboxer,
// replacing the old in-container scion client. project="" is the manager's
// own/operator inbox (bridge always polls with unread=false, project="" — see
// bridge.go PollOnce — the full inbox, deduped by id there).
type brokerInboxer struct{ c brokerCaller }

func newBrokerInboxer(c brokerCaller) brokerInboxer { return brokerInboxer{c: c} }

func (b brokerInboxer) Inbox(ctx context.Context, unread bool, project string) ([]scion.Event, error) {
	raw, err := msgCall(ctx, b.c, wire.PathMsgList, wire.MsgListRequest{All: !unread, Worker: project})
	if err != nil {
		return nil, err
	}
	return decodeMsgEvents(raw)
}

// compile-time proof brokerInboxer satisfies bridge.Inboxer.
var _ bridge.Inboxer = brokerInboxer{}

func newWatchCmd(c brokerCaller) *cobra.Command {
	var file string
	var interval int
	cmd := &cobra.Command{Use: "watch", Short: "Bridge Scion agent events to a file the manager Monitors",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errMissingEventsFile
			}
			b := bridge.New(newBrokerInboxer(c), file)
			cmd.Printf("Watching → %s (every %ds). Ctrl-C to stop.\n", file, interval)
			return b.Run(cmd.Context(), time.Duration(interval)*time.Second)
		}}
	cmd.Flags().StringVar(&file, "events-file", "", "path to append events to (required)")
	cmd.Flags().IntVar(&interval, "interval", 5, "seconds between polls")
	return cmd
}
