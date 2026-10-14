package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
)

// newConvRequest is one new conversation. Reason names it in the backup list.
type newConvRequest struct {
	Reason string
	// Force interrupts a turn the manager is in the middle of.
	Force bool
}

// sessionBegin runs once the old container is gone and returns the commit
// made after the new session started.
type sessionBegin func() (commit func() error, err error)

// beginFreshSession records a new conversation's session as `lever up
// --fresh` does: at the operator's word, on the skill on disk once the old
// container is gone. A session it cannot record on a skill gets a record that
// matches none, so the old session's record never vouches for it.
func beginFreshSession(app *config.App, st state.State) sessionBegin {
	fence := func() error {
		return sessionrec.Append(st.Sessions(), sessionrec.Record{
			Agent: app.Name, SkillHash: brokerctl.UncheckedSkill, Version: cli.VersionString(), Started: time.Now().UTC(),
		})
	}
	return func() (func() error, error) {
		if _, err := sessionrec.SkillHash(app, app.Name); err != nil {
			if ferr := fence(); ferr != nil {
				return nil, ferr
			}
			return func() error { return err }, nil
		}
		commit := brokerctl.BeginSession(app, st, cli.VersionString(), app.Name)
		return func() error {
			err := commit()
			if err != nil {
				err = errors.Join(err, fence())
			}
			return err
		}, nil
	}
}

// Exit codes of `lever conversation new` (and of `restore` for the lock), as
// cli's: a script reads the code, never the text.
const (
	// exitConversationBusy: the manager is in the middle of a turn; nothing
	// was changed.
	exitConversationBusy = 5
	// exitConversationLocked: another `new` or `restore` is running; nothing
	// was changed.
	exitConversationLocked = 6
)

// errConvBusy: the manager reports a turn in progress.
var errConvBusy = errors.New("the manager is in the middle of a turn; nothing was changed. Try again when it is idle, or pass --force to interrupt the turn")

// managerBusy reads the hub's activity for a turn in progress. The activity
// is the agent's own report: an agent can hold off a reset this way, never
// bring one about.
func managerBusy(activity string) bool {
	switch activity {
	case scion.ActivityWorking, "thinking", "executing":
		return true
	}
	return false
}

