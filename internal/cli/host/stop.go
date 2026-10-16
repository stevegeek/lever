package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/treebackup"
)

// errNoHub labels a keep-vm snapshot whose guest could not be synced.
var errNoHub = errors.New("the jail did not answer")

// newStopCmd powers the jail machine off but keeps its disk, so `lever up`
// can resume without a re-apply. Unlike `destroy`, it keeps the machine and
// staged runtime state.
//
// With --keep-vm the agents are suspended and the hub and host daemons stopped,
// but the VM stays running (and keeps its memory): the next `lever up` starts
// only the hub and the manager.
func newStopCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag *string
	var keepVM, noTreeBackup bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Power off the jail, keeping its disk (fast `lever up` resume)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --keep-vm needs this instance's broker and agents, so it refuses a --machine
			// target rather than leave a foreign jail half stopped.
			if keepVM && *machine != "" {
				return usageError{errors.New("stop: --keep-vm acts on the current instance; run it from the instance root without --machine")}
			}
			// When targeting the current instance, stop the host broker too. Unlike
			// destroy, staged runtime state (bootstrap ticket, manifest) is kept.
			ia := loadInstanceApp()
			if keepVM && ia.app == nil {
				return usageError{errors.New("stop: --keep-vm needs an instance: run it from the instance root (no lever.yaml found)")}
			}
			var appName string
			var st state.State // set alongside appName; valid whenever appName != ""
			if ia.app != nil {
				st = stateFor(ia.path)
				appName = ia.app.Name
				unlock, err := lockLifecycle(ia.app, st, "lever stop")
				if err != nil && *machine == "" && brokerHoldsLock(st) {
					// The broker heals the manager under the lock, and the manager can make
					// a heal last minutes, again after every cooldown: the power-off must
					// not wait on it. Stopping the broker ends its hold.
					cmd.PrintErrln("lever: the broker is changing the manager (a certificate heal or a token reset); stopping the broker first.")
					stopHostDaemons(cmd, st)
					unlock, err = lockLifecycle(ia.app, st, "lever stop")
				}
				if err != nil {
					return err
				}
				defer unlock()
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

			// Best-effort: suspend the manager before power-off. Resume relaunches
			// `claude --continue` from the persistent agent home, so suspend keeps the
			// record resumable; `scion stop` would remove the container. Gated on
			// ResolveRunUser so a halted machine is still stoppable. A suspend error only
			// warns (the VM powers off regardless). The timeout keeps a hung scion from
			// blocking power-off.
			//
			// hub is set once the jail answers: --keep-vm then has a hub to stop.
			var hub hubStopper
			// suspended stays true only while every suspend took: --keep-vm must not claim
			// otherwise, since those containers go on running in the VM.
			suspended := true
			if appName != "" {
				if err := b.ResolveRunUser(cmd.Context()); err == nil {
					// HubTokenSource authenticates with the controller PAT. Empty agent
					// role: only start emits --role.
					sc := brokerctl.HostScionClient(b.JailRunner(), st, "")
					hub = sc
					if serr := parkManager(cmd.Context(), cmd, ia.app, sc, stopSessionProbe(b.JailRunner()), stopRoleVerifier(b.JailRunner(), st, sc), agentRevoked(ia.app, st), b.MountDest()); serr != nil {
						if !reportManagerSuspendError(cmd, sc, appName, b.MountDest(), serr) {
							suspended = false
						}
					}
					// After the manager, under its own budget, so a slow worker pass cannot cost
					// the manager its suspend.
					if !suspendRunningWorkers(cmd, sc, ia.app, b.MountDest()) {
						suspended = false
					}
				}
			}

			// A tree snapshot only for this instance, never for a --machine target.
			snapshotTree := ia.app != nil && *machine == "" && !noTreeBackup
			if keepVM {
				err := stopKeepVM(cmd, hub, m, suspended)
				if snapshotTree {
					var syncErr error = errNoHub
					if hub != nil {
						syncErr = guestSync(cmd.Context(), b)
					}
					treeSnapshotAt(cmd, ia.app, st, treebackup.TriggerKeepVM, quiescentKeepVM(suspended, syncErr))
				}
				return err
			}
			if err := b.Stop(cmd.Context()); err != nil {
				return err
			}
			cmd.Printf("machine %q stopped — disk preserved; run `lever up` to resume.\n", m)
			if snapshotTree {
				treeSnapshotAt(cmd, ia.app, st, treebackup.TriggerStop, quiescentAfterStop(b))
			}
			return nil
		},
	}
	machine, backendFlag = addJailTargetFlags(cmd)
	cmd.Flags().BoolVar(&noTreeBackup, "no-tree-backup", false, "do not take the tree snapshot this stop would take (tree_backup.at_stop)")
	cmd.Flags().BoolVar(&keepVM, "keep-vm", false, "suspend the agents and stop the hub and the broker, but leave the VM running (it keeps its memory on the host); the next `lever up` skips the VM boot")
	return cmd
}

