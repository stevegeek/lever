package host

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
)

func TestParseRemoteVerb(t *testing.T) {
	ok := map[string]remoteVerb{
		"spawn session":                       {verb: "spawn", template: "session"},
		"spawn session PR 42: fix  it":        {verb: "spawn", template: "session", label: "PR 42: fix it"},
		"  spawn   session  --task x  ":       {verb: "spawn", template: "session", label: "--task x"},
		"spawn session café ☕":                {verb: "spawn", template: "session", label: "café ☕"},
		"destroy s-abc234":                    {verb: "destroy", name: "s-abc234"},
		"destroy s-abc234 --stop":             {verb: "destroy", name: "s-abc234", stop: true},
		"suspend s-abc234":                    {verb: "suspend", name: "s-abc234"},
		"resume s-abc234":                     {verb: "resume", name: "s-abc234"},
		"list":                                {verb: "list"},
		"list --json":                         {verb: "list"},
		"spawn session --memory 8GiB":         {verb: "spawn", template: "session", memory: "8GiB"},
		"spawn session --memory 1536MiB PR 7": {verb: "spawn", template: "session", memory: "1536MiB", label: "PR 7"},
		"spawn session PR --memory 8GiB":      {verb: "spawn", template: "session", label: "PR --memory 8GiB"},
		"spawn session " + strings.Repeat("é", broker.MaxAgentLabel): {verb: "spawn", template: "session", label: strings.Repeat("é", broker.MaxAgentLabel)},
	}
	for line, want := range ok {
		got, err := parseRemoteVerb(line)
		if err != nil || got != want {
			t.Errorf("%q: %+v, %v; want %+v", line, got, err, want)
		}
	}
	for _, line := range []string{
		"", "   ",
		"spawn", "spawn Session", "spawn ../x", "spawn -t", "spawn session\tlabel", "spawn session\nlist",
		"spawn session a b", "spawn session a​b", "spawn session a‮b", "spawn session \x1b[2J",
		"spawn session " + strings.Repeat("x", broker.MaxAgentLabel+1),
		"spawn session --memory", "spawn session --memory 8gb", "spawn session --memory 8Gi", "spawn session --memory 8 GiB",
		"spawn session --memory=8GiB x", "spawn session --memory -8GiB", "spawn session --memory ../8GiB", "spawn session --memory 99999999GiB",
		"spawn session \xff",
		"destroy", "destroy s-abc234 --force", "destroy s-abc234 --stop --stop", "destroy --stop s-abc234", "destroy S-ABC",
		"destroy s-abc234 --no-backup",
		"suspend", "suspend a b", "resume -x", "stop s-abc234", "list --all", "list x",
		"purge s-abc234", "sh -c id", "scp -t .", "rsync --server", "git-upload-pack x",
		"spawn session " + strings.Repeat("a ", maxRemoteCommand),
	} {
		if v, err := parseRemoteVerb(line); err == nil {
			t.Errorf("%q: accepted as %+v", line, v)
		}
	}
}

// Whatever the caller sends, an accepted command has only names where a
// name goes, a clean label, and a verb of the grammar.
func FuzzParseRemoteVerb(f *testing.F) {
	for _, s := range []string{"spawn session PR 42", "destroy s-abc234 --stop", "list --json", "resume x", "spawn a ‮", "\x00"} {
		f.Add(s)
	}
	name := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	f.Fuzz(func(t *testing.T, line string) {
		v, err := parseRemoteVerb(line)
		if err != nil {
			if v != (remoteVerb{}) {
				t.Fatalf("a refusal returned %+v", v)
			}
			return
		}
		switch v.verb {
		case "spawn":
			if !name.MatchString(v.template) || v.name != "" || v.stop {
				t.Fatalf("%+v", v)
			}
			if !utf8.ValidString(v.label) || utf8.RuneCountInString(v.label) > broker.MaxAgentLabel ||
				strings.Contains(v.label, "  ") || v.label != strings.TrimSpace(v.label) {
				t.Fatalf("label %q", v.label)
			}
			for _, r := range v.label {
				if r != ' ' && (!unicode.IsPrint(r) || unicode.Is(unicode.Cf, r)) {
					t.Fatalf("label %q has %U", v.label, r)
				}
			}
		case "destroy", "suspend", "resume":
			if !name.MatchString(v.name) || v.template != "" || v.label != "" || v.stop && v.verb != "destroy" {
				t.Fatalf("%+v", v)
			}
		case "list":
			if v != (remoteVerb{verb: "list"}) {
				t.Fatalf("%+v", v)
			}
		default:
			t.Fatalf("verb %q", v.verb)
		}
	})
}