// newConversation backs the manager's conversation up and starts it again in
// a fresh claude session on the same record and container image, with its
// boot prompt. It returns the backup (saved is false when the home held no
// conversation).
//
// The hub resumes a suspended record with claude --continue and a stopped one
// in a fresh session, so: suspend (nothing writes the transcripts while they
// are copied), back up, stop, then resume. The hub gives that fresh session
// the task the record was created with, the boot prompt. Up to the stop
// every failure resumes the manager, when this call suspended it, on its own
// conversation. After the stop no resume can continue the old session, so a
// failure leaves the manager stopped and names the backup.
//
// A real stop, not a second claude process, ends the old session: the shared
// SessionEnd hook then reports a phase that is true.
func (o convOps) newConversation(ctx context.Context, req newConvRequest) (m convbackup.Meta, saved bool, err error) {
	if _, err := o.store(); err != nil {
		return m, false, err
	}
	if o.session == nil {
		return m, false, errors.New("internal error: the new conversation is not wired")
	}
	project := o.guest.Workspace
	agents, err := o.sc.List(ctx, project)
	if err != nil {
		return m, false, fmt.Errorf("the hub does not answer (%s); nothing was changed", termsafe.Sanitize(scion.ErrSummary(err)))
	}
	rec := scion.FindAgent(agents, o.app.Name)
	if rec == nil {
		return m, false, errors.New("there is no manager record; run `lever up` first")
	}
	suspended, stopped := false, false
	switch rec.Phase {
	case scion.PhaseRunning:
		if !req.Force && managerBusy(rec.Activity) {
			return m, false, cli.WithExitCode(errConvBusy, exitConversationBusy)
		}
		o.log("conversation: suspending manager %q to back up its conversation", o.app.Name)
		if err := o.sc.Suspend(ctx, o.app.Name, project); err != nil {
			return m, false, fmt.Errorf("suspending the manager failed, nothing was changed: %s", termsafe.Sanitize(scion.ErrSummary(err)))
		}
		suspended = true
	case scion.PhaseSuspended:
	default:
		return m, false, fmt.Errorf("manager %q is in phase %s; a new conversation needs it running or suspended (`lever up` first)", o.app.Name, scion.BoundedQuote(rec.Phase))
	}
	defer func() {
		if err != nil && suspended && !stopped {
			err = o.resumeAfterFailure(ctx, err, "on its own conversation")
		}
	}()
	if last, err := o.waitPhase(ctx, scion.PhaseSuspended); err != nil {
		return m, false, fmt.Errorf("the manager did not suspend (last seen %s); nothing was changed", scion.BoundedQuote(last))
	}
	// skipEmpty: a manager that empties its own transcripts must not rotate the
	// real backups out one reset at a time.
	m, saved, err = o.backupIf(ctx, req.Reason, true)
	if errors.Is(err, convbackup.ErrNoHome) {
		err = fmt.Errorf("%w, so it cannot tell whether the conversation would be lost", err)
	}
	if err != nil {
		return m, false, fmt.Errorf("backing up the conversation failed, nothing was changed: %w", err)
	}
	if !saved {
		o.log("conversation: manager %q's home holds no conversation; nothing to back up", o.app.Name)
	}
	begin := o.session()

	o.log("conversation: stopping manager %q to end its session", o.app.Name)
	if serr := o.sc.Stop(ctx, o.app.Name, project); serr != nil && o.stillSuspended(ctx) {
		return m, saved, fmt.Errorf("stopping the manager failed, so its session was not ended: %s", termsafe.Sanitize(scion.ErrSummary(serr)))
	}
	if last, werr := o.waitPhase(ctx, scion.PhaseStopped); werr != nil {
		if o.stillSuspended(ctx) {
			return m, saved, fmt.Errorf("the manager did not stop (last seen %s), so its session was not ended", scion.BoundedQuote(last))
		}
		stopped = true
		where := "its home held no conversation to back up"
		if saved {
			where = "the conversation is in backup " + m.ID
		}
		return m, saved, fmt.Errorf("the manager did not stop in time (last seen %s), so lever cannot tell whether its session ended and started nothing; %s. `lever doctor` shows the manager, `lever up` brings it up", scion.BoundedQuote(last), where)
	}
	stopped = true

	commit, err := begin()
	if err != nil {
		return m, saved, o.afterStop(m, saved, fmt.Errorf("recording the new session failed: %w", err))
	}
	if err := o.verifyRole(ctx); err != nil {
		return m, saved, o.afterStop(m, saved, err)
	}
	guard, err := o.startResume(ctx)
	if err != nil {
		return m, saved, o.afterStop(m, saved, err)
	}
	o.log("conversation: starting manager %q in a new session", o.app.Name)
	if err := o.sc.Resume(ctx, o.app.Name, project); err != nil {
		// Not apply.ResumeFailed: its exit code and its "conversation kept" are
		// not true once the old session has ended.
		err = fmt.Errorf("starting the manager failed: %s", termsafe.Sanitize(scion.ErrSummary(err)))
		return m, saved, o.afterStop(m, saved, guard.AfterFailure(ctx, err))
	}
	if err := guard.Resumed(ctx); err != nil {
		return m, saved, o.afterStop(m, saved, err)
	}
	if err := commit(); err != nil {
		o.log("conversation: warning: the new session was not recorded on a skill (%v); contacts cannot post to the manager until it next starts fresh", err)
	}
	if err := o.gate(ctx); err != nil {
		return m, saved, o.afterStop(m, saved, guard.AfterFailure(ctx, err))
	}
	return m, saved, guard.Live(ctx)
}

