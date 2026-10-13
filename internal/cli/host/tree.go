package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/treebackup"
)

// Exit codes of `lever tree snapshot` and `lever tree lock`.
const (
	exitTreeIncomplete = 3
	exitTreeBusy       = 75
)

// treeReadWait bounds the shared-lock wait of the read commands and the
// exclusive one of gc, delete and verify --fix.
const treeReadWait = 30 * time.Second

type treeEnv struct {
	app   *config.App
	st    state.State
	place treebackup.Placement
}

func loadTreeEnv() (*treeEnv, error) {
	app, st, err := loadAppAndState(nil)
	if err != nil {
		return nil, err
	}
	return treeEnvFor(app, st), nil
}

func treeEnvFor(app *config.App, st state.State) *treeEnv {
	return &treeEnv{app: app, st: st, place: treebackup.Placement{
		Tree:     app.Tree,
		StateDir: st.Dir,
		Store:    filepath.Join(st.Dir, treebackup.StoreDirName),
	}}
}

// openStore checks the placement first: every `lever tree` command refuses
// a store an agent could reach, with nothing written.
func (e *treeEnv) openStore() (*treebackup.Store, error) {
	if err := e.place.CheckPlacement(); err != nil {
		return nil, err
	}
	s, err := treebackup.OpenStore(e.place.Store, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no tree backups yet in %s (`lever tree snapshot` takes one)", e.place.Store)
	}
	return s, err
}

func (e *treeEnv) workerDirs() [][]byte {
	var out [][]byte
	for _, w := range e.app.Workers {
		if rel, err := filepath.Rel(e.app.Tree, e.app.WorkerDir(w)); err == nil && filepath.IsLocal(rel) {
			out = append(out, []byte(filepath.ToSlash(rel)))
		}
	}
	return out
}

func (e *treeEnv) snapshotOptions(trigger string) treebackup.SnapshotOptions {
	tb := e.app.TreeBackup
	return treebackup.SnapshotOptions{
		Placement:    e.place,
		Trigger:      trigger,
		LeverVersion: cli.Version,
		Limits:       tb.EffectiveLimits(),
		Exclude:      tb.Exclude,
		WorkerDirs:   e.workerDirs(),
		Reverify:     tb.EffectiveReverify(),
		MinFree:      tb.EffectiveMinFree(),
		LockWait:     tb.EffectiveLockWait(),
	}
}

// refuseNested stops a command that writes the store from waiting on the
// shared lock its own `lever tree lock` holds.
func refuseNested(what string) error {
	if treebackup.IsNestedLock() {
		return fmt.Errorf("lever tree %s: refused inside `lever tree lock` (it holds the store lock this would wait for)", what)
	}
	return nil
}

func newTreeCmd(bf BackendFactory) *cobra.Command {
	c := &cobra.Command{
		Use:   "tree",
		Short: "Snapshots of the instance tree: take, list, diff, restore, verify",
	}
	c.AddCommand(newTreeSnapshotCmd(bf), newTreeListCmd(), newTreeLsCmd(), newTreeLogCmd(), newTreeDiffCmd(),
		newTreeCatCmd(), newTreeRestoreCmd(), newTreeVerifyCmd(), newTreeDeleteCmd(), newTreeGCCmd(),
		newTreeLockCmd(), newTreePathCmd())
	return c
}

// machineStopper is the backends' view of whether the jail can write the
// tree right now.
type machineStopper interface {
	MachineStopped(ctx context.Context) (bool, error)
}

// vmOffProbe answers vm-off only while the backend reports the machine
// stopped or absent; an error or a backend that cannot tell is "none".
func vmOffProbe(b backend.Backend) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		ms, ok := b.(machineStopper)
		if !ok {
			return treebackup.QuiescentNone
		}
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if stopped, err := ms.MachineStopped(qctx); err == nil && stopped {
			return treebackup.QuiescentVMOff
		}
		return treebackup.QuiescentNone
	}
}

// interruptible runs f with the first SIGINT closing interrupt (the walk
// stops and commits what it has) and the second cancelling ctx (nothing is
// committed).
func interruptible(ctx context.Context, f func(ctx context.Context, interrupt <-chan struct{}) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	interrupt := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sig:
			close(interrupt)
		case <-done:
			return
		}
		select {
		case <-sig:
			cancel()
		case <-done:
		}
	}()
	return f(ctx, interrupt)
}