// remoteExec runs the forced command as sshd would: the caller's command
// in SSH_ORIGINAL_COMMAND and its stdin.
func remoteExec(t *testing.T, command, stdin string, flags ...string) (agentResult, string, error) {
	t.Helper()
	t.Setenv(remoteCommandEnv, command)
	root := stubRoot(&stubBackend{runner: proc.NewFakeRunner()})
	root.SetIn(strings.NewReader(stdin))
	if flags == nil {
		flags = []string{"--templates", "session,review", "--as", "factory"}
	}
	out, err := clitest.Exec(t, root, append([]string{"agent", "remote-verb"}, flags...)...)
	var res agentResult
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &res)
	return res, out, err
}

// registryWith is the broker's registry list: one agent of the caller's,
// one the operator spawned, one of another template and one destroyed.
var registryWith = canned{body: `{"memory":{"budget_bytes":32212254720,"used_bytes":8589934592,"free_bytes":23622320128},"templates":[{"name":"session","prefix":"s","max_instances":4,"max_running":2,"instances":3,"running":1,"memory_sizes":[{"name":"4GiB","bytes":4294967296},{"name":"8GiB","bytes":8589934592},{"name":"x;rm","bytes":1}],"memory_default":"4GiB"},{"name":"secret","prefix":"x","max_instances":1,"max_running":1}],"agents":[` +
	`{"name":"s-mine01","template":"session","label":"PR 42","created_by":"ssh:factory","created_at":"2026-11-22T12:00:00Z","state":"active","phase":"running","memory":"8GiB","memory_bytes":8589934592},` +
	`{"name":"s-theirs","template":"session","created_by":"operator","created_at":"2026-11-22T12:00:00Z","state":"active"},` +
	`{"name":"s-other1","template":"session","created_by":"ssh:other","created_at":"2026-11-22T12:00:00Z","state":"active"},` +
	`{"name":"x-mine02","template":"secret","created_by":"ssh:factory","created_at":"2026-11-22T12:00:00Z","state":"active"},` +
	`{"name":"s-gone01","template":"session","created_by":"ssh:factory","created_at":"2026-11-22T12:00:00Z","state":"destroyed"}]}`}

// Spawn sends the template, the brief from stdin, the label from the
// command and the caller's name, and prints one JSON line.
func TestRemoteVerbSpawn(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentSpawn: {body: `{"name":"s-abc234","template":"session","phase":"running"}`}})
	res, out, err := remoteExec(t, "spawn session PR 42", "review the PR\n")
	if err != nil || !res.OK || res.Name != "s-abc234" || res.Phase != "running" || res.Exit != 0 {
		t.Fatalf("%v: %s", err, out)
	}
	reqs := rec.all()
	var got map[string]any
	if len(reqs) != 1 || json.Unmarshal([]byte(reqs[0].body), &got) != nil {
		t.Fatalf("%+v", reqs)
	}
	if len(got) != 4 || got["template"] != "session" || got["task"] != "review the PR" || got["label"] != "PR 42" || got["by"] != "ssh:factory" {
		t.Fatalf("body %v", got)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("not one line: %q", out)
	}
}

// --memory goes to the broker; the size it answers is printed.
func TestRemoteVerbSpawnMemory(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentSpawn: {body: `{"name":"s-abc234","template":"session","phase":"running","memory":"8GiB"}`}})
	res, out, err := remoteExec(t, "spawn session --memory 8GiB PR 42", "")
	if err != nil || !res.OK || res.Memory != "8GiB" {
		t.Fatalf("%v: %s", err, out)
	}
	var got map[string]any
	reqs := rec.all()
	if len(reqs) != 1 || json.Unmarshal([]byte(reqs[0].body), &got) != nil || got["memory"] != "8GiB" || got["label"] != "PR 42" {
		t.Fatalf("%+v", reqs)
	}
}

