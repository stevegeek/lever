package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
)

// The manager's conversation across a fresh create (lever#198). `lever up
// --fresh` deletes the manager's record, and scion deletes the record's home
// with it, where Claude Code keeps the transcripts. So --fresh backs them up
// first (package convbackup), and --keep-conversation, or `lever
// conversation restore` later, puts them back into the new record's home.

// convScion is the part of *scion.Client a restore drives (a test seam).
type convScion interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Suspend(ctx context.Context, worker, project string) error
	Resume(ctx context.Context, worker, project string) error
}

// convOps backs up and restores the manager's conversation.
type convOps struct {
	app   *config.App
	st    state.State
	guest convbackup.Guest
	sc    convScion // restore and the pre-backup suspend only
	log   func(format string, args ...any)
	// verifyRole is the pre-role record guard a resume passes (as `up`'s
	// own resume does), and gate the liveness gate after it.
	verifyRole func(ctx context.Context) error
	gate       func(ctx context.Context) error
	// beginResume starts the checks around every resume of the manager
	// (apply.BeginManagerResume, lever#158): the host tree before it, the
	// mounts read back after it. newConvOps always sets it.
	beginResume func(ctx context.Context) (managerResume, error)
	now         func() time.Time
	// suspendWait bounds the wait for a suspended record (a test seam).
	suspendWait apply.RetryBudget
}

// managerResume is the part of *apply.ManagerResume a restore drives (a
// test seam).
type managerResume interface {
	Resumed(ctx context.Context) error
	Live(ctx context.Context) error
	Settle(ctx context.Context) error
	AfterFailure(ctx context.Context, cause error) error
}

// newConvOps wires convOps from a bring-up's live dependencies.
func newConvOps(w *applyWiring, app *config.App, st state.State) convOps {
	project := w.b.MountDest()
	return convOps{
		app:   app,
		st:    st,
		guest: convbackup.Guest{R: w.b.JailRunner(), Workspace: project, Agent: app.Name},
		sc:    w.sc,
		log:   w.deps.Log,
		verifyRole: func(ctx context.Context) error {
			return verifyManagerRole(ctx, w.deps, project, app.Name)
		},
		gate: func(ctx context.Context) error {
			return apply.WaitManagerLive(ctx, w.deps, app.Name, project, nil)
		},
		beginResume: func(ctx context.Context) (managerResume, error) {
			return apply.BeginManagerResume(ctx, w.deps, app, project, "conversation")
		},
	}
}

// startResume runs the checks before a resume of the manager. Without
// beginResume (a convOps not built by newConvOps) it refuses: a resume
// whose mounts nobody reads back is never made.
func (o convOps) startResume(ctx context.Context) (managerResume, error) {
	if o.beginResume == nil {
		return nil, errors.New("internal error: no resume checks wired")
	}
	return o.beginResume(ctx)
}

// errConvStateInTree: a backup holds the whole conversation, and with it
// whatever secrets the agent saw; lever keeps none where an agent can read.
var errConvStateInTree = fmt.Errorf("the state directory (%s) is inside the tree the agents mount, so lever keeps no conversation backup there", state.DirName)

// restoredUnknownSkill is the session record's skill hash for a restored
// conversation whose backup did not know the skill it was started with. It
// matches no skill, so the remote proxy refuses contacts' posts to the
// manager until it next starts fresh.
const restoredUnknownSkill = "unknown (restored conversation)"

func (o convOps) store() (convbackup.Store, error) {
	if brokerctl.StateInsideTree(o.app, o.st) {
		return convbackup.Store{}, errConvStateInTree
	}
	return convbackup.Store{Dir: filepath.Join(o.st.Dir, convbackup.DirName)}, nil
}

func (o convOps) clock() time.Time {
	if o.now != nil {
		return o.now().UTC()
	}
	return time.Now().UTC()
}

// backup copies the manager's transcripts to the store and prunes it to
// convbackup.Keep. found is false when the manager's home holds no
// projects; a home lever cannot find is convbackup.ErrNoHome.
func (o convOps) backup(ctx context.Context, reason string) (m convbackup.Meta, found bool, err error) {
	return o.backupIf(ctx, reason, false)
}