func newTreeSnapshotCmd(bf BackendFactory) *cobra.Command {
	var full, pin, noWait, quiet bool
	var limit time.Duration
	cmd := &cobra.Command{
		Use:   "snapshot",
		Args:  cobra.NoArgs,
		Short: "Take a snapshot of the tree now (exit 0 complete, 3 incomplete, 1 failed, 75 busy)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseNested("snapshot"); err != nil {
				return err
			}
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			if !e.app.TreeBackup.Enabled {
				return errors.New("tree backups are off: set `tree_backup: {enabled: true}` in lever.yaml")
			}
			o := e.snapshotOptions(treebackup.TriggerManual)
			o.Full, o.Pinned, o.NoWait = full, pin, noWait
			// Unlimited unless asked: the operator is waiting for this one.
			o.Limits.Time = limit
			if _, b, berr := resolveJailBackend(bf, "", ""); berr == nil {
				o.Quiescent = vmOffProbe(b)
			}
			var res *treebackup.SnapshotResult
			err = interruptible(cmd.Context(), func(ctx context.Context, interrupt <-chan struct{}) error {
				o.Interrupt = interrupt
				var serr error
				res, serr = treebackup.Snapshot(ctx, o)
				return serr
			})
			if errors.Is(err, treebackup.ErrBusy) {
				return cli.WithExitCode(fmt.Errorf("another tree snapshot is running: %w", err), exitTreeBusy)
			}
			if err != nil {
				return fmt.Errorf("tree snapshot failed: %w", err)
			}
			for _, w := range res.Warnings {
				cmd.PrintErrf("lever: warning: %s\n", w)
			}
			startTreeGC(cmd, e)
			if !quiet || !res.Header.Complete {
				printSnapshotLine(cmd.OutOrStdout(), res.Header)
			}
			if !res.Header.Complete {
				return cli.WithExitCode(errors.New("the tree snapshot is incomplete"), exitTreeIncomplete)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "re-read every cached file (still within the read budget)")
	cmd.Flags().BoolVar(&pin, "pin", false, "keep this snapshot until `lever tree delete` removes it")
	cmd.Flags().DurationVar(&limit, "time", 0, "time budget for the walk and the reads (0: none)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "exit 75 at once when another snapshot is running")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print nothing for a complete snapshot")
	return cmd
}

// printSnapshotLine is the one line a snapshot leaves on the terminal.
func printSnapshotLine(w io.Writer, h *treebackup.Header) {
	files := h.Counts.ByType[treebackup.TypeFile]
	if h.Complete {
		fmt.Fprintf(w, "tree snapshot %s: complete, %d files, %s new (%s, %s)\n", h.ID, files, humanBytes(h.Bytes.New), h.Trigger, h.Quiescent)
		return
	}
	reason := "incomplete"
	switch {
	case h.Root.State == treebackup.StateUnreadable:
		reason = "the tree root is unreadable"
	case h.Root.Listing == treebackup.ListingUnreadable:
		reason = "the tree root cannot be listed"
	case len(h.Partial) > 0:
		p := h.Partial[0]
		at := "the tree root"
		if p.HasAt {
			at = treebackup.Q(p.At)
		}
		reason = fmt.Sprintf("%s(%s) at %s (%d)", p.State, p.Reason, at, p.Count)
	}
	fmt.Fprintf(w, "tree snapshot INCOMPLETE: %s — snapshot %s, %d files (%s, %s)\n", reason, h.ID, files, h.Trigger, h.Quiescent)
	for _, f := range h.Flood {
		at := "the tree root"
		if f.HasAt {
			at = treebackup.Q(f.At)
		}
		fmt.Fprintf(w, "  most names listed: %s (%d)\n", at, f.Count)
	}
}

// startTreeGC is a seam: tests must not start a real `lever` process.
var startTreeGC = startDetachedGC