// Every refusal is one JSON line with a reason and an exit code, and sends
// nothing the broker would act on.
func TestRemoteVerbRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, command, stdin string
		flags                []string
		reason               string
		exit                 int
	}{
		{name: "a template not listed", command: "spawn secret", reason: broker.ReasonNoTemplate, exit: exitWorkerRefused},
		{name: "a shell", command: "sh -c id", reason: broker.ReasonBadRequest, exit: exitWorkerRefused},
		{name: "a control character", command: "spawn session \x1b[2J", reason: broker.ReasonBadRequest, exit: exitWorkerRefused},
		{name: "a brief too long", command: "spawn session", stdin: strings.Repeat("x", broker.MaxTaskFileBytes+1), reason: broker.ReasonBadRequest, exit: exitWorkerRefused},
		{name: "a brief not UTF-8", command: "spawn session", stdin: "\xff", reason: broker.ReasonBadRequest, exit: exitWorkerRefused},
		{name: "the operator's agent", command: "destroy s-theirs", reason: broker.ReasonNotSpawned, exit: exitWorkerRefused},
		{name: "another key's agent", command: "suspend s-other1", reason: broker.ReasonNotSpawned, exit: exitWorkerRefused},
		{name: "its agent of a template no longer listed", command: "resume x-mine02", reason: broker.ReasonNotSpawned, exit: exitWorkerRefused},
		{name: "its destroyed agent", command: "destroy s-gone01", reason: broker.ReasonNotSpawned, exit: exitWorkerRefused},
		{name: "a configured worker", command: "suspend scratch", reason: broker.ReasonNotSpawned, exit: exitWorkerRefused},
		{name: "no --as", command: "list", flags: []string{"--templates", "session"}, exit: 1},
		{name: "a bad --as", command: "list", flags: []string{"--templates", "session", "--as", "Fac:tory"}, exit: 1},
		{name: "no --templates", command: "list", flags: []string{"--as", "factory"}, exit: 1},
		{name: "a bad --templates", command: "list", flags: []string{"--templates", "../x", "--as", "factory"}, exit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := msgDir(t)
			t.Chdir(dir)
			rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentList: registryWith})
			res, out, err := remoteExec(t, tc.command, tc.stdin, tc.flags...)
			if exitCodeOf(err) != tc.exit || res.OK || res.Reason != tc.reason || res.Exit != tc.exit || res.Error == "" {
				t.Fatalf("exit %d (%v): %s", exitCodeOf(err), err, out)
			}
			for _, r := range rec.all() {
				if r.path != broker.PathOperatorAgentList {
					t.Fatalf("sent %s", r.path)
				}
			}
		})
	}
}

// Run by hand, with no SSH_ORIGINAL_COMMAND, it refuses.
func TestRemoteVerbNeedsSSH(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	root := stubRoot(&stubBackend{runner: proc.NewFakeRunner()})
	out, err := clitest.Exec(t, root, "agent", "remote-verb", "--templates", "session", "--as", "factory")
	var res agentResult
	if exitCodeOf(err) != exitWorkerRefused || json.Unmarshal([]byte(strings.TrimSpace(out)), &res) != nil ||
		res.Reason != broker.ReasonBadRequest || res.Exit != exitWorkerRefused || !strings.Contains(res.Error, remoteCommandEnv) {
		t.Fatalf("%v: %s", err, out)
	}
}

// Destroy, suspend and resume reach the caller's own agent, and destroy
// names the caller, so the broker checks it too and keeps the backup.
func TestRemoteVerbActsOnItsOwnAgent(t *testing.T) {
	for _, tc := range []struct{ command, path, want string }{
		{"destroy s-mine01 --stop", broker.PathOperatorAgentDestroy, `{"name":"s-mine01","stop":true,"by":"ssh:factory"}`},
		{"destroy s-mine01", broker.PathOperatorAgentDestroy, `{"name":"s-mine01","by":"ssh:factory"}`},
		{"suspend s-mine01", broker.PathOperatorAgentSuspend, `{"name":"s-mine01","by":"ssh:factory"}`},
		{"resume s-mine01", broker.PathOperatorAgentResume, `{"name":"s-mine01","by":"ssh:factory"}`},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startResumeUDS(t, dir, map[string]canned{
			broker.PathOperatorAgentList:    registryWith,
			broker.PathOperatorAgentDestroy: {body: `{"name":"s-mine01","was":"running","transcript":"20261122T120000Z","workspace":"archived"}`},
			broker.PathOperatorAgentSuspend: {body: `{"worker":"s-mine01","phase":"suspended"}`},
			broker.PathOperatorAgentResume:  {body: `{"worker":"s-mine01","phase":"running"}`},
		})
		res, out, err := remoteExec(t, tc.command, "")
		if err != nil || !res.OK || res.Name != "s-mine01" {
			t.Fatalf("%s: %v: %s", tc.command, err, out)
		}
		reqs := rec.all()
		if len(reqs) != 2 || reqs[0].path != broker.PathOperatorAgentList || reqs[1].path != tc.path || strings.TrimSpace(reqs[1].body) != tc.want {
			t.Fatalf("%s: %+v", tc.command, reqs)
		}
		if tc.path == broker.PathOperatorAgentDestroy && (res.Transcript != "20261122T120000Z" || res.Was != "running") {
			t.Fatalf("%s: %s", tc.command, out)
		}
	}
}

// A broker refusal keeps its reason and exit code, but not the broker's
// text, which is the operator's: it can name other agents and host paths.
func TestRemoteVerbBrokerRefusal(t *testing.T) {
	for _, reason := range []string{broker.ReasonRateLimited, broker.ReasonTemplateBusy, broker.ReasonRuntimeError, "x\u001b[2J"} {
		dir := msgDir(t)
		t.Chdir(dir)
		startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentSpawn: {status: http.StatusConflict,
			body:   "template session allows 1 running: s-theirs1 is running; see /home/op/inst/workspace/sessions",
			header: map[string]string{broker.ReasonHeader: reason}}})
		res, out, err := remoteExec(t, "spawn session", "")
		if exitCodeOf(err) == 0 || res.OK || res.Error == "" {
			t.Fatalf("%s: %v: %s", reason, err, out)
		}
		if strings.Contains(out, "s-theirs1") || strings.Contains(out, "/home/op") || strings.Contains(out, "\x1b") {
			t.Fatalf("%s: the broker's text reached the caller: %s", reason, out)
		}
	}
}

