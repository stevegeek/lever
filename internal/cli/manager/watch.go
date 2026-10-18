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

var (
	errMissingEventsFile = errors.New("watch: --events-file is required")
	errBadInterval       = errors.New("watch: --interval must be at least 1 second")
)

// brokerInboxer adapts the broker msg call to bridge.Inboxer. project="" is the
// manager's own inbox; bridge polls the full inbox and dedupes by id.
type brokerInboxer struct{ c brokerCaller }

func newBrokerInboxer(c brokerCaller) brokerInboxer { return brokerInboxer{c: c} }

func (b brokerInboxer) Inbox(ctx context.Context, unread bool, project string) ([]scion.Event, error) {
	raw, err := msgCall(ctx, b.c, wire.PathMsgList, wire.MsgListRequest{All: !unread, Worker: project})
	if err != nil {
		return nil, err
	}
	return decodeMsgEvents(raw)
}

var _ bridge.Inboxer = brokerInboxer{}

func newWatchCmd(c brokerCaller) *cobra.Command {
	var file string
	var interval int
	cmd := &cobra.Command{Use: "watch", Short: "Bridge Scion agent events to a file the manager Monitors",
		Long: "Bridge Scion agent events to a file the manager Monitors.\n\n" +
			"A poll the broker does not answer, or answers with 5xx, 408 or 429, is logged to stderr\n" +
			"and retried, waiting up to a minute between tries. Any other error ends the command.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errMissingEventsFile
			}
			if interval < 1 {
				return errBadInterval
			}
			b := bridge.New(newBrokerInboxer(c), file)
			b.Log = cmd.ErrOrStderr()
			cmd.Printf("Watching → %s (every %ds). Ctrl-C to stop.\n", file, interval)
			return b.Run(cmd.Context(), time.Duration(interval)*time.Second)
		}}
	cmd.Flags().StringVar(&file, "events-file", "", "path to append events to (required)")
	cmd.Flags().IntVar(&interval, "interval", 5, "seconds between polls")
	return cmd
}
