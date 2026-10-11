package apply

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// What Claude Code 2.1.281 prints when claude --continue meets a background
// session registered in another container.
const staleBgLog = "Your most recent conversation moved to a background session (abc) registered from another machine or container, so this one can't tell whether it is still running. Use `claude --resume` to pick a session, or add --fork-session to branch off a copy."

func TestHarnessDeathHint(t *testing.T) {
	for _, c := range []struct {
		tail string
		want bool
	}{
		{"scion: starting harness\n" + staleBgLog + "\n", true},
		{"Your most recent conversation is running in the background (session abc).", true},
		{"", false},
		{"Error: some other failure", false},
	} {
		got := HarnessDeathHint(c.tail)
		if (got != "") != c.want {
			t.Errorf("HarnessDeathHint(%q) = %q, want hint=%v", c.tail, got, c.want)
		}
		if got != "" && strings.Contains(got, "abc") {
			t.Errorf("hint repeats the log: %q", got)
		}
	}
	for _, w := range []string{"background session", "sessions/", "parkedJobId", "jobs/", "daemon", "transcript is kept", "lever up"} {
		if !strings.Contains(StaleBackgroundHint, w) {
			t.Errorf("hint lacks %q", w)
		}
	}
}

// The start-manager gate reads the container log when the harness dies, and
// names a stale background session as the cause, with the recovery.
func TestStartManagerDeathNamesStaleBackgroundSession(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", dieAfterLiveLists: 2}
	var refs []string
	deps := Deps{
		Scion:            scion.New(r, scion.Options{}),
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 60 * time.Millisecond},
		HarnessLogTail: func(_ context.Context, ref string) (string, error) {
			refs = append(refs, ref)
			return "SECRET-LINE\n" + staleBgLog, nil
		},
	}
	err := runApply(app, deps)
	if !errors.Is(err, scion.ErrAgentDied) {
		t.Fatalf("want ErrAgentDied, got %v", err)
	}
	if !strings.Contains(err.Error(), StaleBackgroundHint) || !strings.Contains(err.Error(), "start-manager") {
		t.Fatalf("error lacks the hint or the step: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-LINE") {
		t.Fatalf("error repeats the container log: %v", err)
	}
	if len(refs) != 1 || !strings.HasSuffix(refs[0], "--hello") {
		t.Fatalf("log read for %q, want one read of the manager container", refs)
	}
}

// An unknown cause, a failed read, or no reader leaves the error as it was.
func TestWithHarnessDeathHintLeavesOtherErrorsAlone(t *testing.T) {
	died := errors.New("start-manager: manager \"hello\" " + scion.ErrAgentDied.Error())
	diedW := errors.Join(scion.ErrAgentDied)
	other := errors.New("boom")
	ctx := context.Background()
	read := func(s string, e error) Deps {
		return Deps{HarnessLogTail: func(context.Context, string) (string, error) { return s, e }}
	}
	if got := withHarnessDeathHint(ctx, read(staleBgLog, nil), other, "/lever", "hello"); got != other {
		t.Errorf("a non-liveness error changed: %v", got)
	}
	if got := withHarnessDeathHint(ctx, Deps{}, diedW, "/lever", "hello"); got != diedW {
		t.Errorf("no reader changed the error: %v", got)
	}
	if got := withHarnessDeathHint(ctx, read("", errors.New("no container")), diedW, "/lever", "hello"); got != diedW {
		t.Errorf("a failed read changed the error: %v", got)
	}
	if got := withHarnessDeathHint(ctx, read("other output", nil), diedW, "/lever", "hello"); got != diedW {
		t.Errorf("an unknown cause changed the error: %v", got)
	}
	if got := withHarnessDeathHint(ctx, read(staleBgLog, nil), died, "/lever", "hello"); got != died {
		t.Errorf("an error that does not wrap the sentinel changed: %v", got)
	}
	notLive := errors.Join(scion.ErrAgentNotLive)
	if got := withHarnessDeathHint(ctx, read(staleBgLog, nil), notLive, "/lever", "hello"); !errors.Is(got, scion.ErrAgentNotLive) || !strings.Contains(got.Error(), StaleBackgroundHint) {
		t.Errorf("a harness that never came up got no hint: %v", got)
	}
	if got := withHarnessDeathHint(ctx, read(staleBgLog, nil), nil, "/lever", "hello"); got != nil {
		t.Errorf("nil error became %v", got)
	}
}