// A destroy's workspace line and warnings name host paths: the caller sees
// neither.
func TestRemoteVerbDestroyHidesHostPaths(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{
		broker.PathOperatorAgentList:    registryWith,
		broker.PathOperatorAgentDestroy: {body: `{"name":"s-mine01","was":"running","workspace":"archived to /home/op/x.tar.gz","warnings":["/home/op/y failed"]}`},
	})
	res, out, err := remoteExec(t, "destroy s-mine01", "")
	if err != nil || !res.OK || len(res.Warnings) != 1 || strings.Contains(out, "/home/op") {
		t.Fatalf("%v: %s", err, out)
	}
}

// With no config or no broker, the caller learns no host path.
func TestRemoteVerbHidesHostErrors(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	res, out, err := remoteExec(t, "list", "", "--templates", "session", "--as", "factory", "--config", dir+"/nowhere/lever.yaml")
	if exitCodeOf(err) == 0 || res.OK || strings.Contains(out, dir) {
		t.Fatalf("no config: %v: %s", err, out)
	}
	res, out, err = remoteExec(t, "list", "") // no broker serves the socket
	if exitCodeOf(err) == 0 || res.OK || strings.Contains(out, dir) {
		t.Fatalf("no broker: %v: %s", err, out)
	}
}

// A key runs at most remoteSlots commands at once.
func TestRemoteSlots(t *testing.T) {
	dir := t.TempDir()
	var held []func()
	for range remoteSlots {
		release, err := takeRemoteSlot(dir, "factory")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	if _, err := takeRemoteSlot(dir, "factory"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("slot %d: %v", remoteSlots+1, err)
	}
	if release, err := takeRemoteSlot(dir, "other"); err != nil {
		t.Fatalf("another key: %v", err)
	} else {
		release()
	}
	held[0]()
	if release, err := takeRemoteSlot(dir, "factory"); err != nil {
		t.Fatalf("a freed slot: %v", err)
	} else {
		release()
	}
	for _, r := range held[1:] {
		r()
	}
}

// list shows the listed templates and the caller's live agents only.
func TestRemoteVerbList(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentList: registryWith})
	t.Setenv(remoteCommandEnv, "list --json")
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "agent", "remote-verb", "--templates", "session", "--as", "factory")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got remoteList
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !got.OK || len(got.Templates) != 1 || got.Templates[0].Name != "session" || got.Templates[0].Running != 1 {
		t.Fatalf("templates %+v", got.Templates)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "s-mine01" || got.Agents[0].Label != "PR 42" || got.Agents[0].Phase != "running" ||
		got.Agents[0].Memory != "8GiB" || got.Agents[0].MemoryBytes != 8<<30 {
		t.Fatalf("agents %+v", got.Agents)
	}
	// Sizes not in the fixed format are dropped, never relayed.
	if tm := got.Templates[0]; strings.Join(tm.MemorySizes, ",") != "4GiB,8GiB" || tm.MemoryDefault != "4GiB" {
		t.Fatalf("template sizes %+v", tm)
	}
	if m := got.Memory; m == nil || m.BudgetBytes != 30<<30 || m.UsedBytes != 8<<30 || m.FreeBytes != 22<<30 {
		t.Fatalf("totals %+v", got.Memory)
	}
	for _, leak := range []string{"secret", "s-theirs", "s-other1", "operator", "created_by"} {
		if strings.Contains(out, leak) {
			t.Fatalf("list shows %q: %s", leak, out)
		}
	}
}

// A caller that never closes stdin is refused, not waited on.
func TestReadBriefTimesOut(t *testing.T) {
	old := remoteBriefTimeout
	remoteBriefTimeout = 50 * time.Millisecond
	t.Cleanup(func() { remoteBriefTimeout = old })
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	if _, err := readBrief(r); err == nil || !strings.Contains(err.Error(), "no brief within") {
		t.Fatal(err)
	}
	if got, err := readBrief(strings.NewReader("  the task\n")); err != nil || got != "the task" {
		t.Fatal(got, err)
	}
}

// lever agent list shows a remote caller as its creator.
func TestCreatorOf(t *testing.T) {
	for in, want := range map[string]string{"ssh:factory": "ssh:factory", "operator": "operator", "lever-dev": "lever-dev", "ssh:\x1b": "", "x:y": ""} {
		if got := creatorOf(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
