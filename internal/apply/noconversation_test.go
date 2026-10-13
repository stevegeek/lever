package apply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// What Claude Code prints when claude --continue finds no conversation in
// the agent home (lever#202).
const noConvLog = "No conversation found to continue"

// noConvRunner is agentLifecycleRunner whose resumed manager dies (claude
// --continue finds nothing) and whose created manager stays up.
type noConvRunner struct {
	*agentLifecycleRunner
	// order records the lifecycle verbs and the test's own hooks, in order.
	order *[]string
}

func (r *noConvRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" {
		r.ensureInit()
		switch v := r.verb(args); v {
		case "start":
			r.dieAfterLiveLists = 0
			*r.order = append(*r.order, v)
		case "resume", "delete", "suspend":
			*r.order = append(*r.order, v)
		}
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *noConvRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *noConvRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// noConvCase is one start-manager run over a manager in phase whose
// resume dies with log as its container log.
type noConvCase struct {
	phase      string
	log        string
	opts       PlanOpts
	empty      func(context.Context) error // ManagerConversationEmpty (nil: not wired)
	backup     func(context.Context, *scion.Agent) error
	mounts     func(jp string) []jail.Mount
	liveChecks []error // successive CheckLiveMounts answers: nil = the mount is elsewhere
}

type noConvResult struct {
	err          error
	r            *noConvRunner
	order, logs  []string
	rearms       int
	commits      int
	proofs       int
	backups      []string // the phase of the record each backup was handed
	loggedJoined string
}

func runNoConv(t *testing.T, c noConvCase) noConvResult {
	t.Helper()
	app, f := newObserveFirstApp(t)
	var res noConvResult
	res.r = &noConvRunner{
		agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: c.phase, initContainerStatus: "Exited (1) 1 minute ago", dieAfterLiveLists: 2},
		order:                &res.order,
	}
	d := Deps{
		Scion: scion.New(res.r, scion.Options{}),
		// A settle window far longer than the two lists the death takes (a
		// short one races the scheduler), and short enough for the fresh
		// manager, which holds it whole.
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: time.Second},
		Log:              func(format string, a ...any) { res.logs = append(res.logs, fmt.Sprintf(format, a...)) },
		HarnessLogTail: func(context.Context, string) (string, error) {
			return "SECRET-LINE\n" + c.log, nil
		},
		RearmBootstrap: func(context.Context) error { res.rearms++; return nil },
		BeginSession: func(string) func() error {
			return func() error { res.commits++; return nil }
		},
	}
	if c.empty != nil {
		d.ManagerConversationEmpty = func(ctx context.Context) error {
			res.proofs++
			res.order = append(res.order, "proof")
			return c.empty(ctx)
		}
	}
	if c.backup != nil {
		d.BackupManagerConversation = func(ctx context.Context, rec *scion.Agent) error {
			res.backups = append(res.backups, rec.Phase)
			res.order = append(res.order, "backup")
			return c.backup(ctx, rec)
		}
	}
	if c.mounts != nil {
		i := 0
		if err := os.MkdirAll(filepath.Join(app.Tree, "kb"), 0o755); err != nil {
			t.Fatal(err)
		}
		mounts := c.mounts(JailPath(app.Tree, app.Tree, ""))
		d.InspectContainerMounts = func(context.Context, string) ([]jail.Mount, error) { return mounts, nil }
		d.CheckLiveMounts = func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			err := c.liveChecks[min(i, len(c.liveChecks)-1)]
			i++
			if err != nil {
				return nil, err
			}
			return []jail.LiveMountProblem{{Target: "/workspace/kb", Reason: jail.LiveMountElsewhere}}, nil
		}
	}
	res.err = Run(context.Background(), app, fillDeps(d), c.opts)
	res.loggedJoined = strings.Join(res.logs, "\n")
	return res
}

func proven(context.Context) error                 { return nil }
func backupOK(context.Context, *scion.Agent) error { return nil }

// lever#202: under `lever up`, a resume whose claude found no conversation
// to continue is replaced by a fresh start once the guest proved the home
// holds no transcript: proof, backup, delete, create, in that order, out
// loud, with a new enrolment ticket and a new session record.
func TestStartManagerStartsFreshWithoutConversation(t *testing.T) {
	for _, phase := range []string{"error", "suspended"} {
		t.Run(phase, func(t *testing.T) {
			res := runNoConv(t, noConvCase{phase: phase, log: noConvLog,
				opts: PlanOpts{FreshWithoutConversation: true}, empty: proven, backup: backupOK})
			if res.err != nil {
				t.Fatalf("Run: %v", res.err)
			}
			want := []string{"resume", "proof", "backup", "delete", "start"}
			if strings.Join(res.order, ",") != strings.Join(want, ",") {
				t.Fatalf("order = %v, want %v", res.order, want)
			}
			if (phase == "error") != (res.r.resumeForceCalls == 1) {
				t.Fatalf("resume --force calls = %d for phase %s", res.r.resumeForceCalls, phase)
			}
			if res.rearms != 1 || res.commits != 1 {
				t.Fatalf("rearms=%d commits=%d, want a new ticket and a new session record", res.rearms, res.commits)
			}
			if !strings.Contains(res.loggedJoined, "has no conversation to continue") || !strings.Contains(res.loggedJoined, "Starting it FRESH") ||
				!strings.Contains(res.loggedJoined, "nothing is lost") {
				t.Fatalf("the fresh start was not said out loud: %q", res.logs)
			}
			if strings.Contains(res.loggedJoined, "SECRET") {
				t.Fatalf("the container log was repeated: %q", res.logs)
			}
		})
	}
}

