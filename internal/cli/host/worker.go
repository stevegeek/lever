package host

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli"
)

// newWorkerCmd is the host-side worker admin command. Every verb goes
// through the broker's operator socket.
func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "Manage worker agents host-side"}
	cmd.AddCommand(newWorkerPurgeCmd(), newWorkerListCmd(), newWorkerStartCmd(), newWorkerResumeCmd(),
		newWorkerRecycleCmd(), newWorkerNewSessionCmd(), newWorkerSuspendCmd(), newWorkerStopCmd(), newWorkerWatchCmd())
	return cmd
}

// workerPurgeResult is `lever worker purge --json`'s one line on stdout.
type workerPurgeResult struct {
	workerVerbResult
	Was           string `json:"was,omitempty"`
	TicketLeft    bool   `json:"ticket_left,omitempty"`
	CutoffUnsaved bool   `json:"cutoff_unsaved,omitempty"`
}

// newWorkerPurgeCmd deletes a worker's scion record and staged ticket so it
// can be dispatched fresh with a new task (scion pins the task at creation).
// The broker does it under the worker's lock and supersedes the certificates
// issued to the worker. It never deletes the worker's workspace, its work
// product. Destructive, so it requires --force. --orphan purges a hub record
// that is neither the manager nor a declared worker.
func newWorkerPurgeCmd() *cobra.Command {
	var force, orphan, asJSON bool
	c := &cobra.Command{
		Use:   "purge NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a worker's scion record + staged ticket (keeps its work product)",
		Long: "Delete a declared worker's scion record and staged ticket through the broker, under the worker's lock,\n" +
			"so it can run a new task; certificates issued to it before the purge are refused from then on. Its work\n" +
			"product in the workspace is kept. --orphan deletes a hub record of this instance that is neither the\n" +
			"manager nor a declared worker (`lever worker list` and `lever doctor` name them).\n" +
			"Exit codes and --json reasons: see the CLI reference.",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			res := workerPurgeResult{workerVerbResult: workerVerbResult{Worker: name}}
			fail := func(err error, reason string, code int) error {
				if asJSON {
					res.Reason, res.Error, res.Exit = reason, err.Error(), code
					printJSONLine(cmd, res)
				}
				return cli.WithExitCode(fmt.Errorf("worker purge %s: %w", name, err), code)
			}
			what := "the worker's scion record and staged ticket so it can run a new task (its work product in the workspace is KEPT)"
			if orphan {
				what = "the orphan hub record " + name
			}
			if !force {
				return fail(fmt.Errorf("this deletes %s; re-run with --force to proceed", what), reasonNoForce, exitWorkerRefused)
			}
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return fail(err, "", 1)
			}
			// Which names are workers is the running broker's call, not this
			// lever.yaml's: the two differ until `lever reload`.
			sock, err := operatorSocket(app, st)
			if err != nil {
				return fail(err, reasonNoOperatorSocket, exitOperatorSocketMissing)
			}
			var resp broker.WorkerPurgeResponse
			err = operatorPost(cmd.Context(), sock, broker.PathOperatorWorkerPurge, broker.WorkerPurgeRequest{Worker: name, Orphan: orphan}, &resp)
			if ref, ok := operatorFailure(err, broker.PathOperatorWorkerPurge); ok {
				return fail(ref, ref.Reason, workerVerbExit(ref.Reason))
			} else if err != nil {
				return fail(err, "", 1)
			}
			res.OK, res.Was, res.TicketLeft, res.CutoffUnsaved = true, safeToken(resp.Was), resp.TicketLeft, resp.CutoffUnsaved
			if asJSON {
				printJSONLine(cmd, res)
				return nil
			}
			if resp.TicketLeft {
				cmd.PrintErrf("lever: warning: the staged ticket of %q was not removed (see the broker log); its next start stages a fresh one\n", name)
			}
			if resp.CutoffUnsaved {
				cmd.PrintErrf("lever: warning: the certificate cutoff for %q was not saved (see the broker log): it holds until the broker restarts\n", name)
			}
			deleted := "scion record deleted (it was " + res.Was + ")"
			if res.Was == broker.PhaseNone {
				deleted = "no scion record to delete"
			}
			if orphan {
				cmd.Printf("orphan %q purged: %s.\n", name, deleted)
				return nil
			}
			dir := name
			for _, w := range app.Workers {
				if w.Name == name {
					dir = app.WorkerDir(w)
				}
			}
			cmd.Printf("worker %q purged: %s; certificates issued to it before now are refused; work product in %s kept.\n", name, deleted, dir)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "confirm the destructive purge (required)")
	c.Flags().BoolVar(&orphan, "orphan", false, "purge a hub record that is neither the manager nor a declared worker")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object on stdout: worker, ok, was, reason, error, exit")
	return c
}

// reasonNoForce is purge's own refusal without --force.
const reasonNoForce = "no-force"
