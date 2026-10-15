package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

const (
	stopClaimMsg = `scion resume m: failed to start agent via Hub: start_in_progress: A stop is in progress for this agent (since 06:14). It is released when the broker reports the agent stopped; start it again then. (status: 409)`
	staleID      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	otherID      = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// fakeJanitor answers State from states in turn (the last one repeats) and
// records every call.
type fakeJanitor struct {
	states    []jail.ContainerState
	stateErr  error
	size      int64
	sizeErr   error
	commitErr error
	removeErr error
	calls     []string
}

func (f *fakeJanitor) State(_ context.Context, ref string) (jail.ContainerState, error) {
	f.calls = append(f.calls, "state "+ref)
	if f.stateErr != nil {
		return jail.ContainerState{}, f.stateErr
	}
	st := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return st, nil
}

func (f *fakeJanitor) WritableSize(_ context.Context, id string) (int64, error) {
	f.calls = append(f.calls, "size "+id[:12])
	return f.size, f.sizeErr
}

func (f *fakeJanitor) Commit(_ context.Context, id, image string) error {
	f.calls = append(f.calls, "commit "+id[:12]+" "+image)
	return f.commitErr
}

func (f *fakeJanitor) Remove(_ context.Context, id string) error {
	f.calls = append(f.calls, "rm "+id[:12])
	return f.removeErr
}

func (f *fakeJanitor) PruneBackups(_ context.Context, repo string, keep int) ([]string, error) {
	f.calls = append(f.calls, fmt.Sprintf("prune %s %d", repo, keep))
	return []string{repo + ":20200101T000000Z"}, nil
}

func (f *fakeJanitor) did(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func created(id string) jail.ContainerState {
	return jail.ContainerState{ID: id, Name: "proj--m", Status: "created"}
}

func stopClaimDeps(j *fakeJanitor, logs *[]string) Deps {
	clock := time.Date(2026, 10, 15, 6, 30, 0, 0, time.UTC)
	return Deps{
		StaleContainer: j,
		StopClaimRetry: RetryBudget{Attempts: 4, Interval: time.Millisecond},
		Log:            func(f string, a ...any) { *logs = append(*logs, fmt.Sprintf(f, a...)) },
		clock:          func() time.Time { return clock },
	}
}

// resumes returns a resume that answers errs in turn, then nil.
func resumes(n *int, errs ...error) func() error {
	return func() error {
		*n++
		if *n <= len(errs) {
			return errs[*n-1]
		}
		return nil
	}
}

func TestStopClaimOtherErrorsPassThrough(t *testing.T) {
	j := &fakeJanitor{states: []jail.ContainerState{created(staleID)}}
	var logs []string
	other := errors.New("failed to start agent via Hub: start_in_progress: A start is already in progress for this agent (a user start, since 06:14). (status: 409)")
	n := 0
	err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, other))
	if err != other || n != 1 || len(j.calls) != 0 {
		t.Fatalf("a refusal that is not a stop claim must come back untouched with podman never asked: err %v, resumes %d, calls %v", err, n, j.calls)
	}
	n = 0
	if err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n)); err != nil || n != 1 || len(j.calls) != 0 {
		t.Fatalf("a resume that works: err %v, resumes %d, calls %v", err, n, j.calls)
	}
}

func TestStopClaimHappyPath(t *testing.T) {
	j := &fakeJanitor{states: []jail.ContainerState{created(staleID)}, size: 300 << 20}
	var logs []string
	n := 0
	held := errors.New(stopClaimMsg)
	err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, held, held))
	if err != nil {
		t.Fatalf("err %v", err)
	}
	want := []string{
		"state proj--m",
		"size 0123456789ab",
		"commit 0123456789ab lever.local/proj--m-backup:20261015T063000Z",
		"state proj--m",
		"rm 0123456789ab",
		"prune lever.local/proj--m-backup 2",
	}
	if strings.Join(j.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls\n %v\nwant\n %v", j.calls, want)
	}
	if n != 3 {
		t.Fatalf("resumes %d; want the refused one, one still held, then the one that works", n)
	}
	all := strings.Join(logs, "\n")
	for _, w := range []string{"stop claim", "lever.local/proj--m-backup:20261015T063000Z", "conversation is in the agent home and is kept", "is gone", "removed older backup images"} {
		if !strings.Contains(all, w) {
			t.Errorf("log lacks %q:\n%s", w, all)
		}
	}
}

func TestStopClaimCommitFailureRemovesNothing(t *testing.T) {
	j := &fakeJanitor{states: []jail.ContainerState{created(staleID)}, commitErr: errors.New("no space left on device")}
	var logs []string
	n := 0
	err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, errors.New(stopClaimMsg)))
	if !errors.Is(err, ErrStopClaimHeld) || j.did("rm ") {
		t.Fatalf("a failed backup must refuse and remove nothing: err %v, calls %v", err, j.calls)
	}
	for _, w := range []string{"no space left on device", "podman rm proj--m", "podman commit proj--m", "conversation"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q: %v", w, err)
		}
	}
	if strings.Contains(err.Error(), "transient failure clears") {
		t.Fatalf("the stop-claim refusal must not say a retry clears it: %v", err)
	}
	var ee *cli.ExitError
	if !errors.As(err, &ee) || ee.Code != cli.ExitManagerResumeFailed {
		t.Fatalf("exit code: %v", err)
	}
	if n != 1 {
		t.Fatalf("resumes %d; no retry after a refusal", n)
	}
}

