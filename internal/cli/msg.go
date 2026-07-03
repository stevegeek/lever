package cli

import (
	"context"
	"encoding/json"
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
// its events. A decode error yields an empty inbox rather than propagating,
// matching the previous scion-client behavior of surfacing transport errors
// only (msgCallFn already returns transport/HTTP errors separately).
func decodeMsgEvents(raw json.RawMessage) []scion.Event {
	var res struct {
		Events []scion.Event `json:"events"`
	}
	_ = json.Unmarshal(raw, &res)
	return res.Events
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
			events := decodeMsgEvents(raw)
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