// backupIf is backup; skipEmpty treats a projects directory with no byte of
// transcript in it (no file, or only empty ones) as nothing to back up: no backup is saved, and none is pruned. The
// fresh start of a manager with no conversation (lever#202) uses it, so an
// agent that empties its transcripts cannot push the real backups out of
// the store one `lever up` at a time.
func (o convOps) backupIf(ctx context.Context, reason string, skipEmpty bool) (m convbackup.Meta, found bool, err error) {
	store, err := o.store()
	if err != nil {
		return m, false, err
	}
	raw, found, changed, err := o.guest.Capture(ctx)
	if err != nil || !found {
		return m, false, err
	}
	if skipEmpty {
		st, cerr := convbackup.Copy(io.Discard, strings.NewReader(raw), "projects", convbackup.DefaultLimits)
		if cerr != nil {
			return m, false, fmt.Errorf("reading the manager's conversation: %w", cerr)
		}
		if st.Bytes == 0 {
			return m, false, nil
		}
	}
	m = convbackup.Meta{Agent: o.app.Name, Created: o.clock(), Reason: reason, Version: cli.VersionString()}
	// The session record of the conversation being saved: the skill it
	// started with travels with it (see restore).
	if recs, rerr := sessionrec.Latest(o.st.Sessions()); rerr != nil {
		o.log("conversation: warning: the session record cannot be read (%v); a restore of this backup is treated as started with an unknown skill", rerr)
	} else if r, ok := recs[o.app.Name]; ok {
		m.SkillHash, m.SessionStarted = r.SkillHash, r.Started
	}
	m, err = store.Save(m, func(w io.Writer) (convbackup.Stats, error) {
		return convbackup.Copy(w, strings.NewReader(raw), "projects", convbackup.DefaultLimits)
	})
	if err != nil {
		return m, false, fmt.Errorf("saving the manager's conversation: %w", err)
	}
	if changed {
		o.log("conversation: note: a transcript changed while it was read (the manager was running); the backup holds it as it was read")
	}
	if m.Skipped > 0 {
		o.log("conversation: note: %d links or special files in the manager's .claude/projects were not backed up", m.Skipped)
	}
	o.log("conversation: backed up manager %q's conversation (%d files, %s) to %s", o.app.Name, m.Files, humanBytes(m.Bytes), store.Path(m.ID))
	if removed, perr := store.Prune(convbackup.Keep); perr != nil {
		o.log("conversation: warning: removing old backups: %v", perr)
	} else if len(removed) > 0 {
		o.log("conversation: removed %d older backup(s); lever keeps the last %d", len(removed), convbackup.Keep)
	}
	return m, true, nil
}

// backupBeforeDelete is apply's BackupManagerConversation for `up --fresh`:
// a running manager is suspended first, so the copy is the conversation as
// it ends (a manager that will not suspend is copied as it runs). *saved is
// set to the backup when there was a conversation to save.
func (o convOps) backupBeforeDelete(saved **convbackup.Meta) func(ctx context.Context, rec *scion.Agent) error {
	return o.backupBeforeDeleteAs(saved, "up --fresh", false)
}

// backupBeforeNoConversationFresh is apply's BackupManagerConversation for a
// plain `lever up`, which deletes a record only for the fresh start of a
// manager with no conversation (lever#202): a projects directory with no
// transcript content is not saved (backupIf), and the reason says which start it was.
func (o convOps) backupBeforeNoConversationFresh(saved **convbackup.Meta) func(ctx context.Context, rec *scion.Agent) error {
	return o.backupBeforeDeleteAs(saved, "up (no conversation)", true)
}

func (o convOps) backupBeforeDeleteAs(saved **convbackup.Meta, reason string, skipEmpty bool) func(ctx context.Context, rec *scion.Agent) error {
	return func(ctx context.Context, rec *scion.Agent) error {
		suspended := false
		if rec != nil && rec.Phase == scion.PhaseRunning && o.sc != nil {
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := o.sc.Suspend(sctx, o.app.Name, o.guest.Workspace); err != nil {
				o.log("conversation: could not suspend manager %q before the backup (%s); copying it while it runs", o.app.Name, termsafe.Sanitize(scion.ErrSummary(err)))
			} else {
				suspended = true
			}
			cancel()
		}
		m, found, err := o.backupIf(ctx, reason, skipEmpty)
		if errors.Is(err, convbackup.ErrNoHome) {
			// The record exists, so scion will delete a home lever could
			// not find (a registration or a layout lever does not know):
			// that is no proof there is nothing to lose.
			err = fmt.Errorf("%w for the record it is about to delete, so it cannot tell whether a conversation would be lost", err)
		}
		if err != nil {
			if suspended {
				err = o.resumeAfterFailure(ctx, err, "")
			}
			return err
		}
		if !found {
			o.log("conversation: manager %q's home holds no conversation; nothing to back up", o.app.Name)
			return nil
		}
		*saved = &m
		return nil
	}
}