// hubStopTimeout bounds --keep-vm's hub stop and the wait for the daemon to
// exit. A var so a test can shrink it.
var hubStopTimeout = 30 * time.Second

type hubStopper interface {
	ServerStop(ctx context.Context) error
}

// stopKeepVM stops the jail's hub and leaves the VM running. hub is nil when
// the jail did not answer: nothing was suspended, and a VM that is up after
// all still runs its agents, so that is an error naming plain `lever stop`. A
// failed hub stop only warns (the next `lever up` starts or reuses it). A
// suspend that did not take is an error: that container keeps running.
func stopKeepVM(cmd *cobra.Command, hub hubStopper, machine string, suspended bool) error {
	if hub == nil {
		return fmt.Errorf("stop --keep-vm: machine %q did not answer, so no agent was suspended; if it is running, `lever stop` powers it off (and `lever up` starts it again)", machine)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), hubStopTimeout)
	err := hub.ServerStop(ctx)
	cancel()
	if err != nil {
		cmd.PrintErrf("lever: warning: stopping the jail's hub failed (the next `lever up` reuses it): %s\n", termsafe.Sanitize(scion.ErrSummary(err)))
	}
	if !suspended {
		return fmt.Errorf("stop --keep-vm: machine %q left running with the hub and broker stopped, but not every agent was suspended (warnings above); its container goes on running in the VM. `lever stop` powers the VM off", machine)
	}
	cmd.Printf("machine %q left running — agents suspended, hub and broker stopped; the VM keeps its memory on the host. Run `lever up` to resume, or `lever stop` to power it off.\n", machine)
	return nil
}

// reportManagerSuspendError reports a failed manager suspend, unless the
// record already reads suspended: scion refuses to suspend it again, which
// changes nothing for the next resume. It reports whether the manager is
// suspended after all.
func reportManagerSuspendError(cmd *cobra.Command, sc workerSuspender, name, project string, serr error) bool {
	lctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	agents, err := sc.List(lctx, project)
	cancel()
	if err == nil {
		if a := scion.FindAgent(agents, name); a != nil && a.Phase == scion.PhaseSuspended {
			cmd.Printf("manager %q is already suspended.\n", name)
			return true
		}
	}
	if hubNotRunning(serr) {
		// Still a warning (the match is on wording), but naming the common case:
		// after `lever stop --keep-vm` the hub is down and the agents already
		// suspended.
		cmd.PrintErrf("lever: warning: scion suspend failed: the jail's hub did not answer (after `lever stop --keep-vm` the agents are already suspended): %s\n", termsafe.Sanitize(scion.ErrSummary(serr)))
	} else {
		cmd.PrintErrf("lever: warning: scion suspend failed (conversation may not resume cleanly on next up): %s\n", termsafe.Sanitize(scion.ErrSummary(serr)))
	}
	return false
}

// parkManager suspends the manager before the power-off, through its
// lifecycle (apply.ManagerPark), under the lifecycle lock stop holds. A
// manager whose hub phase reads stopped while claude still runs (another
// claude fired the shared SessionEnd hook) cannot be suspended, and scion
// resumes a stopped record with a fresh session, losing the conversation:
// it is reported running first (apply.HealAgentSession), within
// stopHealBudget. The report travels on the agent's hub token, often expired
// by then, so an expired token is reset first (scion reset-auth, behind
// verifyRole, the pre-role record guard); verifyRole nil means no reset. It
// returns the suspend's error.
func parkManager(ctx context.Context, cmd *cobra.Command, app *config.App, sc *scion.Client, probe apply.AgentSessionProbe, verifyRole func(ctx context.Context, project, agent string) error, revoked func(agent string) bool, project string) error {
	d := apply.Deps{
		Scion: sc, AgentSession: probe, VerifyAgentRole: verifyRole, AgentRevoked: revoked,
		JailMount: project, ManagerLock: apply.HeldManagerLock,
		Log: func(format string, args ...any) { logLine(cmd.ErrOrStderr(), "lever stop: "+format, args...) },
	}
	_, err := apply.EnsureManager(ctx, app, d, apply.ManagerRequest{Intent: apply.ManagerPark, Subject: "lever stop", HealBudget: stopHealBudget})
	return err
}

