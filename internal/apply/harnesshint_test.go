package apply

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/jail"
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

// The log is a terminal stream: control sequences and pane wrapping must not
// hide the phrase, and must not invent one.
func TestHarnessDeathHintTerminalStream(t *testing.T) {
	for _, c := range []struct {
		name string
		tail string
		want bool
	}{
		{"colours and cursor moves", "\x1b[2K\x1b[1G\x1b[31mYour most recent conversation \x1b[1mmoved\x1b[22m to a \x1b[38;5;208mbackground\x1b[0m session (abc)\x1b[?25h", true},
		{"cursor-forward in place of spaces", "Your most recent conversation moved\x1b[1Cto\x1b[1Ca\x1b[1Cbackground\x1b[1Csession", true},
		{"OSC title and charset switch", "\x1b]0;claude\x07\x1b(BYour most recent conversation moved to a background session\x1b]8;;\x1b\\", true},
		{"wrapped at the pane edge", "Your most recent conversation moved to a backgr\r\nound session (abc) registered from another\nmachine", true},
		{"wrapped between words with indent", "Your most recent conversation is running in the\r\n    background (session abc).", true},
		{"tabs and repeated spaces", "most  recent\tconversation is   running in the background", true},
		{"upper case", "YOUR MOST RECENT CONVERSATION MOVED TO A BACKGROUND SESSION", true},
		{"control codes only", "\x1b[2J\x1b[H\x1b[?1049h\x1b]0;x\x07", false},
		{"unterminated escape", "Error: other failure\x1b[31", false},
		{"other words", "Error: background job finished; session moved", false},
	} {
		if got := HarnessDeathHint(c.tail); (got != "") != c.want {
			t.Errorf("%s: hint=%v, want %v", c.name, got != "", c.want)
		}
	}
}

func TestStripTerminalControls(t *testing.T) {
	in := "a\x1b[1;31mb\x1b[0mc\x1b]0;title\x07d\x1bPdcs\x1b\\e\x1b7f\x1b(Bg\x1b["
	if got := stripTerminalControls(in); got != "abcdefg" {
		t.Fatalf("stripTerminalControls = %q, want %q", got, "abcdefg")
	}
	if got := stripTerminalControls("plain é text"); got != "plain é text" {
		t.Fatalf("plain text changed: %q", got)
	}
}