// conversationEmpty is apply's ManagerConversationEmpty for `lever up`
// (lever#202): nil only when the guest shows the manager's home holds no
// transcript (convbackup.TranscriptsNone). Any other answer is the reason
// lever does not start the manager fresh on its own.
func (o convOps) conversationEmpty(ctx context.Context) error {
	t, err := o.guest.Transcripts(ctx)
	switch {
	case err != nil:
		return err
	case t == convbackup.TranscriptsNone:
		return nil
	case t == convbackup.TranscriptsSome:
		return errors.New("its .claude/projects holds transcripts, so claude --continue may have looked for another working directory's conversation")
	}
	return errors.New("its .claude holds links, special files or another projects directory, which lever does not read")
}

// restore puts backup id ("" = the newest) into the manager's home and
// resumes the manager on it. The manager must be running or suspended; a
// running one is suspended first, and every failure after that resumes it
// as it was. backupFirst saves the conversation it replaces.
//
// The session record is written before the files move: the restored
// conversation is recorded with the skill it was started with (the
// backup's), so the remote proxy never takes it for a session started on the
// skill on disk now.
func (o convOps) restore(ctx context.Context, id string, backupFirst bool) (err error) {
	store, err := o.store()
	if err != nil {
		return err
	}
	m, err := store.Get(id)
	if err != nil {
		return err
	}
	if m.Agent != "" && m.Agent != o.app.Name {
		return fmt.Errorf("backup %s is of agent %q, not of this manager %q", m.ID, termsafe.Sanitize(m.Agent), o.app.Name)
	}
	// Check the stored archive whole before anything changes, and keep a
	// second handle open for the install: a backup taken below may prune
	// this one, and an open file outlives its removal.
	if err := verifyStored(store, m.ID); err != nil {
		return err
	}
	archive, err := store.Open(m.ID)
	if err != nil {
		return err
	}
	defer archive.Close()

	project := o.guest.Workspace
	agents, err := o.sc.List(ctx, project)
	if err != nil {
		return fmt.Errorf("the hub does not answer (%s); bring the instance up first (`lever up --no-attach`)", termsafe.Sanitize(scion.ErrSummary(err)))
	}
	rec := scion.FindAgent(agents, o.app.Name)
	if rec == nil {
		return fmt.Errorf("there is no manager record to restore into; run `lever up --no-attach` first")
	}
	// suspended says this restore suspended the manager, and done that a
	// failure from here on must not resume it (it was resumed, or the role
	// guard refused it).
	suspended, done := false, false
	switch rec.Phase {
	case scion.PhaseRunning:
		o.log("conversation: suspending manager %q to swap its transcripts", o.app.Name)
		if err := o.sc.Suspend(ctx, o.app.Name, project); err != nil {
			return fmt.Errorf("suspending the manager failed, nothing was restored: %s", termsafe.Sanitize(scion.ErrSummary(err)))
		}
		suspended = true
	case scion.PhaseSuspended:
	default:
		return fmt.Errorf("manager %q is in phase %s; a restore needs it running or suspended (`lever up --no-attach` first)", o.app.Name, scion.BoundedQuote(rec.Phase))
	}
	// unsure says an install failed in a way that may have come after the
	// swap: the manager then holds one conversation or the other.
	unsure := false
	defer func() {
		if err != nil && suspended && !done {
			how := "on its own conversation"
			if unsure {
				how = "on whichever conversation its home now holds"
			}
			err = o.resumeAfterFailure(ctx, err, how)
		}
	}()
	if err := o.waitSuspended(ctx); err != nil {
		return err
	}
	if backupFirst {
		if _, _, err := o.backup(ctx, "before restore"); err != nil {
			return fmt.Errorf("backing up the conversation it would replace failed, nothing was restored (--no-backup skips that backup): %w", err)
		}
	}
	prev, hadPrev, err := o.latestSession()
	if err != nil {
		return err
	}
	hash := m.SkillHash
	if hash == "" {
		hash = restoredUnknownSkill
	}
	if err := o.recordSession(hash); err != nil {
		return fmt.Errorf("recording the restored session failed, nothing was restored: %w", err)
	}
	parked, err := o.guest.Install(ctx, archive, o.clock().Format("20060102T150405Z"))
	if err != nil {
		// Only an install known to have changed nothing gets its record
		// back. Any other failure (a timeout, a lost transport) may have
		// come after the swap, so the record refuses contacts until the
		// manager next starts fresh: never the old conversation under the
		// current skill.
		back := restoredUnknownSkill
		if errors.Is(err, convbackup.ErrNotSwapped) && hadPrev {
			back = prev.SkillHash
		} else if !errors.Is(err, convbackup.ErrNotSwapped) {
			unsure = true
		}
		if rerr := o.recordSession(back); rerr != nil {
			o.log("conversation: warning: restoring the session record failed: %v", rerr)
		}
		return fmt.Errorf("writing the conversation into the manager's home failed: %w", err)
	}
	newest := "none"
	if m.Newest != "" {
		newest = termsafe.Sanitize(m.Newest)
	}
	o.log("conversation: restored backup %s into manager %q (%d files; it resumes %s)", m.ID, o.app.Name, m.Files, newest)
	if parked != "" {
		o.log("conversation: the transcripts it replaced are in the manager's home at %s", parked)
	}
	if hash == restoredUnknownSkill {
		o.log("conversation: the backup does not say which lever skill the conversation started with, so contacts cannot post to the manager until it next starts fresh")
	}
	done = true
	if err := o.verifyRole(ctx); err != nil {
		return err
	}
	guard, err := o.startResume(ctx)
	if err != nil {
		return fmt.Errorf("%w; the conversation is restored, but the manager stays suspended: fix that, then run `lever up`", err)
	}
	if err := o.sc.Resume(ctx, o.app.Name, project); err != nil {
		if scion.IsRefusedByHub(err) {
			err = apply.HubRefusedResume("resume", err)
		} else {
			err = apply.ResumeFailed("resume", err)
		}
		return guard.AfterFailure(ctx, err)
	}
	if err := guard.Resumed(ctx); err != nil {
		return err
	}
	if err := o.gate(ctx); err != nil {
		return guard.AfterFailure(ctx, err)
	}
	return guard.Live(ctx)
}