// stopRoleVerifier is apply's pre-role record guard (hubapi.VerifyAgentRole),
// run through the jail with the controller PAT.
func stopRoleVerifier(jr proc.Runner, st state.State, sc *scion.Client) func(ctx context.Context, project, agent string) error {
	hc := &hubapi.Client{T: hubJailTransport(jr, st)}
	return func(ctx context.Context, project, agent string) error {
		return hubapi.VerifyAgentRole(ctx, sc.RolesSupported, hc, project, agent)
	}
}

// stopHealBudget bounds parkManager's heal. A var so a test can shrink it.
var stopHealBudget = 20 * time.Second

var stopSessionProbe = func(r proc.Runner) apply.AgentSessionProbe { return jail.AgentProbe{R: r} }

// The worker pass: one list, then up to workerSuspendParallel suspends at a
// time, each under its own timeout, so one hung worker cannot cost the others
// their suspend.
const (
	workerListTimeout     = 15 * time.Second
	workerSuspendTimeout  = 20 * time.Second
	workerSuspendParallel = 4
)

type workerSuspender interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Suspend(ctx context.Context, worker, project string) error
}

// suspendRunningWorkers suspends every configured worker the hub shows running
// or coming up. A worker left running across the power-off comes back in phase
// error with a never-started container, whose resume answers 409. Best-effort:
// failures warn and the caller powers off regardless. It reports whether every
// needed suspend took (false on a failed list).
func suspendRunningWorkers(cmd *cobra.Command, sc workerSuspender, app *config.App, project string) bool {
	if len(app.Workers) == 0 {
		return true
	}
	lctx, cancel := context.WithTimeout(cmd.Context(), workerListTimeout)
	agents, err := sc.List(lctx, project)
	cancel()
	if err != nil {
		cmd.PrintErrf("lever: warning: listing agents failed, no worker was suspended (a running worker may not resume cleanly on next up): %s\n", termsafe.Sanitize(scion.ErrSummary(err)))
		return false
	}
	var names []string
	for _, wk := range app.Workers {
		if a := scion.FindAgent(agents, wk.Name); a != nil && suspendBeforeStop(a.Phase) {
			names = append(names, wk.Name)
		}
	}
	errs := suspendWorkers(cmd.Context(), sc, names, project, workerSuspendParallel, workerSuspendTimeout)
	// Reported in config order, whatever order the suspends finished in.
	ok := true
	for i, name := range names {
		if errs[i] != nil {
			ok = false
			cmd.PrintErrf("lever: warning: scion suspend of worker %q failed (it may not resume cleanly on next up): %s\n", name, termsafe.Sanitize(scion.ErrSummary(errs[i])))
			continue
		}
		cmd.Printf("worker %q suspended — it stays suspended after `lever up`; resume it from the manager (`lever-manager agent resume %s`).\n", name, name)
	}
	return ok
}

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

// suspendBeforeStop reports a phase whose container is up or coming up
// ("resumed" is scion's own, outside the hub enum).
func suspendBeforeStop(phase string) bool {
	switch phase {
	case scion.PhaseRunning, "resumed", "starting":
		return true
	}
	return false
}

// stopHostDaemons stops the broker and the remote proxy; both are idempotent.
// Failures only warn: the VM-side teardown must still run.
func stopHostDaemons(cmd *cobra.Command, st state.State) {
	if err := brokerctl.StopBroker(st); err != nil {
		cmd.PrintErrf("lever: warning: stopping broker: %v\n", err)
	}
	if err := brokerctl.StopRemoteProxy(st); err != nil {
		cmd.PrintErrf("lever: warning: stopping remote proxy: %v\n", err)
	}
}

// brokerHoldsLock reports whether the lifecycle lock's holder is the broker
// (brokerctl's managerLock names it so). Only lever processes write the
// holder: the lock file is in the state directory, outside the tree.
func brokerHoldsLock(st state.State) bool {
	return strings.HasPrefix(hostledger.LockHolder(st.ConversationLock()), "the broker's ")
}
