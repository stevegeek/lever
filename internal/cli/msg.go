package cli

import (
	"strings"

	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

func newMsgCmd(cf ClientFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "msg", Short: "Send/read typed agent messages"}
	cmd.AddCommand(msgSend(cf), msgList(cf))
	return cmd
}

func msgSend(cf ClientFactory) *cobra.Command {
	var to, project string
	var interrupt bool
	c := &cobra.Command{Use: "send BODY", Args: cobra.MinimumNArgs(1), Short: "Send a message to an agent/user",
		RunE: func(cmd *cobra.Command, args []string) error {
			body := strings.Join(args, " ")
			if err := cf().Message(cmd.Context(), scion.MsgOpts{To: to, Body: body, Interrupt: interrupt, Project: project}); err != nil {
				return err
			}
			cmd.Printf("Sent to %s.\n", to)
			return nil
		}}
	c.Flags().StringVar(&to, "to", "", "recipient: agent:<name> | user:<name> | <name> (required)")
	c.Flags().BoolVar(&interrupt, "interrupt", false, "inject before the agent's next turn")
	projectFlagVar(c, &project)
	_ = c.MarkFlagRequired("to")
	return c
}

func msgList(cf ClientFactory) *cobra.Command {
	var project string
	var all bool
	c := &cobra.Command{Use: "list", Short: "Read the typed event inbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			events, err := cf().Inbox(cmd.Context(), !all, project)
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
	projectFlagVar(c, &project)
	c.Flags().BoolVar(&all, "all", false, "include already-read events")
	return c
}
