package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/wait"
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
	// exitConversationNotRunning: --by found the manager, or the jail, not
	// running; nothing was changed.
	exitConversationNotRunning = 7
)

// errConvBusy: the manager reports a turn in progress.
var errConvBusy = errors.New("the manager is in the middle of a turn; nothing was changed. Try again when it is idle, or pass --force to interrupt the turn")

var errConvNotRunning = errors.New("the manager is not running; nothing was changed")

// newConversation backs the manager's conversation up and starts it again in
// a fresh claude session on the same record and container image, with its
// boot prompt (apply.ManagerNewSession). It returns the backup (saved is
// false when the home held no conversation).
//
// The backup runs while the manager is suspended, so nothing writes the
// transcripts while they are copied; a failed backup resumes it on its own
// conversation. After the stop a failure leaves the manager stopped and
// names the backup.
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
	var begin sessionBegin
	_, err = o.ensure(ctx, apply.ManagerRequest{
		Intent: apply.ManagerNewSession, Task: task, Force: req.Force, RunningOnly: req.RunningOnly,
		WhileSuspended: func(ctx context.Context) error {
			var berr error
			// skipEmpty: a manager that empties its own transcripts must not rotate
			// the real backups out one reset at a time.
			m, saved, berr = o.backupIf(ctx, req.Reason, true)
			if errors.Is(berr, convbackup.ErrNoHome) {
				berr = fmt.Errorf("%w, so it cannot tell whether the conversation would be lost", berr)
			}
			if berr != nil {
				return fmt.Errorf("backing up the conversation failed, nothing was changed: %w", berr)
			}
			if !saved {
				o.log("conversation: manager %q's home holds no conversation; nothing to back up", o.app.Name)
			}
			begin = o.session()
			return nil
		},
		BeginSession: func() (func() error, error) { return begin() },
	})
	switch {
	case err == nil:
		return m, saved, nil
	case errors.Is(err, apply.ErrManagerBusy):
		return m, false, cli.WithExitCode(errConvBusy, exitConversationBusy)
	case errors.Is(err, apply.ErrManagerNotRunning):
		return m, false, cli.WithExitCode(errConvNotRunning, exitConversationNotRunning)
	case errors.Is(err, apply.ErrStopUnconfirmed):
		where := "its home held no conversation to back up"
		if saved {
			where = "the conversation is in backup " + m.ID
		}
		return m, saved, fmt.Errorf("%w; %s. `lever doctor` shows the manager, `lever up` brings it up", err, where)
	case errors.Is(err, apply.ErrSessionEnded):
		return m, saved, o.afterStop(m, saved, err)
	}
	return m, saved, err
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
// name the caller to the one it turns away; maxWait is lockWait's.
func lockConversation(ctx context.Context, app *config.App, st state.State, verb, from string, maxWait time.Duration, notice func(string)) (func(), error) {
	if brokerctl.StateInsideTree(app, st) {
		return nil, errConvStateInTree
	}
	return takeManagerLock(ctx, st, verb, from, maxWait, notice)
}

// lockLifecycle is lockConversation for the other commands that change the
// instance. With the state directory inside the tree there is no new or
// restore to keep apart from, and a lock file there would be the agents' to
// hold.
func lockLifecycle(ctx context.Context, app *config.App, st state.State, verb string, maxWait time.Duration, notice func(string)) (func(), error) {
	if brokerctl.StateInsideTree(app, st) {
		return func() {}, nil
	}
	return takeManagerLock(ctx, st, verb, "", maxWait, notice)
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
	return lockHolderName(p)
}

func lockHolderName(p string) string {
	return cmp.Or(termsafe.Sanitize(hostledger.LockHolder(p)), "another lever command")
}

// lockWaitFlagUsage is --wait's help on the commands that take the lock.
const lockWaitFlagUsage = "when another lever command is changing this instance, wait up to this long for it to finish (e.g. 5m) instead of exiting 6 at once"

func addLockWaitFlag(c *cobra.Command, d *time.Duration, def time.Duration) {
	c.Flags().DurationVar(d, "wait", def, lockWaitFlagUsage)
}

// lockNotice prints a lock wait's notice on the command's stderr.
func lockNotice(cmd *cobra.Command) func(string) {
	return func(msg string) { cmd.PrintErrln(msg) }
}

// lockPoll is how often a waiting command tries the lock again.
var lockPoll = time.Second

// takeManagerLock turns the caller away at once (exit 6, naming the holder)
// unless maxWait is set: then it waits up to maxWait for the holder to
// finish, saying once what it waits for (notice). Only scripts and lever's
// own unattended callers wait by default: a command that waited silently
// would look hung.
func takeManagerLock(ctx context.Context, st state.State, verb, from string, maxWait time.Duration, notice func(string)) (func(), error) {
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("locking the manager record: %w", err)
	}
	p := st.ConversationLock()
	try := func() (func(), error) {
		holder := fmt.Sprintf("`%s`%s (pid %d, since %s)", verb, from, os.Getpid(), time.Now().UTC().Format("15:04:05 UTC"))
		return hostledger.TryLockFileAs(p, holder)
	}
	unlock, err := try()
	if errors.Is(err, hostledger.ErrLocked) && maxWait > 0 {
		if notice != nil {
			notice(fmt.Sprintf("lever: %s is changing this instance; waiting up to %s for it to finish (--wait)", lockHolderName(p), maxWait))
		}
		werr := wait.For(ctx, wait.Spec{Name: "the lifecycle lock", Budget: maxWait, Interval: lockPoll}, func(context.Context) wait.Result {
			unlock, err = try()
			if errors.Is(err, hostledger.ErrLocked) {
				return wait.NotYet("held")
			}
			return wait.Ready()
		})
		if werr != nil && !errors.Is(werr, wait.ErrTimeout) {
			return nil, werr
		}
	}
	if errors.Is(err, hostledger.ErrLocked) {
		other := termsafe.Sanitize(hostledger.LockHolder(p))
		if other == "" {
			other = "another `lever conversation new`, `conversation restore`, `conversation backup`, `up`, `apply`, `stop`, `reload`, `destroy`, `attach` or `acceptance`"
		}
		waited := ""
		if maxWait > 0 {
			waited = fmt.Sprintf(" (waited %s)", maxWait)
		}
		return nil, cli.WithExitCode(fmt.Errorf("%s is changing this instance's manager; nothing was changed%s. Run `%s` again once it has finished, or pass --wait", other, waited, verb), exitConversationLocked)
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
	var lockWait time.Duration
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
			unlock, err := lockConversation(cmd.Context(), app, st, "lever conversation new", from, lockWait, lockNotice(cmd))
			if err != nil {
				return err
			}
			defer unlock()
			if by != "" {
				// lever's own callers may have waited for the lock behind a stop or
				// a destroy: they must not boot, or create, the jail that undid.
				b, err := bf(app.Backend, machineName(app.Name))
				if err != nil {
					return err
				}
				if err := b.ResolveRunUser(cmd.Context()); err != nil {
					return cli.WithExitCode(fmt.Errorf("the jail is not up (%v): nothing was started or changed", err), exitConversationNotRunning)
				}
			}
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
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: rep, Locked: true})
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
	addLockWaitFlag(c, &lockWait, 0)
	return c
}
