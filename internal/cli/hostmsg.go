package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/scion"
)

// newHostMsgCmd is the operator's fire-and-forget note sender: `lever msg send
// BODY --to NAME`. Operator authority, no broker hop (the host owns the CA,
// jail, and config — the same trust model as `lever attach`). Strictly passive:
// it resolves the jail transport but never provisions. NAME resolves like
// attach (the app name → manager; a declared worker name → that worker).
func newHostMsgCmd(bf BackendFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "msg", Short: "Send a note to an agent (host-side, fire-and-forget)"}
	cmd.AddCommand(hostMsgSend(bf))
	return cmd
}

func hostMsgSend(bf BackendFactory) *cobra.Command {
	var to string
	var interrupt bool
	c := &cobra.Command{
		Use:   "send BODY",
		Args:  cobra.MinimumNArgs(1),
		Short: "Send a message to the manager or a worker (--to NAME)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := resolveConfigPath("")
			if err != nil {
				return err
			}
			app, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			b, err := bf(app.Backend, machineName(app.Name))
			if err != nil {
				return err
			}
			slug, project, err := attachTarget(app, b.MountDest(), to)
			if err != nil {
				return err
			}
			if err := b.ResolveRunUser(cmd.Context()); err != nil {
				return fmt.Errorf("msg: jail not up (%v) — run `lever up` first", err)
			}
			sc := scion.New(b.JailRunner(), scion.Options{HubEndpoint: "http://127.0.0.1:8080"})
			if err := sc.Message(cmd.Context(), scion.MsgOpts{
				To: "agent:" + slug, Body: strings.Join(args, " "), Interrupt: interrupt, Project: project,
			}); err != nil {
				return err
			}
			cmd.Printf("Sent to %s.\n", to)
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "", "recipient: the manager (app name) or a declared worker name (required)")
	c.Flags().BoolVar(&interrupt, "interrupt", false, "inject before the agent's next turn")
	_ = c.MarkFlagRequired("to")
	return c
}