// startDetachedGC starts `lever tree gc --detached` after a commit, with
// both locks already released: GC takes the store lock exclusive, and this
// process must not hold a second description of it.
func startDetachedGC(cmd *cobra.Command, e *treeEnv) {
	self, err := os.Executable()
	if err != nil {
		cmd.PrintErrf("lever: warning: tree backup GC not started: %v\n", err)
		return
	}
	logDir := e.st.BringupLogDir()
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		cmd.PrintErrf("lever: warning: tree backup GC not started: %v\n", err)
		return
	}
	logf, err := os.OpenFile(filepath.Join(logDir, "tree-gc.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		cmd.PrintErrf("lever: warning: tree backup GC not started: %v\n", err)
		return
	}
	defer logf.Close()
	c := exec.Command(self, "tree", "gc", "--detached")
	c.Dir = e.app.InstanceDir()
	c.Stdin = nil
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = detachedAttr()
	if err := c.Start(); err != nil {
		cmd.PrintErrf("lever: warning: tree backup GC not started: %v\n", err)
		return
	}
	_ = c.Process.Release()
}

func newTreeListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List the tree snapshots, oldest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			s, err := e.openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			l, err := s.LockShared(treeReadWait)
			if err != nil {
				return err
			}
			defer l.Release()
			infos, strays, err := s.List()
			if err != nil {
				return err
			}
			if len(infos) == 0 {
				cmd.Printf("no tree snapshots in %s\n", s.Dir())
			} else {
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tSEQ\tTAKEN\tTRIGGER\tQUIESCENT\tSTATE\tFILES\tSIZE\tPIN")
				for _, in := range infos {
					h := in.Header
					if h == nil {
						fmt.Fprintf(tw, "%s\t-\t-\t-\t-\tINVALID\t-\t-\t\n", in.ID)
						continue
					}
					stateCol := "complete"
					if !h.Complete {
						stateCol = "INCOMPLETE"
					}
					pin := ""
					if h.Pinned {
						pin = "pinned"
					}
					fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", h.ID, h.Seq, h.Created.Local().Format("2006-01-02 15:04"),
						h.Trigger, h.Quiescent, stateCol, h.Counts.ByType[treebackup.TypeFile], humanBytes(h.Bytes.Total), pin)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			for _, n := range strays {
				cmd.PrintErrf("lever: warning: stray file in snapshots/: %s\n", treebackup.Q([]byte(n)))
			}
			return nil
		},
	}
}

// splitDash separates positional args from the paths after "--".
func splitDash(cmd *cobra.Command, args []string) (before, after []string) {
	if i := cmd.ArgsLenAtDash(); i >= 0 {
		return args[:i], args[i:]
	}
	return args, nil
}

func parsePaths(args []string) ([][]byte, error) {
	var out [][]byte
	for _, a := range args {
		p, err := treebackup.ParsePathArg(a)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// withSharedStore opens the store and holds its shared lock around f.
func withSharedStore(f func(s *treebackup.Store) error) error {
	e, err := loadTreeEnv()
	if err != nil {
		return err
	}
	s, err := e.openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	l, err := s.LockShared(treeReadWait)
	if err != nil {
		return err
	}
	defer l.Release()
	return f(s)
}

func resolveOpen(s *treebackup.Store, ref string) (string, treebackup.OpenManifest, error) {
	id, err := s.Resolve(ref)
	if err != nil {
		return "", nil, err
	}
	return id, s.Open(id), nil
}

func newTreeLsCmd() *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:   "ls <id> [<path>]",
		Args:  cobra.RangeArgs(1, 2),
		Short: "List what a snapshot holds (a path takes raw bytes or the q:\"…\" form lever prints)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var p []byte
			if len(args) == 2 {
				var err error
				if p, err = treebackup.ParsePathArg(args[1]); err != nil {
					return err
				}
			}
			return withSharedStore(func(s *treebackup.Store) error {
				_, m, err := resolveOpen(s, args[0])
				if err != nil {
					return err
				}
				return treebackup.Ls(cmd.OutOrStdout(), m, p, recursive)
			})
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "list the whole subtree")
	return cmd
}

