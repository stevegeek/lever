package host

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/wire"
)

// newHostMsgCmd is `lever msg send BODY --to NAME`, the operator's
// fire-and-forget note with the host user's authority, never a directive's.
// It goes through the broker's 0600 operator socket, which records it in the
// sent ledger before sending so the recipient can verify who wrote it. NAME
// resolves like attach.
func newHostMsgCmd(_ BackendFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "msg", Short: "Send a note to an agent, or list what is queued for agents that are not running"}
	cmd.AddCommand(hostMsgSend(), hostMsgList())
	return cmd
}

// errNoOperatorSocket: the broker is not running or keeps no host records.
// An unrecorded note would never verify and the agent would treat it as data,
// so none is sent.
var errNoOperatorSocket = errors.New("the broker is not running (no operator socket)")

func hostMsgSend() *cobra.Command {
	var to string
	var interrupt, wake bool
	c := &cobra.Command{
		Use:   "send BODY",
		Args:  cobra.MinimumNArgs(1),
		Short: "Send a message to the manager or a worker (--to NAME)",
		Long: "Send a note to the manager or a worker. To one that is not running (suspended or stopped) the broker\n" +
			"queues it and delivers it, in order, when it next runs; `lever msg list` shows the queue. --wake also\n" +
			"resumes a worker to deliver now, when the running caps and memory budget allow; otherwise the note\n" +
			"stays queued and the answer says why. A full queue refuses the note (exit 9, nothing was queued).",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Config comes from the CWD; the recipient comes from --to.
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
			err = operatorPost(cmd.Context(), sock, wire.PathOperatorNote,
				wire.OperatorNoteRequest{To: to, Body: strings.Join(args, " "), Interrupt: interrupt, Wake: wake}, &resp)
			if err != nil {
				var uerr *url.Error
				if errors.As(err, &uerr) {
					return fmt.Errorf("msg: %w (%v); run `lever up` (or type into the session with `lever attach`)", errNoOperatorSocket, err)
				}
				// A recipient that is not running (or has no record) is its own
				// exit code, so a host program can resume it and send again.
				var ref *operatorRefusal
				if errors.As(err, &ref) && (ref.Reason == broker.ReasonNotRunning || ref.Reason == broker.ReasonNoRecord || ref.Reason == broker.ReasonQueueFull) {
					return cli.WithExitCode(fmt.Errorf("msg: %w (nothing was sent)", ref), exitWorkerNotRunning)
				}
				return fmt.Errorf("msg: %w", err)
			}
			if !resp.Queued {
				cmd.Printf("Sent to %s (ref %s).\n", to, resp.ID)
				return nil
			}
			cmd.Printf("Queued for %s, not delivered: it is not running (phase %s). %d message(s) wait for it; they go out when it next runs (`lever msg list --to %s`).\n",
				to, cmp.Or(safeToken(resp.Phase), "unknown"), max(resp.Waiting, 0), to)
			if resp.Waking {
				cmd.Println("Waking it to deliver now.")
			} else if resp.Note != "" {
				cmd.Println(termsafe.Sanitize(resp.Note))
			}
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "", "recipient: the manager (app name) or a declared worker name (required)")
	c.Flags().BoolVar(&interrupt, "interrupt", false, "inject before the agent's next turn")
	c.Flags().BoolVar(&wake, "wake", false, "resume a worker that is not running to deliver now")
	_ = c.MarkFlagRequired("to")
	return c
}

// hostMsgList is `lever msg list [--to NAME]`: the messages the broker holds
// for agents that are not running. A body is the sender's text, which the
// table shows sanitised and cut short.
func hostMsgList() *cobra.Command {
	var to string
	var asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List the messages queued for agents that are not running (--to NAME for one)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return err
			}
			sock, err := operatorSocket(app, st)
			if err != nil {
				return cli.WithExitCode(fmt.Errorf("msg list: %w", err), exitOperatorSocketMissing)
			}
			path := broker.PathOperatorMsgQueue
			if to != "" {
				path += "?to=" + url.QueryEscape(to)
			}
			var resp broker.QueueListResponse
			if err := operatorGet(cmd.Context(), sock, path, &resp); err != nil {
				if ref, ok := operatorFailure(err, broker.PathOperatorMsgQueue); ok {
					return fmt.Errorf("msg list: %w", ref)
				}
				return fmt.Errorf("msg list: %w", err)
			}
			if asJSON {
				printJSONLine(cmd, resp)
				return nil
			}
			printQueue(cmd, resp, time.Now())
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "", "only the messages queued for this agent (the manager or a worker)")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object: off, max_messages, max_bytes, expiry, messages, unreadable")
	return c
}

// maxQueuePreview bounds a body in the table.
const maxQueuePreview = 60

func printQueue(cmd *cobra.Command, resp broker.QueueListResponse, now time.Time) {
	out := cmd.OutOrStdout()
	if resp.Off {
		fmt.Fprintln(out, "no message queue: the state directory is inside the tree, so a message to an agent that is not running is refused.")
		return
	}
	for _, name := range resp.Unreadable {
		fmt.Fprintf(out, "the queue of %s could not be read and is left out (the broker's audit log says why).\n", boundedLabel(name))
	}
	if len(resp.Messages) == 0 {
		fmt.Fprintln(out, "no messages are queued.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TO\tFROM\tQUEUED\tEXPIRES IN\tBYTES\tTEXT*")
	for _, m := range resp.Messages {
		text := boundedText(m.Body, maxQueuePreview)
		if m.Sending {
			text = "(its send was interrupted; it is dropped, not sent again) " + text
		}
		left := "expired"
		if d := m.ExpiresAt.Sub(now); d > 0 {
			left = ago(d)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", boundedLabel(m.To), boundedLabel(m.Kind), m.QueuedAt.Local().Format("2006-01-02 15:04"), left, m.Bytes, text)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "* the sender's own words. Limits per agent: %d messages and %d bytes of text each for the agents', the manager's and the operator's; a message waits at most %s.\n", resp.MaxMessages, resp.MaxBytes, resp.Expiry)
}

// boundedText is s on one line, sanitised and cut to n bytes.
func boundedText(s string, n int) string {
	s = strings.Join(strings.Fields(termsafe.Sanitize(s)), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}