// The start-manager gate reads the container log when the harness dies, and
// names a stale background session as the cause, with the recovery.
func TestStartManagerDeathNamesStaleBackgroundSession(t *testing.T) {
	app, f := newObserveFirstApp(t)
	r := &agentLifecycleRunner{FakeRunner: f, slug: "hello", dieAfterLiveLists: 2}
	var refs []string
	deps := Deps{
		Scion: scion.New(r, scion.Options{}),
		// Long on purpose, as in TestStartManagerFailsWhenHarnessDiesDuringSettle:
		// the death ends the window early, and a short one races the scheduler.
		ManagerLiveRetry: RetryBudget{Attempts: 3, Interval: time.Millisecond, Settle: 10 * time.Second},
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

// Stale background records in a container that outlived the harness are the
// primary signal: the log is not read when they are found.
func TestWithHarnessDeathHintBackgroundRecordsFirst(t *testing.T) {
	ctx := context.Background()
	died := errors.Join(scion.ErrAgentDied)
	var logReads, bgRefs []string
	d := Deps{
		BackgroundSessions: func(_ context.Context, ref string) (jail.BackgroundRecords, error) {
			bgRefs = append(bgRefs, ref)
			return jail.BackgroundRecords{Stale: 2}, nil
		},
		HarnessLogTail: func(_ context.Context, ref string) (string, error) {
			logReads = append(logReads, ref)
			return "", nil
		},
	}
	got := withHarnessDeathHint(ctx, d, died, "/lever", "hello")
	if !errors.Is(got, scion.ErrAgentDied) || !strings.Contains(got.Error(), StaleBackgroundHint) {
		t.Fatalf("stale records gave no hint: %v", got)
	}
	if len(bgRefs) != 1 || bgRefs[0] != "lever--hello" || len(logReads) != 0 {
		t.Fatalf("records read for %q, log read for %q; want records only", bgRefs, logReads)
	}

	// No stale record (or only unreadable ones): the log is the fallback.
	for _, recs := range []jail.BackgroundRecords{{}, {Unreadable: 3, More: true}} {
		d.BackgroundSessions = func(context.Context, string) (jail.BackgroundRecords, error) { return recs, nil }
		d.HarnessLogTail = func(context.Context, string) (string, error) { return "other output", nil }
		if got := withHarnessDeathHint(ctx, d, died, "/lever", "hello"); got != died {
			t.Errorf("records %+v and an unknown log changed the error: %v", recs, got)
		}
		d.HarnessLogTail = func(context.Context, string) (string, error) { return staleBgLog, nil }
		if got := withHarnessDeathHint(ctx, d, died, "/lever", "hello"); !strings.Contains(got.Error(), StaleBackgroundHint) {
			t.Errorf("records %+v: the log fallback gave no hint: %v", recs, got)
		}
	}

	// The container is gone (or the probe fails): only the fallback.
	for _, berr := range []error{jail.ErrNoContainer, jail.ErrSessionsShape} {
		d.BackgroundSessions = func(context.Context, string) (jail.BackgroundRecords, error) {
			return jail.BackgroundRecords{Stale: 1}, berr
		}
		d.HarnessLogTail = func(context.Context, string) (string, error) { return "", jail.ErrNoContainer }
		if got := withHarnessDeathHint(ctx, d, died, "/lever", "hello"); got != died {
			t.Errorf("probe error %v with no log changed the error: %v", berr, got)
		}
		d.HarnessLogTail = func(context.Context, string) (string, error) { return staleBgLog, nil }
		if got := withHarnessDeathHint(ctx, d, died, "/lever", "hello"); !strings.Contains(got.Error(), StaleBackgroundHint) {
			t.Errorf("probe error %v: the log fallback gave no hint: %v", berr, got)
		}
	}

	// A records reader alone (no log reader) still names the cause.
	d = Deps{BackgroundSessions: func(context.Context, string) (jail.BackgroundRecords, error) {
		return jail.BackgroundRecords{Stale: 1}, nil
	}}
	if got := withHarnessDeathHint(ctx, d, died, "/lever", "hello"); !strings.Contains(got.Error(), StaleBackgroundHint) {
		t.Errorf("records reader alone gave no hint: %v", got)
	}
	// A non-liveness error is never probed.
	other := errors.New("boom")
	d.BackgroundSessions = func(context.Context, string) (jail.BackgroundRecords, error) {
		t.Fatal("probed on a non-liveness error")
		return jail.BackgroundRecords{}, nil
	}
	if got := withHarnessDeathHint(ctx, d, other, "/lever", "hello"); got != other {
		t.Errorf("a non-liveness error changed: %v", got)
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

// lever#202: claude --continue with no conversation in the agent home is
// named, with the fresh start as the way out, from a terminal stream too,
// and the log is never repeated.
func TestHarnessDeathHintNoConversation(t *testing.T) {
	for _, c := range []struct {
		name string
		tail string
		want string
	}{
		{"plain", "sciontool: starting harness\nNo conversation found to continue\n", NoConversationHint},
		{"colours and wrapping", "\x1b[31mNo conversation found\r\n to\x1b[1Ccontinue\x1b[0m", NoConversationHint},
		{"upper case", "NO CONVERSATION FOUND TO CONTINUE", NoConversationHint},
		{"other words", "No conversation found; continuing", ""},
		{"stale background printed last", noConvLog + "\n" + staleBgLog, StaleBackgroundHint},
		{"no conversation printed last", staleBgLog + "\n" + noConvLog, NoConversationHint},
	} {
		if got := HarnessDeathHint(c.tail); got != c.want {
			t.Errorf("%s: hint %q, want %q", c.name, got, c.want)
		}
		if got := NoConversationInLog(c.tail); got != (c.want == NoConversationHint) {
			t.Errorf("%s: NoConversationInLog = %v", c.name, got)
		}
	}
	for _, w := range []string{"no conversation to continue", "claude --continue", "never used", "lever up --fresh", "loses nothing", "backs up"} {
		if !strings.Contains(NoConversationHint, w) {
			t.Errorf("hint lacks %q", w)
		}
	}
	if ErrNoConversation.Error() != NoConversationHint {
		t.Fatalf("ErrNoConversation reads %q", ErrNoConversation)
	}

	// The gate's error wraps both the sentinel and the cause; the log is
	// not repeated.
	died := errors.Join(scion.ErrAgentDied)
	d := Deps{HarnessLogTail: func(context.Context, string) (string, error) { return "SECRET-LINE\n" + noConvLog, nil }}
	got := withHarnessDeathHint(context.Background(), d, died, "/lever", "hello")
	if !errors.Is(got, scion.ErrAgentDied) || !errors.Is(got, ErrNoConversation) || !strings.Contains(got.Error(), NoConversationHint) {
		t.Fatalf("got %v", got)
	}
	if strings.Contains(got.Error(), "SECRET") {
		t.Fatalf("log repeated: %v", got)
	}
	// The stale-background hint does not carry the sentinel.
	d.HarnessLogTail = func(context.Context, string) (string, error) { return staleBgLog, nil }
	if got := withHarnessDeathHint(context.Background(), d, died, "/lever", "hello"); errors.Is(got, ErrNoConversation) {
		t.Fatalf("stale background read as no conversation: %v", got)
	}
}