func newTreeLogCmd() *cobra.Command {
	var n int
	var all bool
	cmd := &cobra.Command{
		Use:   "log [-n N] [--all] [-- <path>]",
		Short: "One line per snapshot with what changed, or the snapshots where one path changed",
		RunE: func(cmd *cobra.Command, args []string) error {
			before, after := splitDash(cmd, args)
			if len(before) > 0 || len(after) > 1 {
				return usageError{errors.New("tree log: give at most one path, after --")}
			}
			o := treebackup.LogOptions{N: n, All: all}
			if len(after) == 1 {
				p, err := treebackup.ParsePathArg(after[0])
				if err != nil {
					return err
				}
				o.Path, o.HasPath = p, true
			}
			return withSharedStore(func(s *treebackup.Store) error {
				infos, _, err := s.List()
				if err != nil {
					return err
				}
				snaps := make([]treebackup.LogSnapshot, 0, len(infos))
				for _, in := range infos {
					snaps = append(snaps, treebackup.LogSnapshot{ID: in.ID, Open: s.Open(in.ID)})
				}
				return treebackup.Log(cmd.OutOrStdout(), snaps, o)
			})
		},
	}
	cmd.Flags().IntVarP(&n, "n", "n", 0, "show only the last N snapshots (0: all)")
	cmd.Flags().BoolVar(&all, "all", false, "list every changed path, not the first 20")
	return cmd
}

func newTreeDiffCmd() *cobra.Command {
	var o treebackup.DiffOptions
	cmd := &cobra.Command{
		Use:   "diff <a> [<b>] [-- <path>...]",
		Short: "What changed between two snapshots (b defaults to latest)",
		RunE: func(cmd *cobra.Command, args []string) error {
			before, after := splitDash(cmd, args)
			if len(before) < 1 || len(before) > 2 {
				return usageError{errors.New("tree diff: give one or two snapshots (an id, latest or latest~N)")}
			}
			paths, err := parsePaths(after)
			if err != nil {
				return err
			}
			o.Paths = paths
			b := "latest"
			if len(before) == 2 {
				b = before[1]
			}
			return withSharedStore(func(s *treebackup.Store) error {
				_, ma, err := resolveOpen(s, before[0])
				if err != nil {
					return err
				}
				_, mb, err := resolveOpen(s, b)
				if err != nil {
					return err
				}
				o.Blobs = s
				w := bufio.NewWriter(cmd.OutOrStdout())
				_, derr := treebackup.Diff(w, ma, mb, o)
				if ferr := w.Flush(); derr == nil {
					derr = ferr
				}
				return derr
			})
		},
	}
	cmd.Flags().BoolVar(&o.Stat, "stat", false, "only the summary")
	cmd.Flags().BoolVar(&o.Meta, "meta", false, "also show changes of modification time alone")
	cmd.Flags().BoolVar(&o.NoLimit, "no-limit", false, "lift the 8 MiB output limit")
	cmd.Flags().BoolVar(&o.FullDiff, "full-diff", false, "show text changes even when more than 500 paths changed")
	return cmd
}

func newTreeCatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cat <id> <path>",
		Args:  cobra.ExactArgs(2),
		Short: "Write one file's content from a snapshot to stdout (not to a terminal)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if isTerminal(cmd.OutOrStdout()) {
				return errors.New("tree cat: stdout is a terminal; the content was written by agents, so redirect it to a file or a pager")
			}
			p, err := treebackup.ParsePathArg(args[1])
			if err != nil {
				return err
			}
			return withSharedStore(func(s *treebackup.Store) error {
				_, m, err := resolveOpen(s, args[0])
				if err != nil {
					return err
				}
				return treebackup.Cat(cmd.OutOrStdout(), m, s, p)
			})
		},
	}
}

