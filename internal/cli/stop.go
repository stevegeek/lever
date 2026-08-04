package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
)

// newStopCmd powers the jail machine off while keeping its disk, so a
// following `lever up` can resume fast (no re-apply, no reinstall). This is
// distinct from `destroy`, which deletes the machine and clears staged
// runtime state.
func newStopCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag *string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Power off the jail, keeping its disk (fast `lever up` resume)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Resolve the app (for the manager's scion slug) and, when targeting
			// the current instance (no explicit --machine), stop the host-side
			// broker too — mirroring `destroy`'s broker-stop block. UNLIKE
			// destroy, staged runtime state (bootstrap ticket, manifest) is left
			// alone: stop preserves everything for a fast resume.
			var appName string
			var state brokerctl.State // set alongside appName; valid whenever appName != ""
			if path, perr := resolveConfigPath(""); perr == nil {
				state = stateFor(path)
				if app, lerr := config.Load(path); lerr == nil {
					appName = app.Name
					if *machine == "" {
						if serr := state.StopBroker(); serr != nil {
							cmd.PrintErrf("warning: stopping broker: %v\n", serr)
						}
					}
				}
			}
			if *machine != "" {
				cmd.PrintErrln("note: --machine given; the broker is not stopped (run `lever stop` from the instance root to do that).")
			}

			m, b, err := resolveJailBackend(factory, *machine, *backendFlag)
			if err != nil {
				return err
			}

			// Best-effort checkpoint: SUSPEND the manager before power-off. The
			// conversation is durable — it lives in the agent home (persistent
			// bind-mount), and scion resume relaunches the harness with
			// `claude --continue`, restoring the session (live-proven 2026-07-04)
			// — so suspend is the verb that keeps the record resumable for the
			// next `lever up`. (`scion stop` would REMOVE the container and leave
			// a `stopped` record instead.) Gated on ResolveRunUser so a halted or
			// never-provisioned machine is still stoppable; the suspend error is
			// non-fatal — logged, not returned (the VM powers off regardless, and
			// apply's observe-first start-manager copes with whatever state
			// results) — but surfaced so a recurrence of #3 is diagnosable. The
			// timeout stops a hung scion from blocking power-off.
			if appName != "" {
				if err := b.ResolveRunUser(cmd.Context()); err == nil {
					sctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
					// state was set alongside appName above; HostScionClient's
					// HubTokenSource lets suspend authenticate against the real,
					// dev-auth-off hub with the controller PAT minted by a prior
					// `lever apply`.
					sc := brokerctl.HostScionClient(b.JailRunner(), state)
					if serr := sc.Suspend(sctx, appName, b.MountDest()); serr != nil {
						cmd.PrintErrf("warning: scion suspend failed (conversation may not resume cleanly on next up): %v\n", serr)
					}
					cancel()
				}
			}

			if err := b.Stop(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("machine %q stopped — disk preserved; run `lever up` to resume.\n", m)
			return nil
		},
	}
	machine, backendFlag = addJailTargetFlags(cmd)
	return cmd
}
