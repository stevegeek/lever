package host

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/wire"
)

// newHostMsgCmd is the operator's fire-and-forget note sender: `lever msg send
// BODY --to NAME`. Operator authority (the host user's, the same trust model as
// `lever attach`), never a signed directive's. The note goes through the
// broker's 0600 operator socket (state.State.OperatorSock), which records it in
// the sent ledger as an operator note before it sends it, so the recipient can
// verify who wrote it. NAME resolves like attach (the app name → manager; a
// declared worker name → that worker).
func newHostMsgCmd(_ BackendFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "msg", Short: "Send a note to an agent (host-side, fire-and-forget)"}
	cmd.AddCommand(hostMsgSend())
	return cmd
}

// errNoOperatorSocket is the refusal when the broker has no operator socket:
// it is not running, or it keeps no host records (the state directory is
// inside the tree). An unrecorded note would never verify, and the agent
// would treat it as data, so none is sent.
var errNoOperatorSocket = errors.New("the broker is not running (no operator socket)")

func hostMsgSend() *cobra.Command {
	var to string
	var interrupt bool
	c := &cobra.Command{
		Use:   "send BODY",
		Args:  cobra.MinimumNArgs(1),
		Short: "Send a message to the manager or a worker (--to NAME)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Config is always discovered from the CWD; the recipient NAME comes
			// from --to, never a positional config path.
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return err
			}
			// A friendly local error for a name the broker would refuse.
			if _, _, err := attachTarget(app, "", to); err != nil {
				return fmt.Errorf("msg: %w", err)
			}
			if brokerctl.StateInsideTree(app, st) {
				return fmt.Errorf("msg: the state directory %s is inside the tree, so the broker keeps no record of notes and agents could not verify one; "+
					"move the instance's state out of the tree, or type into the session with `lever attach`", st.Dir)
			}
			sock := st.OperatorSock()
			if _, err := os.Stat(sock); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("msg: %w; run `lever up` (or type into the session with `lever attach`)", errNoOperatorSocket)
			}
			var resp wire.OperatorNoteResponse
			err = httpjson.Post(cmd.Context(), udsClient(sock), udsURL+wire.PathOperatorNote,
				wire.OperatorNoteRequest{To: to, Body: strings.Join(args, " "), Interrupt: interrupt}, &resp)
			if err != nil {
				var uerr *url.Error
				if errors.As(err, &uerr) {
					return fmt.Errorf("msg: %w (%v); run `lever up` (or type into the session with `lever attach`)", errNoOperatorSocket, err)
				}
				return fmt.Errorf("msg: %w", err)
			}
			cmd.Printf("Sent to %s (ref %s).\n", to, resp.ID)
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "", "recipient: the manager (app name) or a declared worker name (required)")
	c.Flags().BoolVar(&interrupt, "interrupt", false, "inject before the agent's next turn")
	_ = c.MarkFlagRequired("to")
	return c
}