func newTreeRestoreCmd() *cobra.Command {
	var to, links string
	var paths []string
	var keepModes, keepVCS bool
	cmd := &cobra.Command{
		Use:   "restore <id> --to <dir>",
		Args:  cobra.ExactArgs(1),
		Short: "Restore a snapshot into a new directory outside the tree",
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return usageError{errors.New("tree restore: --to <dir> is required (a new directory outside the tree)")}
			}
			sel, err := parsePaths(paths)
			if err != nil {
				return err
			}
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			target, err := filepath.Abs(to)
			if err != nil {
				return err
			}
			var rep *treebackup.RestoreReport
			err = withSharedStore(func(s *treebackup.Store) error {
				_, m, err := resolveOpen(s, args[0])
				if err != nil {
					return err
				}
				rep, err = treebackup.Restore(cmd.Context(), treebackup.RestoreOptions{
					Target:    target,
					Manifest:  m,
					Blobs:     s,
					Paths:     sel,
					Links:     treebackup.LinkPolicy(links),
					KeepModes: keepModes,
					KeepVCS:   keepVCS,
					Forbidden: []string{e.app.Tree, e.place.Store, e.st.Dir},
					InsideFD:  fsutil.InsideByIdentityFD,
					MinFree:   e.app.TreeBackup.EffectiveMinFree(),
				})
				return err
			})
			if rep != nil {
				rep.Write(cmd.OutOrStdout())
			}
			if err != nil {
				return err
			}
			if !rep.OK() {
				return errors.New("tree restore: not everything selected was restored (see the report above)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the new directory to restore into (must not exist)")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "restore only this path and what is below it (repeatable)")
	cmd.Flags().StringVar(&links, "links", "inside", "symbolic links to create: inside (only ones that cannot leave the restored tree), all, or none")
	cmd.Flags().BoolVar(&keepModes, "keep-modes", false, "apply the recorded permission bits (never setuid, setgid, sticky, group or other write)")
	cmd.Flags().BoolVar(&keepVCS, "keep-vcs", false, "write git, Mercurial and Jujutsu settings, hooks and links under their own names")
	return cmd
}

func newTreeVerifyCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "verify [--fix]",
		Args:  cobra.NoArgs,
		Short: "Re-hash every stored blob and validate every manifest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fix {
				if err := refuseNested("verify --fix"); err != nil {
					return err
				}
			}
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			s, err := e.openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			rep, err := s.Verify(fix, treeReadWait)
			if err != nil {
				return err
			}
			bad := printVerify(cmd.OutOrStdout(), rep)
			if bad {
				return errors.New("tree verify: problems found (see above)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "move damaged blobs to blobs/corrupt/ (the next snapshot of that content stores it again)")
	return cmd
}

func printVerify(w io.Writer, r *treebackup.VerifyReport) bool {
	fmt.Fprintf(w, "%d manifests, %d blobs checked\n", r.Manifests, r.Blobs)
	section := func(title string, items []string, render func(string) string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "%s:\n", title)
		for _, it := range items {
			fmt.Fprintf(w, "  %s\n", render(it))
		}
	}
	name := func(s string) string { return treebackup.Q([]byte(s)) }
	section("invalid manifests", r.Problems, termsafe.Sanitize)
	section("damaged blobs", r.BadBlobs, name)
	section("missing blobs", r.MissingBlobs, name)
	section("stray files", r.Strays, name)
	section("quarantined", r.Quarantined, name)
	for _, s := range r.DuplicateSeq {
		fmt.Fprintf(w, "duplicate seq: %d\n", s)
	}
	// Stray files are reported, not failed: Finder and copy tools leave them.
	return len(r.BadManifests)+len(r.Problems)+len(r.BadBlobs)+len(r.MissingBlobs)+len(r.DuplicateSeq) > 0
}

func (e *treeEnv) gcOptions(wait time.Duration) treebackup.GCOptions {
	last, daily, weekly := e.app.TreeBackup.EffectiveKeep()
	return treebackup.GCOptions{Keep: treebackup.Keep{Last: last, Daily: daily, Weekly: weekly}, Wait: wait}
}

func newTreeDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <id>...",
		Args:  cobra.MinimumNArgs(1),
		Short: "Delete snapshots (pinned or invalid ones too), then collect garbage",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseNested("delete"); err != nil {
				return err
			}
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			if !yes {
				cmd.Printf("delete %d snapshot(s): %s? [y/N] ", len(args), strings.Join(args, " "))
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return errors.New("tree delete: not confirmed; nothing deleted")
				}
			}
			s, err := e.openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			ids := make([]string, 0, len(args))
			for _, a := range args {
				// A literal id is taken as it is, so an invalid manifest can be deleted.
				if treebackup.ValidID(a) {
					ids = append(ids, a)
					continue
				}
				id, err := s.Resolve(a)
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			res, err := s.Delete(ids, e.gcOptions(treeReadWait))
			if res != nil {
				printGC(cmd.OutOrStdout(), res)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}

func printGC(w io.Writer, r *treebackup.GCResult) {
	switch {
	case r.Busy:
		fmt.Fprintln(w, "tree gc: the store is busy; skipped")
		return
	case r.Blocked != "":
		fmt.Fprintf(w, "tree gc: BLOCKED by %s: nothing was deleted (`lever tree verify` names the problem; `lever tree delete` removes a bad manifest)\n", treebackup.Q([]byte(r.Blocked)))
	}
	for _, id := range r.Deleted {
		fmt.Fprintf(w, "deleted snapshot %s\n", treebackup.Q([]byte(id)))
	}
	for _, n := range r.Strays {
		fmt.Fprintf(w, "stray file in snapshots/ (ignored): %s\n", treebackup.Q([]byte(n)))
	}
	for _, sum := range r.Quarantined {
		fmt.Fprintf(w, "damaged blob moved to blobs/corrupt/: %s\n", treebackup.Q([]byte(sum)))
	}
	fmt.Fprintf(w, "tree gc: %d blobs removed; %d blobs, %s kept\n", r.BlobsRemoved, r.BlobCount, humanBytes(r.StoreBytes))
}

func newTreeGCCmd() *cobra.Command {
	var detached bool
	cmd := &cobra.Command{
		Use:   "gc",
		Args:  cobra.NoArgs,
		Short: "Apply retention and remove unreferenced blobs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseNested("gc"); err != nil {
				return err
			}
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			wait := treeReadWait
			if detached {
				lowerPriority()
				wait = 0
				cmd.Printf("%s tree gc --detached\n", time.Now().UTC().Format(time.RFC3339))
			}
			s, err := e.openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			res, err := s.GC(e.gcOptions(wait))
			if res != nil {
				printGC(cmd.OutOrStdout(), res)
			}
			if err != nil {
				return err
			}
			if res.Blocked != "" {
				return fmt.Errorf("tree gc: blocked by %s", res.Blocked)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&detached, "detached", false, "run as the background GC a snapshot starts (try the lock once)")
	_ = cmd.Flags().MarkHidden("detached")
	return cmd
}

func newTreeLockCmd() *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "lock [--wait D] -- <cmd> [args...]",
		Short: "Run a command (an off-host copy) while GC cannot change the store",
		RunE: func(cmd *cobra.Command, args []string) error {
			before, after := splitDash(cmd, args)
			if len(before) > 0 || len(after) == 0 {
				return usageError{errors.New("tree lock: give the command after --")}
			}
			if err := refuseNested("lock"); err != nil {
				return err
			}
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			path, bin, err := lockCommandPath(after[0], os.Getenv("PATH"), e.app.Tree)
			if err != nil {
				return err
			}
			s, err := e.openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			l, err := s.LockShared(wait)
			if errors.Is(err, treebackup.ErrBusy) {
				return cli.WithExitCode(fmt.Errorf("tree lock: %w", err), exitTreeBusy)
			}
			if err != nil {
				return err
			}
			defer l.Release()
			latest, _ := s.Resolve("latest")
			c := exec.CommandContext(cmd.Context(), bin, after[1:]...)
			// The program sees the name it was given, not the resolved path: a
			// multi-call binary (busybox) dispatches on it.
			c.Args[0] = after[0]
			c.Env = append(lockEnv(os.Environ(), path), treebackup.EnvStore+"="+s.Dir(), treebackup.EnvLatest+"="+latest)
			c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := c.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) && ee.ExitCode() > 0 {
					return cli.WithExitCode(fmt.Errorf("tree lock: the command exited with status %d", ee.ExitCode()), ee.ExitCode())
				}
				return fmt.Errorf("tree lock: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for the lock (exit 75 after that)")
	return cmd
}

