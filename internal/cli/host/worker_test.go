package host

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
)

// purgeRequestOf decodes the one request a purge sent.
func purgeRequestOf(t *testing.T, rec *reqRecorder) broker.WorkerPurgeRequest {
	t.Helper()
	reqs := rec.all()
	var got broker.WorkerPurgeRequest
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].path != broker.PathOperatorWorkerPurge || json.Unmarshal([]byte(reqs[0].body), &got) != nil {
		t.Fatalf("requests %+v", reqs)
	}
	return got
}

// Purge asks the broker's operator socket, never scion: the broker deletes
// the record under the worker's lock. The workspace is not touched.
func TestWorkerPurgePostsToTheOperatorSocket(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	ws := filepath.Join(dir, "workspace", "workers", "scratch")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	product := filepath.Join(ws, "result.txt")
	if err := os.WriteFile(product, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerPurge: {body: `{"worker":"scratch","was":"suspended"}`}})
	fr := proc.NewFakeRunner() // no scripts: any jail call fails the test below
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: fr}), "worker", "purge", "scratch", "--force")
	if err != nil {
		t.Fatalf("purge: %v (%s)", err, out)
	}
	if got := purgeRequestOf(t, rec); got.Worker != "scratch" || got.Orphan {
		t.Fatalf("request %+v", got)
	}
	if len(fr.Calls) != 0 {
		t.Fatalf("purge called the jail itself: %+v", fr.Calls)
	}
	if !strings.Contains(out, `worker "scratch" purged: scion record deleted (it was suspended)`) || !strings.Contains(out, ws) {
		t.Fatalf("output %q", out)
	}
	if b, err := os.ReadFile(product); err != nil || string(b) != "keep me" {
		t.Fatalf("work product: %q, %v", b, err)
	}
}

// Without --force, for an undeclared name without --orphan, and for a
// declared one with it, purge sends nothing.
func TestWorkerPurgeRefusesBeforeTheBroker(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{"worker", "purge", "scratch"}, reasonNoForce},
		{[]string{"worker", "purge", "ghost", "--force"}, broker.ReasonNotAWorker},
		{[]string{"worker", "purge", "scratch", "--orphan", "--force"}, broker.ReasonNotOrphan},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerPurge: {body: `{"worker":"x","was":"none"}`}})
		out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), append(tc.args, "--json")...)
		if exitCodeOf(err) != exitWorkerRefused {
			t.Fatalf("%v: exit %d (%v)", tc.args, exitCodeOf(err), err)
		}
		var res workerPurgeResult
		if json.Unmarshal([]byte(strings.TrimSpace(out)), &res) != nil || res.Reason != tc.reason || res.OK {
			t.Fatalf("%v: json %q", tc.args, out)
		}
		if n := len(rec.all()); n != 0 {
			t.Fatalf("%v: sent %d request(s)", tc.args, n)
		}
	}
}

// --orphan sends the flag, and --json reports what the broker answered.
func TestWorkerPurgeOrphan(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerPurge: {body: `{"worker":"ghost","was":"running","ticket_left":true}`}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "purge", "ghost", "--orphan", "--force", "--json")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got := purgeRequestOf(t, rec); got.Worker != "ghost" || !got.Orphan {
		t.Fatalf("request %+v", got)
	}
	var res workerPurgeResult
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &res) != nil || !res.OK || res.Was != "running" || !res.TicketLeft {
		t.Fatalf("json %q", out)
	}
}

// A broker refusal keeps its reason and exit code.
func TestWorkerPurgeBrokerRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
		exit   int
	}{
		{http.StatusServiceUnavailable, broker.ReasonBusy, exitWorkerBusy},
		{http.StatusNotFound, broker.ReasonNoRecord, exitWorkerNoRecord},
		{http.StatusBadGateway, broker.ReasonRuntimeError, exitWorkerResumeFailed},
		{http.StatusForbidden, broker.ReasonNotOrphan, exitWorkerRefused},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerPurge: {status: tc.status, body: "refused",
			header: map[string]string{broker.ReasonHeader: tc.reason}}})
		_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "purge", "ghost", "--orphan", "--force")
		if exitCodeOf(err) != tc.exit {
			t.Fatalf("%s: exit %d (%v), want %d", tc.reason, exitCodeOf(err), err, tc.exit)
		}
	}
}

// With no broker on the operator socket, purge says so and touches nothing.
func TestWorkerPurgeNeedsTheBroker(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	fr := proc.NewFakeRunner()
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: fr}), "worker", "purge", "scratch", "--force")
	if exitCodeOf(err) != exitOperatorSocketMissing || len(fr.Calls) != 0 {
		t.Fatalf("exit %d (%v), jail calls %+v", exitCodeOf(err), err, fr.Calls)
	}
}
