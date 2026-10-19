package host

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/wire"
)

func writeFile(t *testing.T, p, s string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// start, recycle and new-session send --task-file's task, trimmed; without
// the flag they send none, which leaves the broker to the worker's task_file.
func TestWorkerStartAndRecycleSendTheTask(t *testing.T) {
	for _, verb := range []string{"start", "recycle", "new-session"} {
		path := map[string]string{"start": broker.PathOperatorWorkerStart, "recycle": broker.PathOperatorWorkerRecycle,
			"new-session": broker.PathOperatorWorkerNewSession}[verb]
		for _, withFile := range []bool{true, false} {
			dir := msgDir(t)
			t.Chdir(dir)
			rec := startResumeUDS(t, dir, map[string]canned{path: {body: `{"worker":"scratch","phase":"running"}`}})
			args := []string{"worker", verb, "scratch"}
			if withFile {
				args = append(args, "--task-file", writeFile(t, filepath.Join(dir, "tasks", "job.md"), "\n  close job 42\n"))
			}
			out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), args...)
			if err != nil {
				t.Fatalf("%s: %v", verb, err)
			}
			reqs := rec.all()
			var got wire.WorkerStartRequest
			if len(reqs) != 1 || reqs[0].method != http.MethodPost || json.Unmarshal([]byte(reqs[0].body), &got) != nil {
				t.Fatalf("%s: requests %+v", verb, reqs)
			}
			want := ""
			if withFile {
				want = "close job 42"
			}
			if got.Worker != "scratch" || got.Task != want {
				t.Fatalf("%s: request %+v", verb, got)
			}
			if !strings.Contains(out, "worker scratch is running") {
				t.Fatalf("%s: output %q", verb, out)
			}
		}
	}
}

func TestWorkerSuspendAndStopPostTheirRoutes(t *testing.T) {
	for verb, path := range map[string]string{"suspend": broker.PathOperatorWorkerSuspend, "stop": broker.PathOperatorWorkerStop} {
		dir := msgDir(t)
		t.Chdir(dir)
		phase := map[string]string{"suspend": "suspended", "stop": "stopped"}[verb]
		rec := startResumeUDS(t, dir, map[string]canned{path: {body: `{"worker":"scratch","phase":"` + phase + `"}`}})
		out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", verb, "scratch", "--json")
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		var res workerVerbResult
		if err := json.Unmarshal([]byte(lastJSONLine(out)), &res); err != nil || !res.OK || res.Phase != phase {
			t.Fatalf("%s: %q %+v %v", verb, out, res, err)
		}
		if reqs := rec.all(); len(reqs) != 1 || reqs[0].body != `{"worker":"scratch"}` {
			t.Fatalf("%s: %+v", verb, reqs)
		}
	}
}

// A --task-file the CLI refuses never reaches the broker: a link in the tree
// (it could name any file the operator can read), a FIFO, an oversize, empty
// or flag-shaped task, a missing file.
func TestWorkerStartTaskFileRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		make   func(t *testing.T, dir string) string
		reason string
	}{
		"tree link to a secret": {func(t *testing.T, dir string) string {
			secret := writeFile(t, filepath.Join(dir, "secret.key"), "PRIVATE KEY")
			p := filepath.Join(dir, "workspace", "jobs", "job.md")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, p); err != nil {
				t.Fatal(err)
			}
			return p
		}, reasonTaskFile},
		"fifo": {func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "job.fifo")
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			return p
		}, reasonTaskFile},
		"huge": {func(t *testing.T, dir string) string {
			return writeFile(t, filepath.Join(dir, "huge.md"), strings.Repeat("x", broker.MaxTaskFileBytes+1))
		}, reasonTaskFile},
		"over the budget": {func(t *testing.T, dir string) string {
			return writeFile(t, filepath.Join(dir, "long.md"), strings.Repeat("x", 16<<10))
		}, broker.ReasonTaskTooLong},
		"flag": {func(t *testing.T, dir string) string {
			return writeFile(t, filepath.Join(dir, "flag.md"), "--config=/etc/passwd")
		}, broker.ReasonBadTask},
		"empty": {func(t *testing.T, dir string) string {
			return writeFile(t, filepath.Join(dir, "empty.md"), " \n")
		}, reasonTaskFile},
		"missing":   {func(t *testing.T, dir string) string { return filepath.Join(dir, "nope.md") }, reasonTaskFile},
		"directory": {func(t *testing.T, dir string) string { return dir }, reasonTaskFile},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerStart: {body: `{}`}})
		p := tc.make(t, dir)
		out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "start", "scratch", "--task-file", p, "--json")
		if exitCodeOf(err) != 10 {
			t.Errorf("%s: exit %d (%v)", name, exitCodeOf(err), err)
		}
		var res workerVerbResult
		if jerr := json.Unmarshal([]byte(lastJSONLine(out)), &res); jerr != nil || res.Reason != tc.reason || res.Exit != 10 {
			t.Errorf("%s: %q %+v", name, out, res)
		}
		if strings.Contains(out, "PRIVATE") || len(rec.all()) != 0 {
			t.Errorf("%s: the broker was asked or the file leaked: %q %+v", name, out, rec.all())
		}
	}
}

