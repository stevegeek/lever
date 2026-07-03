package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

// msgCallFn is the active broker caller (seam for tests), mirroring groveCallFn.
var msgCallFn = msgCall

// msgCall is brokerCall specialized to the raw msg-endpoint response body: it
// loads bootstrap+identity exactly as groveCall does and posts JSON, returning
// the undecoded response so msg/watch can decode {"events":[...]} themselves.
func msgCall(ctx context.Context, endpoint string, body any) (json.RawMessage, error) {
	return brokerCall[json.RawMessage](ctx, endpoint, body)
}

// decodeMsgEvents unmarshals a /msg/list response body ({"events":[...]}) into
// its events. A malformed body is an error — swallowing it would make msg list
// print "Inbox empty." and the watch bridge drop events forever, silently. An
// absent/empty "events" key stays benign: it decodes to a nil slice (empty inbox).
func decodeMsgEvents(raw json.RawMessage) ([]scion.Event, error) {
	var res struct {
		Events []scion.Event `json:"events"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode /msg/list response: %w", err)
	}
	return res.Events, nil
}

func newMsgCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "msg", Short: "Send/read typed agent messages (broker-routed)"}
	cmd.AddCommand(msgSend(), msgList())
	return cmd
}

func msgSend() *cobra.Command {
	var to string
	var interrupt bool
	c := &cobra.Command{Use: "send BODY", Args: cobra.MinimumNArgs(1), Short: "Send a message to an agent/user",
		RunE: func(cmd *cobra.Command, args []string) error {
			body := strings.Join(args, " ")
			if _, err := msgCallFn(cmd.Context(), "/msg/send",
				map[string]any{"to": to, "body": body, "interrupt": interrupt}); err != nil {
				return err
			}
			cmd.Printf("Sent to %s.\n", to)
			return nil
		}}
	c.Flags().StringVar(&to, "to", "", "recipient: agent:<name> | user:<name> | <name> (required)")
	c.Flags().BoolVar(&interrupt, "interrupt", false, "inject before the agent's next turn")
	_ = c.MarkFlagRequired("to")
	return c
}

func msgList() *cobra.Command {
	var grove string
	var all bool
	c := &cobra.Command{Use: "list", Short: "Read the typed event inbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := msgCallFn(cmd.Context(), "/msg/list", map[string]any{"all": all, "grove": grove})
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
	c.Flags().StringVar(&grove, "grove", "", "manager only: read this grove's project inbox")
	c.Flags().BoolVar(&all, "all", false, "include already-read events")
	return c
}
