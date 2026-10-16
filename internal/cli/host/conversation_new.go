package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
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
	// RunningOnly leaves a manager that is not running alone (exit 7).
	RunningOnly bool
}

// sessionBegin runs once the old container is gone and returns the commit
// made after the new session started.
type sessionBegin func() (commit func() error, err error)

// beginFreshSession records a new conversation's session as `lever up
// --fresh` does: at the operator's word (the daily reset's time in lever.yaml
// is that word too), on the skill on disk once the old container is gone. A session it cannot record on a skill gets a record that
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
	// exitConversationLocked: another change to the manager record (`new`,
	// `restore`, `up`, `apply`, `stop`, `reload`) is running; nothing was
	// changed.
	exitConversationLocked = 6
	// exitConversationNotRunning: --by daily found the manager not running;
	// nothing was changed.
	exitConversationNotRunning = 7
)

// errConvBusy: the manager reports a turn in progress.
var errConvBusy = errors.New("the manager is in the middle of a turn; nothing was changed. Try again when it is idle, or pass --force to interrupt the turn")

var errConvNotRunning = errors.New("the manager is not running; nothing was changed")

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
// the boot prompt: manager.prompt_file as it is now. Up to the stop
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
	if o.session == nil || o.bootTask == nil {
		return m, false, errors.New("internal error: the new conversation is not wired")
	}
	// Read before anything changes: a prompt that cannot start a session is
	// the operator's to fix while the manager still runs.
	task, err := o.bootTask()
	if err != nil {
		return m, false, fmt.Errorf("%w; nothing was changed", err)
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
		if req.RunningOnly {
			return m, false, cli.WithExitCode(errConvNotRunning, exitConversationNotRunning)
		}
	default:
		if req.RunningOnly {
			return m, false, cli.WithExitCode(errConvNotRunning, exitConversationNotRunning)
		}
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
	if err := o.resumeFresh(ctx, task); err != nil {
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
	return m, saved, o.live(ctx, guard)
}

// resumeFresh starts the stopped record in a new session on task, the
// current manager.prompt_file, which the hub also stores as the record's task.
// With no prompt_file the hub falls back to the task it stored at the last
// fresh create or new conversation; it offers no way to clear that.
func (o convOps) resumeFresh(ctx context.Context, task string) error {
	if task == "" {
		return o.sc.Resume(ctx, o.app.Name, o.guest.Workspace)
	}
	return o.sc.ResumeTask(ctx, o.app.Name, o.guest.Workspace, task)
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

// lockConversation keeps the changes to one instance's manager record apart:
// new and restore suspend, swap or stop it; up, apply, stop and reload
// resume, suspend or recreate it. verb and from (where the command came from, or "")
// name the caller to the one it turns away.
func lockConversation(app *config.App, st state.State, verb, from string) (func(), error) {
	if brokerctl.StateInsideTree(app, st) {
		return nil, errConvStateInTree
	}
	return takeManagerLock(st, verb, from)
}

// lockLifecycle is lockConversation for up, apply, stop and reload. With the state
// directory inside the tree there is no new or restore to keep apart from,
// and a lock file there would be the agents' to hold.
func lockLifecycle(app *config.App, st state.State, verb string) (func(), error) {
	if brokerctl.StateInsideTree(app, st) {
		return func() {}, nil
	}
	return takeManagerLock(st, verb, "")
}

// managerLockHolder names the command that holds the manager lock now, or ""
// when none does. It takes the lock for a moment to tell.
func managerLockHolder(app *config.App, st state.State) string {
	if brokerctl.StateInsideTree(app, st) {
		return ""
	}
	p := st.ConversationLock()
	unlock, err := hostledger.TryLockFile(p)
	if err == nil {
		unlock()
		return ""
	}
	if !errors.Is(err, hostledger.ErrLocked) {
		return ""
	}
	return cmp.Or(termsafe.Sanitize(hostledger.LockHolder(p)), "another lever command")
}

// takeManagerLock never waits: a new conversation can hold the lock for
// minutes, and a command that waited on it silently would look hung. The
// one it turns away exits 6 and names the holder.
func takeManagerLock(st state.State, verb, from string) (func(), error) {
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("locking the manager record: %w", err)
	}
	p := st.ConversationLock()
	holder := fmt.Sprintf("`%s`%s (pid %d, since %s)", verb, from, os.Getpid(), time.Now().UTC().Format("15:04:05 UTC"))
	unlock, err := hostledger.TryLockFileAs(p, holder)
	if errors.Is(err, hostledger.ErrLocked) {
		other := termsafe.Sanitize(hostledger.LockHolder(p))
		if other == "" {
			other = "another `lever conversation new`, `conversation restore`, `up`, `apply`, `stop` or `reload`"
		}
		return nil, cli.WithExitCode(fmt.Errorf("%s is changing this instance's manager; nothing was changed. Run `%s` again once it has finished", other, verb), exitConversationLocked)
	}
	if err != nil {
		return nil, fmt.Errorf("locking the manager record: %w", err)
	}
	return unlock, nil
}

// conversationNewDeadline bounds a whole new conversation: it holds the lock
// that lever stop also takes, so a stalled hub must not keep the operator from
// powering off. Recovery steps run on their own budget past it.
const conversationNewDeadline = 20 * time.Minute

// newConvBy are the callers --by names, beyond the operator's terminal.
var newConvBy = map[string]newConvRequest{
	"":     {Reason: "new conversation"},
	"chat": {Reason: "new conversation (chat)"},
	// The broker's daily reset (manager.daily_reset).
	"daily": {Reason: "new conversation (daily)", RunningOnly: true},
}

func newConversationNewCmd(bf BackendFactory) *cobra.Command {
	var force bool
	var by, progressMode string
	c := &cobra.Command{
		Use:   "new [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Back up the manager's conversation and start it again in a new one, with its boot prompt",
		Long: "Back up the manager's conversation, then start the manager again in a new Claude session on the same\n" +
			"record (no image reload, no record deletion), which runs the boot prompt (manager.prompt_file as it is\n" +
			"now). A manager in the middle of a turn is left alone unless --force.\n" +
			"`lever conversation restore` puts the old conversation back.",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			req, ok := newConvBy[by]
			if !ok {
				return fmt.Errorf("--by: unknown caller %q", by)
			}
			if by == "daily" && force {
				return errors.New("--by daily never interrupts a turn; --force is refused")
			}
			req.Force = force
			path, app, err := loadAppPath(args)
			if err != nil {
				return err
			}
			st := stateFor(path)
			from := ""
			switch by {
			case "chat":
				from = " from the chat page"
			case "daily":
				from = " for the daily reset"
			}
			unlock, err := lockConversation(app, st, "lever conversation new", from)
			if err != nil {
				return err
			}
			defer unlock()
			// The daily reset keeps no bring-up record: a manager that reports
			// busy at every retry would otherwise rotate the operator's own
			// records out each day. Its output is in conversation-new.log.
			recordIn := st
			if by == "daily" {
				recordIn = state.State{}
			}
			run, pctx, err := startBringup(cmd, app, recordIn, "conversation-new", progressMode)
			if err != nil {
				return err
			}
			defer run.finishOnReturn(&err)
			rep := run.rep
			ctx, stop := applySignalContext(pctx)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, conversationNewDeadline)
			defer cancel()
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
	c.Flags().StringVar(&by, "by", "", "the caller that asks (chat, daily); for lever's own use")
	_ = c.Flags().MarkHidden("by")
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
	return c
}
