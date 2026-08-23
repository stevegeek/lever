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

// msgCall is brokerCall specialized to the raw msg-endpoint response body,
// returning the undecoded response so msg/watch can decode {"events":[...]}
// themselves.
func msgCall(ctx context.Context, c brokerCaller, endpoint string, body any) (json.RawMessage, error) {
	return brokerCall[json.RawMessage](ctx, c, endpoint, body)
}

// decodeMsgEvents unmarshals a /msg/list response body ({"events":[...]}) into
// its events. A malformed body is an error — swallowing it would make msg list
// print "Inbox empty." and the watch bridge drop events forever, silently. An
// absent/empty "events" key stays benign: it decodes to a nil slice (empty inbox).
func decodeMsgEvents(raw json.RawMessage) ([]scion.Event, error) {
	var res wire.MsgListResponse[scion.Event]
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode /msg/list response: %w", err)
	}
	return res.Events, nil
}

func newMsgCmd(c brokerCaller) *cobra.Command {
	cmd := &cobra.Command{Use: "msg", Short: "Send/read typed agent messages (broker-routed)"}
	cmd.AddCommand(msgSend(c), msgList(c))
	return cmd
}

func msgSend(c brokerCaller) *cobra.Command {
	var to string
	var interrupt bool
	cmd := &cobra.Command{Use: "send BODY", Args: cobra.MinimumNArgs(1), Short: "Send a message to an agent/user",
		RunE: func(cmd *cobra.Command, args []string) error {
			body := strings.Join(args, " ")
			if _, err := msgCall(cmd.Context(), c, wire.PathMsgSend,
				wire.MsgSendRequest{To: to, Body: body, Interrupt: interrupt}); err != nil {
				return err
			}
			cmd.Printf("Sent to %s.\n", to)
			return nil
		}}
	cmd.Flags().StringVar(&to, "to", "", "recipient: agent:<name> | user:<name> | <name> (required)")
	cmd.Flags().BoolVar(&interrupt, "interrupt", false, "inject before the agent's next turn")
	_ = cmd.MarkFlagRequired("to")
	return cmd
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
