package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/testutil"
)

// oversizedListRunner caps every scion list, the hub readiness probe's
// included, the way the jail runner does past its output cap: until a delete
// of the manager's record succeeds (owner "manager"), or always (owner
// "worker").
type oversizedListRunner struct {
	*agentLifecycleRunner
	owner   string
	deleted bool
	// order is the scion verbs in the order they ran; a capped list is
	// "list(capped)".
	order []string
}

func (r *oversizedListRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && len(args) > 0 {
		r.ensureInit()
		verb := args[0]
		if verb == "-g" && len(args) > 2 {
			verb = args[2]
		}
		if verb == "list" && (r.owner == "worker" || !r.deleted) {
			r.order = append(r.order, "list(capped)")
			if isObserveList(args) {
				r.listCalls++
			}
			return proc.Result{Code: -1, Stdout: `[{"slug":"hello","activity":"AAAA`}, fmt.Errorf("limactl: %w", proc.ErrOutputLimit)
		}
		r.order = append(r.order, verb)
		res, err := r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
		if verb == "delete" && err == nil {
			r.deleted = true
		}
		return res, err
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *oversizedListRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// The manager's own record is the oversized one: --fresh gets past the hub
// readiness wait, suspends and backs up the manager, deletes it by name and
// creates it anew.
func TestStartManagerFreshDeletesAnOversizedManagerByName(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &oversizedListRunner{owner: "manager", agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: "hello",
		initPhase: "running", initContainerStatus: "Up 2 days"}}
	backups := 0
	var backupRec *scion.Agent
	var atBackup []string
	err := runApplyFresh(app, Deps{
		Scion: scion.New(r, scion.Options{}),
		BackupManagerConversation: func(_ context.Context, rec *scion.Agent) error {
			backups++
			backupRec, atBackup = rec, slices.Clone(r.order)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if backups != 1 || backupRec != nil || r.deleteCalls != 1 || r.startCalls != 1 || r.resumeCalls != 0 {
		t.Fatalf("backups %d (rec %+v), delete/start/resume = %d/%d/%d, want 1 (nil), 1/1/0", backups, backupRec, r.deleteCalls, r.startCalls, r.resumeCalls)
	}
	if !slices.Contains(atBackup, "suspend") || slices.Contains(atBackup, "delete") {
		t.Fatalf("verbs before the backup %v: want the suspend and no delete", atBackup)
	}
	var argv []string
	for _, c := range f.Calls {
		if slices.Contains(c.Args, "delete") {
			argv = c.Args
		}
	}
	if !slices.Contains(argv, "hello") {
		t.Fatalf("delete argv %v does not name the manager", argv)
	}
}

// A failed backup of an unlisted manager deletes nothing and does not
// resume it: lever could not see whether it ran before the suspend.
func TestStartManagerFreshOversizedBackupFailureKeepsTheRecord(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &oversizedListRunner{owner: "manager", agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: "hello",
		initPhase: "running", initContainerStatus: "Up 2 days"}}
	boom := errors.New("disk full")
	err := runApplyFresh(app, Deps{
		Scion:                     scion.New(r, scion.Options{}),
		BackupManagerConversation: func(context.Context, *scion.Agent) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the backup failure", err)
	}
	testutil.WantErrContaining(t, err, "lever deleted nothing")
	testutil.WantErrContaining(t, err, "stays suspended")
	testutil.WantErrContaining(t, err, "--no-backup")
	if r.deleteCalls != 0 || r.startCalls != 0 || r.resumeCalls != 0 {
		t.Fatalf("delete/start/resume = %d/%d/%d, want 0/0/0", r.deleteCalls, r.startCalls, r.resumeCalls)
	}
}

// Without --fresh an oversized list stops start-manager at its first read,
// with the recovery in the error, and nothing is changed.
func TestStartManagerFailsFastOnAnOversizedList(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &oversizedListRunner{owner: "manager", agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped"}}
	err := runApply(app, Deps{Scion: scion.New(r, scion.Options{})})
	if !errors.Is(err, scion.ErrAgentListTooLarge) {
		t.Fatalf("err = %v, want ErrAgentListTooLarge", err)
	}
	testutil.WantErrContaining(t, err, "lever up --fresh --keep-conversation")
	testutil.WantErrContaining(t, err, "lever worker purge NAME --force")
	if r.listCalls != 1 || r.deleteCalls != 0 || r.startCalls != 0 || r.resumeCalls != 0 || r.suspendCalls != 0 {
		t.Fatalf("list/delete/start/resume/suspend = %d/%d/%d/%d/%d, want 1/0/0/0/0", r.listCalls, r.deleteCalls, r.startCalls, r.resumeCalls, r.suspendCalls)
	}
}

// A worker's record is the oversized one: --fresh backs up and deletes the
// manager, then finds the list still too large, creates nothing and says to
// purge the workers.
func TestStartManagerFreshOversizedWorkerGivesThePurgeAdvice(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &oversizedListRunner{owner: "worker", agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped"}}
	backups := 0
	err := runApplyFresh(app, Deps{
		Scion:                     scion.New(r, scion.Options{}),
		BackupManagerConversation: func(context.Context, *scion.Agent) error { backups++; return nil },
	})
	if !errors.Is(err, scion.ErrAgentListTooLarge) {
		t.Fatalf("err = %v, want ErrAgentListTooLarge", err)
	}
	for _, want := range []string{`manager "hello"'s record was deleted`, "a worker's record is oversized", "nothing was created", "lever worker purge NAME --force"} {
		testutil.WantErrContaining(t, err, want)
	}
	if backups != 1 || r.deleteCalls != 1 || r.startCalls != 0 {
		t.Fatalf("backups %d, delete/start = %d/%d, want 1, 1/0", backups, r.deleteCalls, r.startCalls)
	}
	if strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("the error carries the capped list's text: %v", err)
	}
}

// A delete that fails with the list still too large may have left the
// manager's own oversized record: the error says to retry --fresh, not to
// purge workers.
func TestStartManagerFreshOversizedFailedDeleteSaysRetry(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &oversizedListRunner{owner: "manager", agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: "hello",
		initPhase: "suspended", initContainerStatus: "stopped", deleteErr: errors.New("hub: 500")}}
	err := runApplyFresh(app, Deps{Scion: scion.New(r, scion.Options{})})
	if !errors.Is(err, scion.ErrAgentListTooLarge) {
		t.Fatalf("err = %v, want ErrAgentListTooLarge", err)
	}
	testutil.WantErrContaining(t, err, "may still be the oversized one")
	testutil.WantErrContaining(t, err, "run `lever up --fresh` again")
	if r.startCalls != 0 {
		t.Fatalf("starts = %d, want none", r.startCalls)
	}
}