// Every way the fresh start is not taken keeps the record and reports the
// death with the hint: not proven, no prover, a failed backup, `lever
// apply` (no opt-in), another cause in the log, and a failed mount check.
func TestStartManagerKeepsRecordWhenNoConversationNotProven(t *testing.T) {
	cases := []struct {
		name      string
		c         noConvCase
		wantProof int
		wantLog   string
		wantErr   []string
		noHint    bool
	}{
		{name: "home holds transcripts",
			c:         noConvCase{phase: "error", log: noConvLog, opts: PlanOpts{FreshWithoutConversation: true}, empty: func(context.Context) error { return errors.New("its .claude/projects holds transcripts\x1b]0;x\x07") }, backup: backupOK},
			wantProof: 1, wantLog: "could not prove that the agent home holds no transcript (its .claude/projects holds transcripts)"},
		{name: "no prover wired",
			c: noConvCase{phase: "error", log: noConvLog, opts: PlanOpts{FreshWithoutConversation: true}, backup: backupOK}},
		{name: "backup fails",
			c:         noConvCase{phase: "error", log: noConvLog, opts: PlanOpts{FreshWithoutConversation: true}, empty: proven, backup: func(context.Context, *scion.Agent) error { return errors.New("disk full") }},
			wantProof: 1, wantErr: []string{"backing up its conversation failed, so the manager was NOT deleted", "disk full"}},
		{name: "lever apply",
			c: noConvCase{phase: "error", log: noConvLog, empty: proven, backup: backupOK}},
		{name: "another cause",
			c:      noConvCase{phase: "error", log: "Error: something else", opts: PlanOpts{FreshWithoutConversation: true}, empty: proven, backup: backupOK},
			noHint: true},
		{name: "stale background session is the later cause",
			c:      noConvCase{phase: "error", log: noConvLog + "\n" + staleBgLog, opts: PlanOpts{FreshWithoutConversation: true}, empty: proven, backup: backupOK},
			noHint: true, wantErr: []string{StaleBackgroundHint}},
		{name: "mount check failed",
			c: noConvCase{phase: "suspended", log: noConvLog, opts: PlanOpts{FreshWithoutConversation: true}, empty: proven, backup: backupOK,
				mounts: func(jp string) []jail.Mount {
					return []jail.Mount{{Source: path.Join(jp, "kb"), Destination: "/workspace/kb"}}
				},
				liveChecks: []error{jail.ErrContainerNotRunning, nil}},
			wantErr: []string{`"kb" is mounted from another place`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runNoConv(t, tc.c)
			if !errors.Is(res.err, scion.ErrAgentDied) {
				t.Fatalf("want ErrAgentDied, got %v", res.err)
			}
			if !tc.noHint && !errors.Is(res.err, ErrNoConversation) {
				t.Fatalf("the error lacks the no-conversation cause: %v", res.err)
			}
			if tc.noHint && errors.Is(res.err, ErrNoConversation) {
				t.Fatalf("another cause read as no conversation: %v", res.err)
			}
			if res.r.deleteCalls != 0 || res.r.startCalls != 0 || len(res.backups) != 0 && tc.name != "backup fails" {
				t.Fatalf("deleted=%d started=%d backups=%v; the record must be kept", res.r.deleteCalls, res.r.startCalls, res.backups)
			}
			if res.proofs != tc.wantProof {
				t.Fatalf("proofs = %d, want %d", res.proofs, tc.wantProof)
			}
			if tc.wantLog != "" && !strings.Contains(res.loggedJoined, tc.wantLog) {
				t.Fatalf("logs %q, want %q", res.logs, tc.wantLog)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(res.err.Error(), w) {
					t.Fatalf("err %q lacks %q", res.err, w)
				}
			}
			if strings.Contains(res.err.Error()+res.loggedJoined, "SECRET") {
				t.Fatalf("the container log was repeated: %v", res.err)
			}
		})
	}
}

// A fresh start that itself dies is reported as that death: one fresh start
// per run, never a loop.
func TestStartManagerFreshWithoutConversationOnce(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", initPhase: "error", initContainerStatus: "Exited (1)", dieAfterLiveLists: 2}
	proofs := 0
	err := Run(context.Background(), app, fillDeps(Deps{
		Scion:            scion.New(r, scion.Options{}),
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 10 * time.Second},
		HarnessLogTail:   func(context.Context, string) (string, error) { return noConvLog, nil },
		ManagerConversationEmpty: func(context.Context) error {
			proofs++
			return nil
		},
	}), PlanOpts{FreshWithoutConversation: true})
	if !errors.Is(err, scion.ErrAgentDied) && !errors.Is(err, scion.ErrAgentNotLive) {
		t.Fatalf("want the second death, got %v", err)
	}
	if proofs != 1 || r.deleteCalls != 1 || r.startCalls != 1 {
		t.Fatalf("proofs=%d deletes=%d starts=%d, want one fresh start", proofs, r.deleteCalls, r.startCalls)
	}
}