// lockCommandPath filters PATH for the command `lever tree lock` runs:
// empty and relative entries and every entry inside the tree are dropped,
// because a shell the operator runs there (`sh -c 'restic …'`) looks its
// programs up through PATH, and the tree is agent-written. The command
// itself must resolve outside the tree too. An entry whose place cannot be
// proven is dropped.
func lockCommandPath(name, pathEnv, tree string) (filtered, bin string, err error) {
	var keep []string
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		in, ierr := fsutil.InsideByIdentity(d, tree)
		if ierr != nil || in {
			continue
		}
		keep = append(keep, d)
	}
	filtered = strings.Join(keep, string(filepath.ListSeparator))
	if strings.ContainsRune(name, '/') {
		bin = name
	} else {
		for _, d := range keep {
			p := filepath.Join(d, name)
			if fi, serr := os.Stat(p); serr == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
				bin = p
				break
			}
		}
		if bin == "" {
			return "", "", fmt.Errorf("tree lock: %s: not found in PATH (entries inside the tree are not searched)", name)
		}
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fmt.Errorf("tree lock: %s: %w", name, err)
	}
	in, err := fsutil.InsideByIdentity(real, tree)
	if err != nil {
		return "", "", fmt.Errorf("tree lock: cannot prove %s is outside the tree: %w", real, err)
	}
	if in {
		return "", "", fmt.Errorf("tree lock: %s is inside the tree, where agents write; refusing to run it", real)
	}
	return filtered, real, nil
}

// lockEnv replaces PATH in env.
func lockEnv(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, treebackup.EnvStore+"=") || strings.HasPrefix(kv, treebackup.EnvLatest+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+path)
}

func newTreePathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Args:  cobra.NoArgs,
		Short: "Print the tree backup store's path",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadTreeEnv()
			if err != nil {
				return err
			}
			if err := e.place.CheckPlacement(); err != nil {
				return err
			}
			cmd.Println(e.place.Store)
			return nil
		},
	}
}

// treeSnapshotAt is the snapshot `lever stop` and `lever destroy` take once
// the machine is off. It only warns: the stop or destroy has already
// happened, and its exit status must not depend on the backup.
func treeSnapshotAt(cmd *cobra.Command, app *config.App, st state.State, trigger string, quiescent func(ctx context.Context) string) {
	if app == nil || !app.TreeBackup.Enabled || !app.TreeBackup.EffectiveAtStop() {
		return
	}
	e := treeEnvFor(app, st)
	o := e.snapshotOptions(trigger)
	o.Quiescent = quiescent
	if !hasTreeSnapshot(e) {
		cmd.Printf("first tree snapshot of %s: it reads up to %s of file content, for at most %s (a `lever tree snapshot` while the jail runs makes later stops fast)\n",
			app.Tree, humanBytes(o.Limits.Read), o.Limits.Time)
	}
	var res *treebackup.SnapshotResult
	err := interruptible(cmd.Context(), func(ctx context.Context, interrupt <-chan struct{}) error {
		o.Interrupt = interrupt
		var serr error
		res, serr = treebackup.Snapshot(ctx, o)
		return serr
	})
	if err != nil {
		if errors.Is(err, treebackup.ErrBusy) {
			cmd.PrintErrf("lever: warning: tree snapshot skipped: %v\n", err)
		} else {
			cmd.PrintErrf("lever: warning: tree snapshot failed: %v\n", err)
		}
		return
	}
	for _, w := range res.Warnings {
		cmd.PrintErrf("lever: warning: %s\n", w)
	}
	printSnapshotLine(cmd.OutOrStdout(), res.Header)
	startTreeGC(cmd, e)
}

func hasTreeSnapshot(e *treeEnv) bool {
	ents, err := os.ReadDir(filepath.Join(e.place.Store, "snapshots"))
	return err == nil && len(ents) > 0
}

// quiescentAfterStop is the label for stop and destroy: the machine is off
// when the backend says so before and after the walk.
func quiescentAfterStop(b backend.Backend) func(ctx context.Context) string { return vmOffProbe(b) }

// guestSyncOutputLimit caps what the guest's sync may print: the guest is
// not trusted, and an uncapped exec buffers all of it in this process.
const guestSyncOutputLimit = 4 << 10

// guestSync flushes the guest's write-back cache into the share before a
// keep-vm snapshot: without it the suspended agents' last writes may not
// have reached the host yet. Output past the cap is a failed sync, so the
// snapshot is labelled none.
func guestSync(ctx context.Context, b backend.Backend) error {
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err := b.JailRunner().Run(proc.WithOutputLimit(sctx, guestSyncOutputLimit), nil, "sync")
	return err
}

func quiescentKeepVM(suspended bool, syncErr error) func(ctx context.Context) string {
	q := treebackup.QuiescentNone
	if suspended && syncErr == nil {
		q = treebackup.QuiescentSuspended
	}
	return func(context.Context) string { return q }
}