// resumeAfterFailure resumes a manager this code suspended, after cause,
// through the pre-role record guard, on a context of its own (ctx may be
// what was cancelled), and says so in the error it returns. how says on
// which conversation ("" leaves it out).
func (o convOps) resumeAfterFailure(ctx context.Context, cause error, how string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if o.verifyRole != nil {
		if gerr := o.verifyRole(rctx); gerr != nil {
			return fmt.Errorf("%w; the manager stays suspended: %w", cause, gerr)
		}
	}
	guard, gerr := o.startResume(rctx)
	if gerr != nil {
		return fmt.Errorf("%w; the manager stays suspended: %w", cause, gerr)
	}
	if rerr := o.sc.Resume(rctx, o.app.Name, o.guest.Workspace); rerr != nil {
		err := fmt.Errorf("%w; the manager is suspended and resuming it failed (%s): run `lever up`", cause, termsafe.Sanitize(scion.ErrSummary(rerr)))
		return guard.AfterFailure(rctx, err)
	}
	// No liveness gate here: Settle checks until the container runs.
	verr := guard.Resumed(rctx)
	if verr == nil {
		verr = guard.Settle(rctx)
	}
	if verr != nil {
		return fmt.Errorf("%w; resuming the manager after that failed its mount check: %w", cause, verr)
	}
	if how != "" {
		how = " " + how
	}
	return fmt.Errorf("%w; the manager was resumed%s", cause, how)
}

// verifyStored reads backup id whole through the archive checks.
func verifyStored(store convbackup.Store, id string) error {
	rc, err := store.Open(id)
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err := convbackup.Copy(io.Discard, rc, "", convbackup.DefaultLimits); err != nil {
		return fmt.Errorf("backup %s is damaged, nothing was restored: %w", id, err)
	}
	return nil
}

// waitSuspended waits until the hub reads the manager suspended with no live
// container: no process of the agent's may run while its files move.
func (o convOps) waitSuspended(ctx context.Context) error {
	b := o.suspendWait
	if b.Attempts <= 0 {
		b = apply.RetryBudget{Attempts: 30, Interval: time.Second}
	}
	var last string
	err := retry.Until(ctx, b.Attempts, b.Interval, func() (bool, error) {
		agents, err := o.sc.List(ctx, o.guest.Workspace)
		if err != nil {
			return false, nil
		}
		rec := scion.FindAgent(agents, o.app.Name)
		if rec == nil {
			return false, errors.New("the manager record disappeared")
		}
		last = rec.Phase + "/" + rec.ContainerStatus
		return rec.Phase == scion.PhaseSuspended && !scion.ContainerLive(rec.ContainerStatus), nil
	})
	if errors.Is(err, retry.ErrExhausted) {
		return fmt.Errorf("the manager did not suspend (last seen %s); nothing was restored", scion.BoundedQuote(last))
	}
	return err
}