// The new reasons map to their documented exit codes.
func TestWorkerVerbExitCodes(t *testing.T) {
	for _, tc := range []struct {
		verb, path string
		status     int
		reason     string
		exit       int
	}{
		{"recycle", broker.PathOperatorWorkerRecycle, http.StatusTooManyRequests, broker.ReasonRateLimited, 75},
		{"recycle", broker.PathOperatorWorkerRecycle, http.StatusConflict, broker.ReasonNotStopped, 12},
		{"recycle", broker.PathOperatorWorkerRecycle, http.StatusServiceUnavailable, broker.ReasonBusy, 75},
		{"start", broker.PathOperatorWorkerStart, http.StatusConflict, broker.ReasonTaskFixed, 10},
		{"start", broker.PathOperatorWorkerStart, http.StatusInternalServerError, broker.ReasonConfigFile, 10},
		{"start", broker.PathOperatorWorkerStart, http.StatusBadGateway, broker.ReasonNotLive, 3},
		{"start", broker.PathOperatorWorkerStart, http.StatusConflict, broker.ReasonWorkerCap, 75},
		{"start", broker.PathOperatorWorkerStart, http.StatusForbidden, broker.ReasonRevoked, 10},
		{"stop", broker.PathOperatorWorkerStop, http.StatusNotFound, broker.ReasonNoRecord, 8},
		{"suspend", broker.PathOperatorWorkerSuspend, http.StatusBadGateway, broker.ReasonRuntimeError, 3},
		{"new-session", broker.PathOperatorWorkerNewSession, http.StatusNotFound, broker.ReasonNoRecord, 8},
		{"new-session", broker.PathOperatorWorkerNewSession, http.StatusBadGateway, broker.ReasonRuntimeError, 3},
		{"new-session", broker.PathOperatorWorkerNewSession, http.StatusConflict, broker.ReasonRoleMismatch, 10},
		{"resume", broker.PathOperatorWorkerResume, http.StatusConflict, broker.ReasonImageUnqualified, 10},
		{"start", broker.PathOperatorWorkerStart, http.StatusConflict, broker.ReasonImageUnqualified, 10},
		{"new-session", broker.PathOperatorWorkerNewSession, http.StatusConflict, broker.ReasonImageUnqualified, 10},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		startResumeUDS(t, dir, map[string]canned{tc.path: {status: tc.status, body: "refused\x1b[2J", header: map[string]string{broker.ReasonHeader: tc.reason}}})
		out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", tc.verb, "scratch", "--json")
		var res workerVerbResult
		_ = json.Unmarshal([]byte(lastJSONLine(out)), &res)
		if exitCodeOf(err) != tc.exit || res.Reason != tc.reason || res.Exit != tc.exit || strings.Contains(res.Error, "\x1b") {
			t.Errorf("%s %s: exit %d %+v", tc.verb, tc.reason, exitCodeOf(err), res)
		}
	}
}

func TestWorkerVerbsWithoutOrOlderBroker(t *testing.T) {
	for _, verb := range []string{"start", "recycle", "suspend", "stop", "list"} {
		dir := msgDir(t)
		t.Chdir(dir)
		args := []string{"worker", verb, "scratch"}
		if verb == "list" {
			args = []string{"worker", "list"}
		}
		if _, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), args...); exitCodeOf(err) != 11 || !strings.Contains(err.Error(), "lever up") {
			t.Errorf("%s, no broker: %v (exit %d)", verb, err, exitCodeOf(err))
		}
		startResumeUDS(t, dir, map[string]canned{"/other": {}})
		if _, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), args...); exitCodeOf(err) != 11 || !strings.Contains(err.Error(), "lever reload") {
			t.Errorf("%s, older broker: %v (exit %d)", verb, err, exitCodeOf(err))
		}
	}
}

func TestWorkerVerbsRefuseUndeclaredNames(t *testing.T) {
	for _, verb := range []string{"start", "recycle", "suspend", "stop"} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startResumeUDS(t, dir, map[string]canned{})
		_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", verb, "assistant")
		if exitCodeOf(err) != 10 || len(rec.all()) != 0 {
			t.Errorf("%s: %v (exit %d), requests %+v", verb, err, exitCodeOf(err), rec.all())
		}
	}
}

