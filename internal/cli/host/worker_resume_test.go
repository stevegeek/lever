package host

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/wire"
)

func startResumeUDS(t *testing.T, dir string, routes map[string]canned) *reqRecorder {
	t.Helper()
	stateDir := filepath.Join(dir, ".lever-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return startUDSAt(t, filepath.Join(stateDir, "operator.sock"), routes)
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *cli.ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 1
}

// The command asks the broker's operator socket, never scion, and prints
// the phase the broker reports.
func TestWorkerResumePostsToTheOperatorSocket(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerResume: {body: `{"worker":"scratch","phase":"running"}`}})
	fr := proc.NewFakeRunner()
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: fr}), "worker", "resume", "scratch")
	if err != nil {
		t.Fatalf("worker resume: %v", err)
	}
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].path != broker.PathOperatorWorkerResume {
		t.Fatalf("requests = %+v", reqs)
	}
	var got wire.WorkerRequest
	if err := json.Unmarshal([]byte(reqs[0].body), &got); err != nil || got.Worker != "scratch" {
		t.Fatalf("request %q (%v)", reqs[0].body, err)
	}
	if len(fr.Calls) != 0 {
		t.Fatalf("worker resume called the jail directly: %+v", fr.Calls)
	}
	if !strings.Contains(out, "worker scratch is running") {
		t.Fatalf("output %q", out)
	}
}

// Each documented reason maps to its exit code, and --json carries the
// reason, the phase and the code on one line.
func TestWorkerResumeExitCodes(t *testing.T) {
	for _, tc := range []struct {
		status        int
		reason, phase string
		exit          int
	}{
		{http.StatusServiceUnavailable, broker.ReasonBusy, "", 75},
		{http.StatusConflict, broker.ReasonWorkerCap, "", 75},
		{http.StatusNotFound, broker.ReasonNoRecord, "", 8},
		{http.StatusConflict, broker.ReasonHubRefused, "stopped", 4},
		{http.StatusBadGateway, broker.ReasonNotLive, "", 3},
		{http.StatusBadGateway, broker.ReasonRuntimeError, "error", 3},
		{http.StatusForbidden, broker.ReasonRevoked, "", 10},
		{http.StatusConflict, broker.ReasonRoleMismatch, "", 10},
		{http.StatusConflict, broker.ReasonWorkspaceMissing, "", 10},
		{http.StatusBadGateway, broker.ReasonRuntimeUnavailable, "", 11},
		{http.StatusInternalServerError, "", "", 1},
		{http.StatusConflict, "Bad Reason!", "", 1}, // not a token: dropped
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		hdr := map[string]string{}
		if tc.reason != "" {
			hdr[broker.ReasonHeader] = tc.reason
		}
		if tc.phase != "" {
			hdr[broker.PhaseHeader] = tc.phase
		}
		startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerResume: {status: tc.status, body: "refused: details\x1b[31m", header: hdr}})
		out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "resume", "scratch", "--json")
		if got := exitCodeOf(err); got != tc.exit {
			t.Errorf("%q: exit %d, want %d (%v)", tc.reason, got, tc.exit, err)
			continue
		}
		var res workerResumeResult
		line := lastJSONLine(out)
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			t.Errorf("%q: --json line %q: %v", tc.reason, line, err)
			continue
		}
		wantReason := tc.reason
		if strings.ContainsAny(wantReason, " \x1b") {
			wantReason = ""
		}
		if res.OK || res.Worker != "scratch" || res.Reason != wantReason || res.Phase != tc.phase || res.Exit != tc.exit {
			t.Errorf("%q: result %+v", tc.reason, res)
		}
		if strings.Contains(res.Error, "\x1b") {
			t.Errorf("%q: unsanitized error text %q", tc.reason, res.Error)
		}
	}
}

func TestWorkerResumeJSONSuccess(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerResume: {body: `{"worker":"scratch","phase":"running"}`}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "resume", "scratch", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res workerResumeResult
	if err := json.Unmarshal([]byte(lastJSONLine(out)), &res); err != nil || !res.OK || res.Phase != "running" || res.Exit != 0 {
		t.Fatalf("%q: %+v %v", out, res, err)
	}
}

// An undeclared name (the manager's included) is refused locally, before
// the broker is asked.
func TestWorkerResumeUnknownWorker(t *testing.T) {
	for _, name := range []string{"nope", "assistant"} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerResume: {body: `{}`}})
		_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "resume", name)
		if exitCodeOf(err) != 10 || !strings.Contains(err.Error(), "scratch") {
			t.Fatalf("%s: %v (exit %d)", name, err, exitCodeOf(err))
		}
		if len(rec.all()) != 0 {
			t.Fatalf("%s: the broker was asked: %+v", name, rec.all())
		}
	}
}

func TestWorkerResumeWithoutTheBroker(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "resume", "scratch")
	if exitCodeOf(err) != 11 || !strings.Contains(err.Error(), "lever up") {
		t.Fatalf("%v (exit %d)", err, exitCodeOf(err))
	}
}

// `lever msg send` to a worker that is not running exits 9 (nothing sent),
// so a host program can resume it and send again; other refusals stay 1.
func TestHostMsgSendNotRunningExitCode(t *testing.T) {
	for _, tc := range []struct {
		reason string
		exit   int
	}{
		{broker.ReasonNotRunning, 9},
		{broker.ReasonNoRecord, 9},
		{"", 1},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		hdr := map[string]string{}
		if tc.reason != "" {
			hdr[broker.ReasonHeader] = tc.reason
			hdr[broker.PhaseHeader] = "suspended"
		}
		startOperatorUDS(t, dir, canned{status: http.StatusConflict, body: "worker scratch is not running (phase suspended)", header: hdr})
		_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "msg", "send", "hi", "--to", "scratch")
		if got := exitCodeOf(err); got != tc.exit || !strings.Contains(err.Error(), "not running") {
			t.Errorf("%q: exit %d (want %d): %v", tc.reason, got, tc.exit, err)
		}
	}
}

func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "{") {
			return lines[i]
		}
	}
	return ""
}

// --json's line is on stdout; the error goes to stderr only.
func TestWorkerResumeJSONIsOnStdout(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerResume: {status: http.StatusNotFound, body: "no record",
		header: map[string]string{broker.ReasonHeader: broker.ReasonNoRecord}}})
	root := stubRoot(&stubBackend{runner: proc.NewFakeRunner()})
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"worker", "resume", "scratch", "--json"})
	err := root.Execute()
	if exitCodeOf(err) != 8 {
		t.Fatalf("exit %d: %v", exitCodeOf(err), err)
	}
	var res workerResumeResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &res); err != nil || res.Reason != broker.ReasonNoRecord {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if strings.Contains(stderr.String(), `"reason"`) {
		t.Fatalf("JSON on stderr: %q", stderr.String())
	}
}

// A broker older than this lever has no route: 404 with no reason.
func TestWorkerResumeOlderBroker(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{"/other": {}})
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "resume", "scratch")
	if exitCodeOf(err) != 11 || !strings.Contains(err.Error(), "lever reload") {
		t.Fatalf("%v (exit %d)", err, exitCodeOf(err))
	}
}
