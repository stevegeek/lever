package host

import (
	"context"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
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
			ia := loadInstanceApp()
			var appName string
			var st state.State // set alongside appName; valid whenever appName != ""
			if ia.app != nil {
				st = stateFor(ia.path)
				appName = ia.app.Name
				if *machine == "" {
					stopHostDaemons(cmd, st)
				}
			}
			if *machine != "" {
				cmd.PrintErrln("note: --machine given; the broker is not stopped (run `lever stop` from the instance root to do that).")
			}

			m, b, err := resolveJailBackendFor(factory, ia, *machine, *backendFlag)
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
					// st was set alongside appName above; HostScionClient's
					// HubTokenSource lets suspend authenticate against the real,
					// dev-auth-off hub with the controller PAT minted by a prior
					// `lever apply`.
					// Empty agent role: this client only calls List and Suspend,
					// and only start emits --role.
					sc := brokerctl.HostScionClient(b.JailRunner(), st, "")
					// The heal has its own budget, ahead of the suspend's, so a
					// slow one cannot cost the manager its suspend.
					hctx, hcancel := context.WithTimeout(cmd.Context(), stopHealBudget)
					healStoppedManager(hctx, cmd, sc, stopSessionProbe(b.JailRunner()), stopRoleVerifier(b.JailRunner(), st, sc), appName, b.MountDest())
					hcancel()
					sctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
					if serr := sc.Suspend(sctx, appName, b.MountDest()); serr != nil {
						cmd.PrintErrf("warning: scion suspend failed (conversation may not resume cleanly on next up): %s\n", termsafe.Sanitize(scion.ErrSummary(serr)))
					}
					cancel()
					// After the manager, under its own budget, so a slow worker
					// pass cannot cost the manager its suspend.
					suspendRunningWorkers(cmd, sc, ia.app, b.MountDest())
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

// healStoppedManager runs before the manager's suspend. A manager whose hub
// phase reads stopped while its claude still runs (another claude process in
// the container fired the shared SessionEnd hook) cannot be suspended, and
// scion resumes a stopped record with a FRESH session, so the next `lever up`
// would lose the conversation's continuity. Reporting the session running
// first lets the suspend keep it (apply.HealAgentSession). Best-effort, like
// the suspend: a failure is a warning.
//
// The session report travels on the agent's hub token, and a manager left
// stopped this way has often also slept past its token's expiry: the heal
// resets an expired token first (scion reset-auth, behind verifyRole, the
// pre-role record guard), then reports. verifyRole nil ⇒ no reset.
func healStoppedManager(ctx context.Context, cmd *cobra.Command, sc *scion.Client, probe apply.AgentSessionProbe, verifyRole func(ctx context.Context, project, agent string) error, name, project string) {
	agents, err := sc.List(ctx, project)
	if err != nil {
		return // the suspend that follows reports a hub that cannot answer
	}
	rec := scion.FindAgent(agents, name)
	if rec == nil || rec.Phase != scion.PhaseStopped {
		return
	}
	apply.HealAgentSession(ctx, apply.SessionHealer{Scion: sc, Probe: probe, VerifyRole: verifyRole, Log: func(format string, args ...any) {
		logLine(cmd.ErrOrStderr(), "lever stop: "+format, args...)
	}}, project, rec)
}

// stopRoleVerifier is the pre-role record guard for the stop heal's token
// reset: the hub read apply's guard makes (hubapi.VerifyAgentRole), through
// the jail with the controller PAT.
func stopRoleVerifier(jr proc.Runner, st state.State, sc *scion.Client) func(ctx context.Context, project, agent string) error {
	hc := &hubapi.Client{T: hubJailTransport(jr, st)}
	return func(ctx context.Context, project, agent string) error {
		return hubapi.VerifyAgentRole(ctx, sc.RolesSupported, hc, project, agent)
	}
}

// stopHealBudget bounds healStoppedManager: a list, a token read, a harness
// probe, the session report and its re-read. A var so a test can shrink it.
var stopHealBudget = 20 * time.Second

// stopSessionProbe builds the in-container probe the stop heal uses; a test
// seam.
var stopSessionProbe = func(r proc.Runner) apply.AgentSessionProbe { return jail.AgentProbe{R: r} }

// The worker pass of `lever stop`: one list, then up to
// workerSuspendParallel suspends at a time, each under its own
// workerSuspendTimeout, so one hung worker cannot cost the others their
// suspend. workerListTimeout bounds the list.
const (
	workerListTimeout     = 15 * time.Second
	workerSuspendTimeout  = 20 * time.Second
	workerSuspendParallel = 4
)

// workerSuspender is the part of *scion.Client the worker pass uses (a
// test seam).
type workerSuspender interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Suspend(ctx context.Context, worker, project string) error
}