// stillSuspended reports whether the hub reads the manager suspended: a stop
// that did not land left the old session resumable.
func (o convOps) stillSuspended(ctx context.Context) bool {
	agents, err := o.sc.List(ctx, o.guest.Workspace)
	if err != nil {
		return false
	}
	rec := scion.FindAgent(agents, o.app.Name)
	return rec != nil && rec.Phase == scion.PhaseSuspended
}

// afterStop says where things stand when a step after the stop failed: the
// old session can no longer be continued, the backup holds it.
func (o convOps) afterStop(m convbackup.Meta, saved bool, cause error) error {
	if !saved {
		return fmt.Errorf("%w; the manager's old session has ended (its home held no conversation to back up): `lever up` starts it in a new conversation", cause)
	}
	return fmt.Errorf("%w; the manager's old session has ended and is in backup %s: `lever up` starts it in a new conversation, and `lever conversation restore %s` after that puts the old one back", cause, m.ID, m.ID)
}

// lockConversation keeps two conversation changes (new, restore) of one
// instance apart: each suspends, swaps or stops the same record.
func lockConversation(app *config.App, st state.State) (func(), error) {
	if brokerctl.StateInsideTree(app, st) {
		return nil, errConvStateInTree
	}
	unlock, err := hostledger.TryLockFile(filepath.Join(st.Dir, "conversation.lock"))
	if errors.Is(err, hostledger.ErrLocked) {
		return nil, cli.WithExitCode(errors.New("another `lever conversation new` or `restore` is running; nothing was changed"), exitConversationLocked)
	}
	if err != nil {
		return nil, fmt.Errorf("locking the conversation: %w", err)
	}
	return unlock, nil
}

// newConvBy are the callers --by names, beyond the operator's terminal.
var newConvBy = map[string]newConvRequest{
	"":     {Reason: "new conversation"},
	"chat": {Reason: "new conversation (chat)"},
}

func newConversationNewCmd(bf BackendFactory) *cobra.Command {
	var force bool
	var by, progressMode string
	c := &cobra.Command{
		Use:   "new [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Back up the manager's conversation and start it again in a new one, with its boot prompt",
		Long: "Back up the manager's conversation, then start the manager again in a new Claude session on the same\n" +
			"record (no image reload, no record deletion), which runs the boot prompt the record was created with\n" +
			"(manager.prompt_file at the last fresh create). A manager in the middle\n" +
			"of a turn is left alone unless --force. `lever conversation restore` puts the old conversation back.",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			req, ok := newConvBy[by]
			if !ok {
				return fmt.Errorf("--by: unknown caller %q", by)
			}
			req.Force = force
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			st := stateFor(path)
			unlock, err := lockConversation(app, st)
			if err != nil {
				return err
			}
			defer unlock()
			run, pctx, err := startBringup(cmd, app, st, "conversation-new", progressMode)
			if err != nil {
				return err
			}
			defer run.finishOnReturn(&err)
			rep := run.rep
			ctx, stop := applySignalContext(pctx)
			defer stop()
			rep.Group("jail", 0)
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: rep})
			if err != nil {
				return err
			}
			rep.Group("conversation", 0)
			step := rep.Start("new conversation")
			m, saved, err := newConvOps(w, app, st).newConversation(ctx, req)
			step.Done(err)
			if err != nil {
				return err
			}
			stop()
			run.finish(nil)
			if saved {
				cmd.Printf("manager %q is on a new conversation; the old one is backup %s (`lever conversation restore %s` resumes it).\n", app.Name, m.ID, m.ID)
			} else {
				cmd.Printf("manager %q is on a new conversation.\n", app.Name)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "interrupt a turn the manager is in the middle of")
	c.Flags().StringVar(&by, "by", "", "the caller that asks (chat); for lever's own use")
	_ = c.Flags().MarkHidden("by")
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
	return c
}
