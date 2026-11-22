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

// Spawn sends the template, the task file's task and the label, and nothing
// else; the broker picks the name.
func TestAgentSpawnPostsToTheOperatorSocket(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	task := filepath.Join(dir, "task.md")
	if err := os.WriteFile(task, []byte("review the PR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentSpawn: {body: `{"name":"s-abc234","template":"session","phase":"running"}`}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "agent", "spawn", "session", "--task-file", task, "--label", "PR 42")
	if err != nil {
		t.Fatalf("%v (%s)", err, out)
	}
	reqs := rec.all()
	var got map[string]any
	if len(reqs) != 1 || json.Unmarshal([]byte(reqs[0].body), &got) != nil {
		t.Fatalf("%+v", reqs)
	}
	if len(got) != 3 || got["template"] != "session" || got["task"] != "review the PR" || got["label"] != "PR 42" {
		t.Fatalf("body %v", got)
	}
	if !strings.Contains(out, "agent s-abc234 spawned from template session; it is running.") {
		t.Fatalf("%q", out)
	}
}

// A refusal keeps its reason and exit code, in --json too.
func TestAgentSpawnRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
		exit   int
	}{
		{http.StatusConflict, broker.ReasonTemplateFull, exitWorkerBusy},
		{http.StatusConflict, broker.ReasonTemplateBusy, exitWorkerBusy},
		{http.StatusNotFound, broker.ReasonNoTemplate, exitWorkerRefused},
		{http.StatusBadRequest, broker.ReasonBadLabel, exitWorkerRefused},
		{http.StatusBadGateway, broker.ReasonNotLive, exitWorkerResumeFailed},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentSpawn: {status: tc.status, body: "refused", header: map[string]string{broker.ReasonHeader: tc.reason}}})
		out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "agent", "spawn", "session", "--json")
		if exitCodeOf(err) != tc.exit {
			t.Fatalf("%s: exit %d (%v)", tc.reason, exitCodeOf(err), err)
		}
		var res agentResult
		if json.Unmarshal([]byte(strings.TrimSpace(out)), &res) != nil || res.Reason != tc.reason || res.OK {
			t.Fatalf("%s: %q", tc.reason, out)
		}
	}
}

// Destroy needs --force; with it, it sends stop and no_backup as given and
// reports what the broker did.
func TestAgentDestroy(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentDestroy: {
		body: `{"name":"s-abc234","was":"suspended","transcript":"20261122T120000Z","workspace":"archived to /s/agent-archives/s-abc234.tar.gz","warnings":["x\u001b[2Jy"]}`}})
	root := func() []string { return []string{"agent", "destroy", "s-abc234"} }
	if _, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), root()...); exitCodeOf(err) != exitWorkerRefused || len(rec.all()) != 0 {
		t.Fatalf("no --force: %v, %d requests", err, len(rec.all()))
	}
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), append(root(), "--force", "--stop", "--no-backup")...)
	if err != nil {
		t.Fatalf("%v (%s)", err, out)
	}
	var got broker.DestroyRequest
	if reqs := rec.all(); len(reqs) != 1 || json.Unmarshal([]byte(reqs[0].body), &got) != nil || got != (broker.DestroyRequest{Name: "s-abc234", Stop: true, NoBackup: true}) {
		t.Fatalf("%+v", rec.all())
	}
	if !strings.Contains(out, "agent s-abc234 destroyed (it was suspended): conversation backed up as 20261122T120000Z; workspace archived to") {
		t.Fatalf("%q", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("a broker string reached the terminal raw")
	}
}

// The list prints the templates and agents.
func TestAgentList(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentList: {body: `{"templates":[{"name":"session","prefix":"s","max_instances":4,"max_running":2,"instances":1,"running":1,"manager_may":true}],` +
		`"agents":[{"name":"s-abc234","template":"session","label":"PR \u001b42","created_by":"operator","created_at":"2026-11-22T12:00:00Z","state":"active","dir":"sessions/s-abc234","phase":"running"}]}`}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "agent", "list")
	if err != nil {
		t.Fatalf("%v (%s)", err, out)
	}
	for _, want := range []string{"session", "1/4", "1/2", "operator, manager", "s-abc234", "active", "running", "2026-11-22 12:00"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("a label reached the terminal raw")
	}
}

// worker verbs take a spawned agent's name: whether it exists is the
// broker's call.
func TestWorkerResumeTakesASpawnedName(t *testing.T) {
	dir := directiveTestDir(t)
	writeInstanceInto(t, dir, instanceYAML("assistant", managerYAML+scratchWorkerYAML+
		"templates:\n  - name: session\n    prefix: s\n    dir_root: sessions\n    resources: {memory: 1GiB}\n    max_instances: 2\n    max_running: 1\n"))
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorWorkerResume: {body: `{"worker":"s-abc234","phase":"running"}`}})
	if _, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "worker", "resume", "s-abc234"); err != nil {
		t.Fatal(err)
	}
	if len(rec.all()) != 1 {
		t.Fatal("not sent")
	}
}
