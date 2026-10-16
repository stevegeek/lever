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
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
)

// `lever up --fresh` deletes the manager's record, and scion deletes the home
// with it, where Claude Code keeps the transcripts. So --fresh backs them up
// first (package convbackup), and --keep-conversation or `lever conversation
// restore` puts them back into the new record's home.

type convOps struct {
	app   *config.App
	st    state.State
	guest convbackup.Guest
	log   func(format string, args ...any)
	// deps carries the manager's lifecycle (apply.EnsureManager), which
	// suspends, stops, resumes and gates the manager around a restore or a
	// new conversation.
	deps apply.Deps
	now  func() time.Time
	// session begins the record of a new conversation's fresh session (see
	// newConversation).
	session func() sessionBegin
	// bootTask reads the boot prompt a new conversation's session runs.
	bootTask func() (string, error)
}

func newConvOps(w *applyWiring, app *config.App, st state.State) convOps {
	return convOps{
		app:      app,
		st:       st,
		guest:    convbackup.Guest{R: w.b.JailRunner(), Workspace: w.b.MountDest(), Agent: app.Name},
		log:      w.deps.Log,
		deps:     w.deps,
		session:  func() sessionBegin { return beginFreshSession(app, st) },
		bootTask: func() (string, error) { return apply.ManagerTask(app) },
	}
}

// ensure runs the manager's lifecycle for a conversation command.
func (o convOps) ensure(ctx context.Context, q apply.ManagerRequest) (apply.ManagerResult, error) {
	q.Subject = "conversation"
	return apply.EnsureManager(ctx, o.app, o.deps, q)
}

// errConvStateInTree: a backup holds the whole conversation and whatever
// secrets the agent saw; lever keeps none where an agent can read.
var errConvStateInTree = fmt.Errorf("the state directory (%s) is inside the tree the agents mount, so lever keeps no conversation backup there", state.DirName)

// restoredUnknownSkill matches no skill, so the proxy refuses contacts' posts
// to a manager restored from a backup without a skill record until it next
// starts fresh.
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

// backup copies the transcripts to the store and prunes it to convbackup.Keep.
// found is false when the home holds no projects; a home lever cannot find is
// convbackup.ErrNoHome.
func (o convOps) backup(ctx context.Context, reason string) (m convbackup.Meta, found bool, err error) {
	return o.backupIf(ctx, reason, false)
}

// backupIf with skipEmpty saves and prunes nothing when the transcripts hold no
// bytes, so an agent that empties its transcripts cannot push the real backups
// out of the store one `lever up` at a time.
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
	// The skill the saved session started with travels with it (see restore).
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

// backupBeforeDelete is BackupManagerConversation for `up --fresh` (the
// manager's lifecycle suspends a running manager first, so the copy is the
// conversation as it ends). *saved is set when there was a conversation to
// save.
func (o convOps) backupBeforeDelete(saved **convbackup.Meta) func(ctx context.Context, rec *scion.Agent) error {
	return o.backupBeforeDeleteAs(saved, "up --fresh", false)
}

// backupBeforeNoConversationFresh is BackupManagerConversation for the fresh
// start of a manager with no conversation: empty transcripts are not saved.
func (o convOps) backupBeforeNoConversationFresh(saved **convbackup.Meta) func(ctx context.Context, rec *scion.Agent) error {
	return o.backupBeforeDeleteAs(saved, "up (no conversation)", true)
}

func (o convOps) backupBeforeDeleteAs(saved **convbackup.Meta, reason string, skipEmpty bool) func(ctx context.Context, rec *scion.Agent) error {
	return func(ctx context.Context, _ *scion.Agent) error {
		m, found, err := o.backupIf(ctx, reason, skipEmpty)
		if errors.Is(err, convbackup.ErrNoHome) {
			// scion will delete a home lever could not find: no proof there is nothing to
			// lose.
			err = fmt.Errorf("%w for the record it is about to delete, so it cannot tell whether a conversation would be lost", err)
		}
		if err != nil {
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

// conversationEmpty is nil only when the guest shows the manager's home holds
// no transcript (convbackup.TranscriptsNone). Any other answer is why lever
// does not start the manager fresh on its own.
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

// restore puts backup id ("" = newest) into the manager's home and resumes it.
// The manager must be running or suspended; a running one is suspended first,
// and any failure after that resumes it as it was. backupFirst saves the
// conversation it replaces.
//
// The session record is written before the files move, with the backup's
// skill, so the proxy never takes the restored conversation for one started on
// the current skill.
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
	// Check the archive whole before anything changes, and keep a handle open: a
	// backup taken below may prune it, and an open file outlives its removal.
	if err := verifyStored(store, m.ID); err != nil {
		return err
	}
	archive, err := store.Open(m.ID)
	if err != nil {
		return err
	}
	defer archive.Close()

	// unsure: an install failed possibly after the swap, so either conversation
	// may be in place.
	unsure := false
	_, err = o.ensure(ctx, apply.ManagerRequest{
		Intent: apply.ManagerRestore,
		WhileSuspended: func(ctx context.Context) error {
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
				// Only an install known to have changed nothing gets its record back.
				// Otherwise the record refuses contacts until the manager next starts
				// fresh: never the old conversation under the current skill.
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
			switch hash {
			case restoredUnknownSkill:
				o.log("conversation: the backup does not say which lever skill the conversation started with, so contacts cannot post to the manager until it next starts fresh")
			case brokerctl.UncheckedSkill:
				o.log("conversation: the conversation comes from a fresh start lever made on its own over a changed skill, so contacts cannot post to the manager until it next starts fresh without --keep-conversation")
			}
			return nil
		},
		ResumeHow: func() string {
			if unsure {
				return "on whichever conversation its home now holds"
			}
			return "on its own conversation"
		},
	})
	return err
}

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

func newConversationCmd(bf BackendFactory) *cobra.Command {
	c := &cobra.Command{
		Use:   "conversation",
		Short: "Start the manager on a new conversation; list, take and restore backups of its conversation",
	}
	c.AddCommand(newConversationNewCmd(bf), newConversationListCmd(), newConversationBackupCmd(bf), newConversationRestoreCmd(bf))
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
			unlock, err := lockConversation(app, st, "lever conversation restore", "")
			if err != nil {
				return err
			}
			defer unlock()
			run, pctx, err := startBringup(cmd, app, st, "conversation-restore", progressMode)
			if err != nil {
				return err
			}
			defer run.finishOnReturn(&err)
			rep := run.rep
			ctx, stop := applySignalContext(pctx)
			defer stop()
			rep.Group("jail", 0)
			w, err := buildApplyDeps(ctx, app, path, bf, applyOpts{Cmd: cmd, Rep: rep, Locked: true})
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