func (o convOps) latestSession() (sessionrec.Record, bool, error) {
	recs, err := sessionrec.Latest(o.st.Sessions())
	if err != nil {
		return sessionrec.Record{}, false, fmt.Errorf("reading the session record: %w", err)
	}
	r, ok := recs[o.app.Name]
	return r, ok, nil
}

func (o convOps) recordSession(hash string) error {
	return sessionrec.Append(o.st.Sessions(), sessionrec.Record{
		Agent: o.app.Name, SkillHash: hash, Version: cli.VersionString(), Started: o.clock(),
	})
}

// newConversationCmd is `lever conversation`: the manager's conversation
// backups, which `lever up --fresh` takes on its own.
func newConversationCmd(bf BackendFactory) *cobra.Command {
	c := &cobra.Command{
		Use:   "conversation",
		Short: "List, take and restore backups of the manager's conversation",
	}
	c.AddCommand(newConversationListCmd(), newConversationBackupCmd(bf), newConversationRestoreCmd(bf))
	return c
}

func newConversationListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List the manager's conversation backups, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return err
			}
			store, err := convOps{app: app, st: st}.store()
			if err != nil {
				return err
			}
			all, err := store.List()
			if err != nil {
				return err
			}
			if len(all) == 0 {
				cmd.Printf("no conversation backups in %s\n", store.Dir)
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTAKEN\tBY\tFILES\tSIZE\tRESUMES")
			for _, m := range all {
				newest := "-"
				if m.Newest != "" {
					newest = termsafe.Sanitize(m.Newest)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", m.ID, m.Created.Local().Format("2006-01-02 15:04"),
					termsafe.Sanitize(m.Reason), m.Files, humanBytes(m.Bytes), newest)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			cmd.Printf("in %s (lever keeps the last %d)\n", store.Dir, convbackup.Keep)
			return nil
		},
	}
}

func newConversationBackupCmd(bf BackendFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Args:  cobra.NoArgs,
		Short: "Back up the manager's conversation to the state directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, st, err := loadAppAndState(nil)
			if err != nil {
				return err
			}
			_, b, err := resolveJailBackend(bf, "", "")
			if err != nil {
				return err
			}
			// Passive: the jail must be up; nothing is provisioned.
			if err := b.ResolveRunUser(cmd.Context()); err != nil {
				return fmt.Errorf("the jail is not up (%w); the manager's home lives there", err)
			}
			o := convOps{app: app, st: st, log: func(format string, args ...any) { logLine(cmd.ErrOrStderr(), format, args...) },
				guest: convbackup.Guest{R: b.JailRunner(), Workspace: b.MountDest(), Agent: app.Name}}
			m, found, err := o.backup(cmd.Context(), "manual")
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("manager %q's home in the jail holds no conversation; nothing was backed up", app.Name)
			}
			cmd.Printf("backup %s\n", m.ID)
			return nil
		},
	}
}

func newConversationRestoreCmd(bf BackendFactory) *cobra.Command {
	var noBackup bool
	var progressMode string
	c := &cobra.Command{
		Use:   "restore [ID]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Put a backed-up conversation back into the manager and resume it (default: the newest backup)",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			id := ""
			if len(args) == 1 {
				id = args[0]
				if !convbackup.ValidID(id) {
					return fmt.Errorf("%q is not a backup id (`lever conversation list` shows them)", id)
				}
			}
			path, app, err := loadAppPath(nil)
			if err != nil {
				return err
			}
			st := stateFor(path)
			run, pctx, err := startBringup(cmd, app, st, "conversation-restore", progressMode)
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
			step := rep.Start("restore conversation")
			err = newConvOps(w, app, st).restore(ctx, id, !noBackup)
			step.Done(err)
			if err != nil {
				return err
			}
			stop()
			run.finish(nil)
			cmd.Printf("manager %q resumed on the restored conversation; `lever attach` to talk to it.\n", app.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&noBackup, "no-backup", false, "do not back up the conversation the restore replaces")
	c.Flags().StringVar(&progressMode, "progress", "", progressFlagUsage)
	return c
}