const listBody = `{"workers":[` +
	`{"name":"scratch","phase":"running","activity":"waiting_for_input","activity_source":"worker-reported","last_activity":"2026-10-19T12:00:00Z","container":"Up 3 minutes","idle_at_prompt":true},` +
	`{"name":"other\u001b[2J","phase":"none","activity":"` + "x\\tyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy" + `","activity_source":"worker-reported","last_activity":null,"container":"","idle_at_prompt":null}]}`

func TestWorkerListJSON(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerList: {body: listBody}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if reqs := rec.all(); len(reqs) != 1 || reqs[0].method != http.MethodGet {
		t.Fatalf("requests %+v", reqs)
	}
	line := lastJSONLine(out)
	var res workerListResult
	if err := json.Unmarshal([]byte(line), &res); err != nil || !res.OK || res.Exit != 0 || len(res.Workers) != 2 {
		t.Fatalf("%q: %+v %v", line, res, err)
	}
	s, o := res.Workers[0], res.Workers[1]
	if s.IdleAtPrompt == nil || !*s.IdleAtPrompt || s.LastActivity == nil || s.ActivitySource != "worker-reported" {
		t.Fatalf("scratch %+v", s)
	}
	if strings.Contains(o.Name, "\x1b") || len(o.Activity) > maxListLabel+len("…") || strings.Contains(o.Activity, "\t") || o.IdleAtPrompt != nil {
		t.Fatalf("other not bounded and sanitized: %+v", o)
	}
	for _, raw := range []string{`"idle_at_prompt":null`, `"last_activity":null`} {
		if !strings.Contains(line, raw) {
			t.Fatalf("line lacks %s: %s", raw, line)
		}
	}
}

func TestWorkerListTable(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerList: {body: listBody}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "ACTIVITY*", "IDLE AT PROMPT*", "scratch", "waiting_for_input", "Up 3 minutes", "yes", "unknown", "the worker's word"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("escape in the table: %q", out)
	}
}

func TestWorkerListRefusal(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerList: {status: http.StatusBadGateway, body: "runtime error",
		header: map[string]string{broker.ReasonHeader: broker.ReasonRuntimeError}}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "list", "--json")
	var res workerListResult
	if exitCodeOf(err) != 1 || json.Unmarshal([]byte(lastJSONLine(out)), &res) != nil || res.OK || res.Reason != broker.ReasonRuntimeError || res.Exit != 1 {
		t.Fatalf("exit %d %q %+v", exitCodeOf(err), out, res)
	}
}

// The autostart converge asks the start route with no task; a refusal comes
// back as an error naming the reason, for up to warn with.
func TestAutostartWorker(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	app, st, err := loadAppAndState(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := autostartWorker(t.Context(), app, st, "scratch"); err == nil || !strings.Contains(err.Error(), "lever up") {
		t.Fatalf("no broker: %v", err)
	}
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerStart: {body: `{"worker":"scratch","phase":"running"}`}})
	if err := autostartWorker(t.Context(), app, st, "scratch"); err != nil {
		t.Fatal(err)
	}
	if reqs := rec.all(); len(reqs) != 1 || reqs[0].body != `{"worker":"scratch","task":""}` {
		t.Fatalf("%+v", reqs)
	}
}

func TestAutostartWorkerRefusal(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	app, st, err := loadAppAndState(nil)
	if err != nil {
		t.Fatal(err)
	}
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerStart: {status: http.StatusConflict, body: "too many workers",
		header: map[string]string{broker.ReasonHeader: broker.ReasonWorkerCap}}})
	if err := autostartWorker(t.Context(), app, st, "scratch"); err == nil || !strings.Contains(err.Error(), "worker-cap") {
		t.Fatalf("%v", err)
	}
}

// An autostart over a record the hub would rewrite fails with the broker's
// way out, for up to warn with.
func TestAutostartWorkerRewritableImage(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	app, st, err := loadAppAndState(nil)
	if err != nil {
		t.Fatal(err)
	}
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerStart: {status: http.StatusConflict, body: "nothing was changed. Run `lever worker recycle scratch`",
		header: map[string]string{broker.ReasonHeader: broker.ReasonImageUnqualified}}})
	err = autostartWorker(t.Context(), app, st, "scratch")
	if err == nil || !strings.Contains(err.Error(), broker.ReasonImageUnqualified) || !strings.Contains(err.Error(), "lever worker recycle scratch") {
		t.Fatalf("%v", err)
	}
}