func TestStopClaimNeverRemovesARunningContainer(t *testing.T) {
	for _, st := range []jail.ContainerState{
		{ID: staleID, Name: "proj--m", Status: "running", Running: true},
		{ID: staleID, Name: "proj--m", Status: "paused", Paused: true},
		{ID: staleID, Name: "proj--m", Status: "stopping"},
		{ID: staleID, Name: "proj--m", Status: "created", Running: true},
	} {
		j := &fakeJanitor{states: []jail.ContainerState{st}}
		var logs []string
		n := 0
		held := errors.New(stopClaimMsg)
		err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, held, held, held, held, held))
		if !errors.Is(err, ErrStopClaimHeld) || j.did("rm ") || j.did("commit ") {
			t.Fatalf("%+v: err %v, calls %v", st, err, j.calls)
		}
		if n != 4 {
			t.Fatalf("%+v: resumes %d; want the budget's 4", st, n)
		}
	}
}

func TestStopClaimNotReleasedInTime(t *testing.T) {
	j := &fakeJanitor{states: []jail.ContainerState{created(staleID)}}
	var logs []string
	n := 0
	held := errors.New(stopClaimMsg)
	err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, held, held, held, held, held, held))
	if !errors.Is(err, ErrStopClaimHeld) || !strings.Contains(err.Error(), "broker may be offline") {
		t.Fatalf("err %v", err)
	}
	if n != 4 || strings.Count(strings.Join(j.calls, "|"), "rm ") != 1 {
		t.Fatalf("one removal, then the bounded wait: resumes %d, calls %v", n, j.calls)
	}
}

func TestStopClaimContainerReplacedMeanwhile(t *testing.T) {
	j := &fakeJanitor{states: []jail.ContainerState{created(staleID), created(otherID)}}
	var logs []string
	n := 0
	err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, errors.New(stopClaimMsg)))
	if !errors.Is(err, ErrStopClaimHeld) || j.did("rm ") {
		t.Fatalf("a container that changed between the looks must not be removed: err %v, calls %v", err, j.calls)
	}
	j = &fakeJanitor{states: []jail.ContainerState{created(staleID), {ID: staleID, Name: "proj--m", Status: "running", Running: true}}}
	n = 0
	err = ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, errors.New(stopClaimMsg)))
	if !errors.Is(err, ErrStopClaimHeld) || j.did("rm ") {
		t.Fatalf("a container that started between the looks must not be removed: err %v, calls %v", err, j.calls)
	}
}

func TestStopClaimHugeLayerSkipsBackup(t *testing.T) {
	j := &fakeJanitor{states: []jail.ContainerState{{ID: staleID, Name: "proj--m", Status: "exited"}}, size: 17 << 30}
	var logs []string
	n := 0
	if err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, errors.New(stopClaimMsg))); err != nil {
		t.Fatal(err)
	}
	if j.did("commit ") || j.did("prune ") || !j.did("rm ") {
		t.Fatalf("calls %v", j.calls)
	}
	if all := strings.Join(logs, "\n"); !strings.Contains(all, "17.0 GiB") || !strings.Contains(all, "no backup was taken") {
		t.Fatalf("logs:\n%s", all)
	}
	// A size podman could not report is not backed up either: the agent can
	// make the measurement slow.
	j = &fakeJanitor{states: []jail.ContainerState{created(staleID)}, sizeErr: errors.New("timeout")}
	n = 0
	if err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, errors.New(stopClaimMsg))); err != nil || j.did("commit ") || !j.did("rm ") {
		t.Fatalf("err %v calls %v", err, j.calls)
	}
}

func TestStopClaimNoContainerWaits(t *testing.T) {
	j := &fakeJanitor{stateErr: fmt.Errorf("inspecting: %w", jail.ErrNoContainer)}
	var logs []string
	n := 0
	held := errors.New(stopClaimMsg)
	if err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, held, held)); err != nil {
		t.Fatal(err)
	}
	if len(j.calls) != 1 {
		t.Fatalf("calls %v", j.calls)
	}
	// Any other podman failure removes nothing and stops at once.
	j = &fakeJanitor{stateErr: errors.New("cannot connect to podman")}
	n = 0
	if err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, held)); !errors.Is(err, ErrStopClaimHeld) || n != 1 {
		t.Fatalf("err %v resumes %d", err, n)
	}
	// Unwired: the manual fix, nothing else.
	n = 0
	d := stopClaimDeps(nil, &logs)
	d.StaleContainer = nil
	if err := ResumeClearingStopClaim(context.Background(), d, "m", "/x/proj", "up", resumes(&n, held)); !errors.Is(err, ErrStopClaimHeld) || !strings.Contains(err.Error(), "podman rm proj--m") {
		t.Fatalf("err %v", err)
	}
}

