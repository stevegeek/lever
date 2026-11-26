package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// msgCall returns the undecoded body so msg and watch can decode it themselves.
func msgCall(ctx context.Context, c brokerCaller, endpoint string, body any) (json.RawMessage, error) {
	return brokerCall[json.RawMessage](ctx, c, endpoint, body)
}

// decodeMsgEvents fails on a malformed body: swallowing it would make msg list
// print "Inbox empty." and the watch bridge drop events silently. An absent
// "events" key is an empty inbox.
func decodeMsgEvents(raw json.RawMessage) ([]scion.Event, error) {
	var res wire.MsgListResponse[scion.Event]
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode /msg/list response: %w", err)
	}
	// The broker already marks each event; marking again is a no-op and covers an
	// older broker.
	for i, e := range res.Events {
		res.Events[i] = scion.WorkerReported(e)
	}
	return res.Events, nil
}

func newMsgCmd(c brokerCaller) *cobra.Command {
	cmd := &cobra.Command{Use: "msg", Short: "Send/read typed agent messages (broker-routed)"}
	cmd.AddCommand(msgSend(c), msgList(c), msgRecipients(c))
	return cmd
}

// msgAddressHelp is the address vocabulary of `msg send --to`. The broker
// decides (identity-derived, default-deny); `msg recipients` asks it.
const msgAddressHelp = "Address forms for --to:\n" +
	"  user:manager          the manager, from any agent (the form a worker uses)\n" +
	"  agent:<name> | <name> a declared worker, or the manager by its own agent name\n\n" +
	"The manager may message any declared worker. A worker may message the manager, and\n" +
	"another worker only when the instance allows worker-to-worker messaging. No other\n" +
	"user:<name> is routable.\n" +
	"`lever-manager msg recipients` lists the addresses this agent may send to; a refused\n" +
	"send names them too."

func msgSend(c brokerCaller) *cobra.Command {
	var to string
	var interrupt, wake bool
	cmd := &cobra.Command{Use: "send BODY", Args: cobra.MinimumNArgs(1), Short: "Send a message to an agent (queued while it is not running)",
		Long: "Send a message to an agent through the broker. To an agent that is not running (suspended or\n" +
			"stopped) the broker queues it and delivers it, in order, when the agent next runs; the answer says\n" +
			"so and how many wait. --wake also resumes the agent to deliver now, when you may resume it and\n" +
			"the running caps and memory budget allow; otherwise it stays queued and the answer says why.\n\n" + msgAddressHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			body := strings.Join(args, " ")
			res, err := brokerCall[wire.MsgSendResponse](cmd.Context(), c, wire.PathMsgSend,
				wire.MsgSendRequest{To: to, Body: body, Interrupt: interrupt, Wake: wake})
			if err != nil {
				return err
			}
			if !res.Queued {
				cmd.Printf("Sent to %s.\n", to)
				return nil
			}
			cmd.Printf("Queued for %s, not delivered: it is not running (phase %s). %d message(s) wait for it; they go out when it next runs.\n",
				to, res.Phase, res.Waiting)
			if res.Waking {
				cmd.Println("Waking it to deliver now.")
			} else if res.Note != "" {
				cmd.Println(res.Note)
			}
			return nil
		}}
	cmd.Flags().StringVar(&to, "to", "", "recipient: user:manager | agent:<name> | <name> (required; \"msg recipients\" lists yours)")
	cmd.Flags().BoolVar(&interrupt, "interrupt", false, "inject before the agent's next turn")
	cmd.Flags().BoolVar(&wake, "wake", false, "resume a recipient that is not running to deliver now (only one you may resume)")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func msgRecipients(c brokerCaller) *cobra.Command {
	return &cobra.Command{Use: "recipients", Args: cobra.NoArgs, Short: "List the addresses this agent may send to",
		Long: "List the addresses this agent may pass to `msg send --to`, as the broker's policy has them.\n\n" + msgAddressHelp,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := brokerCall[wire.MsgRecipientsResponse](cmd.Context(), c, wire.PathMsgRecipients, struct{}{})
			if err != nil {
				return err
			}
			for _, addr := range res.Recipients {
				cmd.Println(addr)
			}
			return nil
		}}
}

func msgList(c brokerCaller) *cobra.Command {
	var worker string
	var all bool
	cmd := &cobra.Command{Use: "list", Short: "Read the typed event inbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := msgCall(cmd.Context(), c, wire.PathMsgList, wire.MsgListRequest{All: all, Worker: worker})
			if err != nil {
				return err
			}
			events, err := decodeMsgEvents(raw)
			if err != nil {
				return err
			}
			if len(events) == 0 {
				cmd.Println("Inbox empty.")
				return nil
			}
			for _, e := range events {
				status, _ := e["status"].(string)
				msg, _ := e["message"].(string)
				cmd.Printf("  [%s] %s %s\n", e.ID(), status, msg)
			}
			return nil
		}}
	cmd.Flags().StringVar(&worker, "worker", "", "manager only: read this worker's project inbox")
	cmd.Flags().BoolVar(&all, "all", false, "include already-read events")
	return cmd
}
