package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/dailyreset"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// TestNewConversationRunningOnly: --by daily leaves a manager that is not
// running alone with its own exit code, and a busy one with exit 5.
func TestNewConversationRunningOnly(t *testing.T) {
	req := newConvBy["daily"]
	if !req.RunningOnly || req.Force {
		t.Fatalf("daily request = %+v", req)
	}
	for _, phase := range []string{scion.PhaseSuspended, scion.PhaseStopped, scion.PhaseError, "starting"} {
		f, _ := newConvFixtureWith(t)
		f.sc.phase = phase
		_, _, err := f.ops.newConversation(context.Background(), req)
		var ee *cli.ExitError
		if !errors.As(err, &ee) || ee.Code != exitConversationNotRunning || len(f.sc.calls) != 0 {
			t.Fatalf("phase %q: err %v calls %q", phase, err, f.calls())
		}
	}
	f, _ := newConvFixtureWith(t)
	f.sc.activity = "working"
	_, _, err := f.ops.newConversation(context.Background(), req)
	var ee *cli.ExitError
	if !errors.As(err, &ee) || ee.Code != exitConversationBusy || len(f.sc.calls) != 0 {
		t.Fatalf("busy: err %v calls %q", err, f.calls())
	}
	f, sess := newConvFixtureWith(t)
	if _, _, err := f.ops.newConversation(context.Background(), req); err != nil || f.calls() != "suspend,stop,resume-fresh" {
		t.Fatalf("running: err %v calls %q", err, f.calls())
	}
	// Recorded as any fresh start at the operator's word, so contacts keep
	// posting after a daily reset.
	if sess.began != 1 || sess.committed != 1 {
		t.Fatalf("session = %+v", sess)
	}
}

// TestDailyResetOutcome: the broker runs `conversation new --by daily`,
// never forced, and reads its exit code.
func TestDailyResetOutcome(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	old := startConversationNew
	t.Cleanup(func() { startConversationNew = old })
	code := 0
	var runErr error
	startConversationNew = func(_ context.Context, _ state.State, cfg, by string, force bool) (int, error) {
		if cfg != "/inst/lever.yaml" || by != "daily" || force {
			t.Errorf("run %q --by %q force %v", cfg, by, force)
		}
		return code, runErr
	}
	for _, tc := range []struct {
		code int
		err  error
		want dailyreset.Outcome
	}{
		{0, nil, dailyreset.OK},
		{exitConversationBusy, nil, dailyreset.Busy},
		{exitConversationLocked, nil, dailyreset.Locked},
		{exitConversationNotRunning, nil, dailyreset.NotRunning},
		{1, nil, dailyreset.Failed},
		{0, errors.New("exec failed"), dailyreset.Failed},
	} {
		code, runErr = tc.code, tc.err
		if got, _ := dailyResetOutcome(context.Background(), st, "/inst/lever.yaml"); got != tc.want {
			t.Errorf("code %d err %v: %v, want %v", tc.code, tc.err, got, tc.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, runErr = 0, context.Canceled
	if got, _ := dailyResetOutcome(ctx, st, "/inst/lever.yaml"); got != dailyreset.Interrupted {
		t.Errorf("cancelled: %v", got)
	}
}

func TestDailyResetJobWiring(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, "state")}
	app := &config.App{Name: "mgr", Tree: filepath.Join(root, "tree")}
	if dailyResetJob(app, st, "/inst/lever.yaml") != nil {
		t.Error("wired while off")
	}
	app.Manager.DailyReset = "06:00"
	if dailyResetJob(app, st, "/inst/lever.yaml") == nil {
		t.Error("not wired")
	}
	in := &config.App{Name: "mgr", Tree: root, Manager: config.Manager{DailyReset: "06:00"}}
	if dailyResetJob(in, st, "/inst/lever.yaml") != nil {
		t.Error("wired with the state directory inside the tree")
	}
}

func TestCheckDailyReset(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, "state")}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	app := &config.App{Name: "mgr", Tree: filepath.Join(root, "tree")}
	now := time.Date(2026, time.October, 10, 5, 0, 0, 0, time.Local)
	if r := checkDailyReset(app, st, now); !r.ok || r.fix != "" || !strings.HasPrefix(r.detail, "off") {
		t.Fatalf("off: %+v", r)
	}
	app.Manager.DailyReset = "06:00"
	r := checkDailyReset(app, st, now)
	if !r.ok || r.fix != "" || !strings.Contains(r.detail, "at 06:00") || !strings.Contains(r.detail, "next 2026-10-10 06:00") || !strings.Contains(r.detail, "no run yet") {
		t.Fatalf("fresh: %+v", r)
	}
	save := func(s dailyreset.State) {
		t.Helper()
		if err := dailyreset.Save(st.DailyReset(), s); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, time.October, 9, 6, 0, 0, 0, time.Local)
	save(dailyreset.State{Day: "2026-10-09", Done: true, LastAttempt: at, Result: dailyreset.ResultOK})
	if r := checkDailyReset(app, st, now); !r.ok || r.fix != "" || !strings.Contains(r.detail, "last ok") {
		t.Fatalf("ok: %+v", r)
	}
	for _, res := range []string{dailyreset.ResultFailed, dailyreset.ResultGaveUp, dailyreset.ResultStarted} {
		save(dailyreset.State{Day: "2026-10-09", Done: true, LastAttempt: at, Result: res, Detail: "why"})
		if r := checkDailyReset(app, st, now); !r.ok || r.fix == "" || !strings.Contains(r.detail, "last "+res) {
			t.Fatalf("%s: %+v", res, r)
		}
	}
	save(dailyreset.State{Day: "2099-01-01", Done: true, LastAttempt: at.AddDate(73, 0, 0), Result: dailyreset.ResultOK})
	if r := checkDailyReset(app, st, now); !r.ok || !strings.Contains(r.fix, "ahead of the host clock") {
		t.Fatalf("future: %+v", r)
	}
	if err := os.WriteFile(st.DailyReset(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkDailyReset(app, st, now); r.ok {
		t.Fatalf("unreadable: %+v", r)
	}
	in := &config.App{Name: "mgr", Tree: root, Manager: config.Manager{DailyReset: "06:00"}}
	if r := checkDailyReset(in, st, now); !r.ok || r.fix == "" || !strings.Contains(r.detail, "inside the tree") {
		t.Fatalf("inside: %+v", r)
	}
}