func TestResumeErrorKeepsTheStopClaimRefusal(t *testing.T) {
	sc := stopClaimFailure("up", "proj--m", "why", errors.New(stopClaimMsg))
	if got := ResumeError("resume", sc); got != sc {
		t.Fatalf("ResumeError rewrapped the stop-claim refusal: %v", got)
	}
	if got := ResumeError("resume", errors.New("container state corrupt")); !strings.Contains(got.Error(), "transient failure clears") {
		t.Fatalf("other failures keep ResumeFailed: %v", got)
	}
	if got := ResumeError("resume", errors.New("forbidden (status: 403)")); !strings.Contains(got.Error(), "hub refused") {
		t.Fatalf("a hub refusal keeps HubRefusedResume: %v", got)
	}
}

// TestStartManagerClearsStopClaim runs the recovery through apply's
// start-manager step.
func TestStartManagerClearsStopClaim(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "stopped", initContainerStatus: "Created",
		resumeErr:              errors.New(stopClaimMsg),
		resumeFailsThenSucceed: 2,
	}
	j := &fakeJanitor{states: []jail.ContainerState{{ID: staleID, Name: "hello", Status: "created"}}}
	var logged logSink
	err := runApply(app, Deps{
		Scion:          scion.New(r, scion.Options{}),
		Log:            logged.logf,
		StaleContainer: j,
		StopClaimRetry: RetryBudget{Attempts: 4, Interval: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.resumeCalls != 3 || !j.did("commit ") || !j.did("rm ") || r.deleteCalls != 0 || r.startCalls != 0 {
		t.Fatalf("resumes %d deletes %d starts %d calls %v", r.resumeCalls, r.deleteCalls, r.startCalls, j.calls)
	}
}

func TestStartManagerStopClaimNotClearedSaysSo(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{
		FakeRunner: f, slug: "hello",
		initPhase: "stopped", initContainerStatus: "Created",
		resumeErr: errors.New(stopClaimMsg),
	}
	j := &fakeJanitor{states: []jail.ContainerState{{ID: staleID, Name: "hello", Status: "running", Running: true}}}
	var logged logSink
	err := runApply(app, Deps{
		Scion:          scion.New(r, scion.Options{}),
		Log:            logged.logf,
		StaleContainer: j,
		StopClaimRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond},
	})
	if !errors.Is(err, ErrStopClaimHeld) || strings.Contains(err.Error(), "transient failure clears") || j.did("rm ") {
		t.Fatalf("err %v calls %v", err, j.calls)
	}
	if r.deleteCalls != 0 || r.startCalls != 0 {
		t.Fatalf("the manager must be kept: deletes %d starts %d", r.deleteCalls, r.startCalls)
	}
}

func TestStopClaimLateRemovalRestartsTheWait(t *testing.T) {
	running := jail.ContainerState{ID: staleID, Name: "proj--m", Status: "running", Running: true}
	exited := jail.ContainerState{ID: staleID, Name: "proj--m", Status: "exited"}
	j := &fakeJanitor{states: []jail.ContainerState{running, running, running, exited}}
	var logs []string
	n := 0
	held := errors.New(stopClaimMsg)
	errs := []error{held, held, held, held, held}
	if err := ResumeClearingStopClaim(context.Background(), stopClaimDeps(j, &logs), "m", "/x/proj", "up", resumes(&n, errs...)); err != nil {
		t.Fatalf("a removal on the last attempt must still be followed by a full wait: %v (resumes %d, calls %v)", err, n, j.calls)
	}
	if !j.did("rm ") || n != 6 {
		t.Fatalf("resumes %d calls %v", n, j.calls)
	}
}

func TestIsStopClaimHeld(t *testing.T) {
	if !IsStopClaimHeld(errors.New(stopClaimMsg)) {
		t.Fatal("the hub's stop-claim 409 must match")
	}
	for _, s := range []string{
		`failed to start agent via Hub: start_in_progress: A start is already in progress for this agent (a user start, since 06:14). It will finish or be released by 06:20; run scion stop to cancel it. (status: 409)`,
		`A stop is in progress for this agent`,
		`start_in_progress: A stop is in progress for this agent (since 06:14).`,
		`conflict: agent "m" already exists in this project (status: 409)`,
		"failed to start agent via Hub: start_in_progress: A stop is in progress for this agent (since 06:14).\n(status: 409)",
	} {
		if IsStopClaimHeld(errors.New(s)) {
			t.Errorf("IsStopClaimHeld(%q) = true", s)
		}
	}
	if IsStopClaimHeld(nil) {
		t.Fatal("nil")
	}
}
