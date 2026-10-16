package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/dailyreset"
	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/state"
)

// dailyResetJob is the broker's daily new conversation for the manager, or
// nil when manager.daily_reset is off. The schedule comes from the config
// the broker read at start, and its record is a host file: the agent can
// only answer busy, which holds a day's reset off until its window ends.
// With the state directory inside the tree there is none: the agent could
// write the record, and lever keeps no conversation backup there.
func dailyResetJob(app *config.App, st state.State, configPath string) func(context.Context) {
	if app.Manager.DailyReset == "" || brokerctl.StateInsideTree(app, st) {
		return nil
	}
	c, err := dailyreset.Parse(app.Manager.DailyReset)
	if err != nil {
		return nil
	}
	s := &dailyreset.Scheduler{
		Clock:    c,
		Path:     st.DailyReset(),
		LockPath: st.DailyResetLock(),
		Now:      time.Now,
		Reset: func(ctx context.Context) (dailyreset.Outcome, string) {
			return dailyResetOutcome(ctx, st, configPath)
		},
		Log: func(msg string) {
			f, err := os.OpenFile(st.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return
			}
			defer f.Close()
			logging.New(f, "daily-reset").Info(msg)
		},
	}
	return s.Run
}

// dailyResetOutcome runs `lever conversation new --by daily` as the chat page
// does, never with --force, and reads its exit code.
func dailyResetOutcome(ctx context.Context, st state.State, configPath string) (dailyreset.Outcome, string) {
	code, err := startConversationNew(ctx, st, configPath, "daily", false)
	logPath := filepath.Join(st.BringupLogDir(), conversationNewLog)
	switch {
	case err != nil && ctx.Err() != nil:
		return dailyreset.Interrupted, ""
	case err != nil:
		return dailyreset.Failed, fmt.Sprintf("lever conversation new did not start: %v", err)
	case code == 0:
		return dailyreset.OK, ""
	case code == exitConversationBusy:
		return dailyreset.Busy, "the manager was in the middle of a turn"
	case code == exitConversationLocked:
		return dailyreset.Locked, "another change to the manager was running"
	case code == exitConversationNotRunning:
		return dailyreset.NotRunning, "the manager was not running"
	}
	return dailyreset.Failed, fmt.Sprintf("lever conversation new exited %d (see %s)", code, logPath)
}

const dailyResetRow = "daily reset"

// checkDailyReset is doctor's row for manager.daily_reset: off, or when it
// next runs and how the last run went.
func checkDailyReset(app *config.App, st state.State, now time.Time) checkResult {
	if app.Manager.DailyReset == "" {
		return checkResult{name: dailyResetRow, ok: true, detail: "off (manager.daily_reset)"}
	}
	c, err := dailyreset.Parse(app.Manager.DailyReset)
	if err != nil {
		return checkResult{name: dailyResetRow, detail: err.Error(), fix: "set manager.daily_reset to HH:MM"}
	}
	if brokerctl.StateInsideTree(app, st) {
		return checkResult{name: dailyResetRow, ok: true, detail: "off: the state directory is inside the tree, where the agent could write the reset's record",
			fix: "point `tree:` at a subdirectory that does not contain lever.yaml's directory"}
	}
	rec, err := dailyreset.Load(st.DailyReset())
	if err != nil {
		return checkResult{name: dailyResetRow, detail: "the broker cannot read its record, so it makes no daily reset: " + err.Error(),
			fix: "move " + st.DailyReset() + " aside; the broker writes a new one"}
	}
	var parts []string
	parts = append(parts, "at "+c.String()+" host time")
	next := dailyreset.NextDue(c, rec, now)
	if !next.IsZero() {
		parts = append(parts, "next "+next.Local().Format("2006-01-02 15:04 MST"))
	}
	last := "no run yet"
	if rec.Result != "" {
		last = "last " + rec.Result
		if !rec.LastAttempt.IsZero() {
			last += " at " + rec.LastAttempt.Local().Format("2006-01-02 15:04 MST")
		}
		if rec.Detail != "" {
			last += " (" + rec.Detail + ")"
		}
	}
	parts = append(parts, last)
	r := checkResult{name: dailyResetRow, ok: true, detail: strings.Join(parts, "; ")}
	logPath := filepath.Join(st.BringupLogDir(), conversationNewLog)
	switch {
	case rec.LastAttempt.After(now.Add(time.Minute)) || next.IsZero():
		r.fix = "the record is dated ahead of the host clock (was the clock wrong?); the broker sets it right at its next look, or move " + st.DailyReset() + " aside"
	case rec.Result == dailyreset.ResultFailed:
		r.fix = "read " + logPath + "; the next day's reset runs as usual, `lever conversation new` makes one now"
	case rec.Result == dailyreset.ResultGaveUp:
		r.fix = "the manager stayed busy or not running, or another change to it held the lock, through the window; `lever conversation new` makes one now"
	case rec.Result == dailyreset.ResultStarted && now.Sub(rec.LastAttempt) > 30*time.Minute:
		r.fix = "the broker did not record how the run ended; read " + logPath
	}
	return r
}