// suspendRunningWorkers suspends every configured worker the hub shows
// running, or on its way up, before the power-off. A worker left running
// across the power-off comes back with hub phase error and a container that
// was created but never started; a plain resume of that record answers 409,
// so it needed `lever worker purge`. A suspended record resumes cleanly.
//
// Best-effort, like the manager's suspend: every failure is a warning and the
// caller powers off regardless. No worker is stopped or deleted here.
func suspendRunningWorkers(cmd *cobra.Command, sc workerSuspender, app *config.App, project string) {
	if len(app.Workers) == 0 {
		return
	}
	lctx, cancel := context.WithTimeout(cmd.Context(), workerListTimeout)
	agents, err := sc.List(lctx, project)
	cancel()
	if err != nil {
		cmd.PrintErrf("warning: listing agents failed, no worker was suspended (a running worker may not resume cleanly on next up): %s\n", termsafe.Sanitize(scion.ErrSummary(err)))
		return
	}
	var names []string
	for _, wk := range app.Workers {
		if a := scion.FindAgent(agents, wk.Name); a != nil && suspendBeforeStop(a.Phase) {
			names = append(names, wk.Name)
		}
	}
	errs := suspendWorkers(cmd.Context(), sc, names, project, workerSuspendParallel, workerSuspendTimeout)
	// Reported in config order once every suspend has returned, whatever
	// order they finished in.
	for i, name := range names {
		if errs[i] != nil {
			cmd.PrintErrf("warning: scion suspend of worker %q failed (it may not resume cleanly on next up): %s\n", name, termsafe.Sanitize(scion.ErrSummary(errs[i])))
			continue
		}
		cmd.Printf("worker %q suspended — it stays suspended after `lever up`; resume it from the manager (`lever-manager agent resume %s`).\n", name, name)
	}
}

// suspendWorkers suspends names with at most parallel calls in flight, each
// under its own timeout, and returns each call's error at its name's index.
func suspendWorkers(ctx context.Context, sc workerSuspender, names []string, project string, parallel int, timeout time.Duration) []error {
	errs := make([]error, len(names))
	sem := make(chan struct{}, max(1, parallel))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			sctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			errs[i] = sc.Suspend(sctx, name, project)
		}()
	}
	wg.Wait()
	return errs
}

// suspendBeforeStop reports a phase in which a worker's container is up or
// coming up: running, and the two interim phases that lead to it ("resumed"
// is scion's own, outside the hub enum). Suspended, stopped and error records
// have no live container to lose.
func suspendBeforeStop(phase string) bool {
	switch phase {
	case scion.PhaseRunning, "resumed", "starting":
		return true
	}
	return false
}

// stopHostDaemons stops the host-side daemons tied to the current instance:
// the broker, then the remote-access proxy. Both are idempotent, so this is
// safe even when no proxy was ever started (no remote.pid ⇒ no-op). Failures
// are warnings: the VM-side teardown that follows must still run.
func stopHostDaemons(cmd *cobra.Command, st state.State) {
	if err := brokerctl.StopBroker(st); err != nil {
		cmd.PrintErrf("warning: stopping broker: %v\n", err)
	}
	if err := brokerctl.StopRemoteProxy(st); err != nil {
		cmd.PrintErrf("warning: stopping remote proxy: %v\n", err)
	}
}
