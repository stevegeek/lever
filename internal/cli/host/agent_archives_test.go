package host

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
)

// archivesWith is the broker's archive list: two of the caller's (one of a
// template it may not use), one of another key's and one of the
// operator's.
var archivesWith = canned{body: `{"archives":[` +
	`{"name":"s-gone01","template":"session","label":"PR 42","created_by":"ssh:factory","created_at":"2026-11-22T12:00:00Z","destroyed_at":"2026-11-23T12:00:00Z","format":"combined","workspace_bytes":2048,"transcript_bytes":1024,"restorable":true,"template_configured":true},` +
	`{"name":"x-gone02","template":"secret","created_by":"ssh:factory","created_at":"2026-11-22T12:00:00Z","format":"split","workspace_bytes":1,"transcript_bytes":0,"restorable":true,"template_configured":true},` +
	`{"name":"s-other9","template":"session","created_by":"ssh:other","created_at":"2026-11-22T12:00:00Z","format":"combined","workspace_bytes":1,"transcript_bytes":1,"restorable":true,"template_configured":true},` +
	`{"name":"s-oper99","template":"session","created_by":"operator","created_at":"2026-11-22T12:00:00Z","format":"split","workspace_bytes":1,"transcript_bytes":1,"restorable":true,"template_configured":true,"problem":"/home/op/x is not a regular file"}]}`}

// archives shows only the caller's own archives of its templates, without
// creators or host paths.
func TestRemoteVerbArchives(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentArchives: archivesWith})
	t.Setenv(remoteCommandEnv, "archives --json")
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "agent", "remote-verb", "--templates", "session", "--as", "factory")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got remoteArchives
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !got.OK || len(got.Archives) != 1 || got.Archives[0].Name != "s-gone01" || got.Archives[0].WorkspaceBytes != 2048 ||
		got.Archives[0].TranscriptBytes != 1024 || !got.Archives[0].Restorable || got.Archives[0].DestroyedAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	for _, leak := range []string{"secret", "x-gone02", "s-other9", "s-oper99", "operator", "created_by", "/home/op"} {
		if strings.Contains(out, leak) {
			t.Fatalf("archives shows %q: %s", leak, out)
		}
	}
}

// restore and archive-delete reach only the caller's own archive, and name
// the caller so the broker checks it too.
func TestRemoteVerbRestoreAndArchiveDelete(t *testing.T) {
	for _, tc := range []struct{ command, path, want string }{
		{"restore s-gone01", broker.PathOperatorAgentRestore, `{"archive":"s-gone01","by":"ssh:factory"}`},
		{"restore s-gone01 --memory 8GiB again: PR 42", broker.PathOperatorAgentRestore, `{"archive":"s-gone01","label":"again: PR 42","memory":"8GiB","by":"ssh:factory"}`},
		{"archive-delete s-gone01", broker.PathOperatorAgentArchiveDelete, `{"name":"s-gone01","by":"ssh:factory"}`},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startResumeUDS(t, dir, map[string]canned{
			broker.PathOperatorAgentArchives:      archivesWith,
			broker.PathOperatorAgentRestore:       {body: `{"name":"s-new234","template":"session","phase":"running","memory":"8GiB","restored":{"from":"s-gone01","files":3,"transcripts":".lever/conversation/s-gone01"}}`},
			broker.PathOperatorAgentArchiveDelete: {body: `{"name":"s-gone01","freed_bytes":3072}`},
		})
		res, out, err := remoteExec(t, tc.command, "")
		if err != nil || !res.OK {
			t.Fatalf("%s: %v: %s", tc.command, err, out)
		}
		reqs := rec.all()
		if len(reqs) != 2 || reqs[0].path != broker.PathOperatorAgentArchives || reqs[1].path != tc.path || strings.TrimSpace(reqs[1].body) != tc.want {
			t.Fatalf("%s: %+v", tc.command, reqs)
		}
		if tc.path == broker.PathOperatorAgentRestore && (res.Name != "s-new234" || res.RestoredFrom != "s-gone01" || res.Memory != "8GiB" || res.Template != "session") {
			t.Fatalf("%s: %s", tc.command, out)
		}
		if tc.path == broker.PathOperatorAgentArchiveDelete && (res.Name != "s-gone01" || res.Freed != 3072) {
			t.Fatalf("%s: %s", tc.command, out)
		}
		if strings.Contains(out, ".lever/conversation") {
			t.Fatalf("%s: %s", tc.command, out)
		}
	}
	// Not the caller's, or of a template it may not use: refused alike,
	// with nothing sent but the list.
	for _, command := range []string{"restore s-other9", "restore s-oper99", "restore x-gone02", "archive-delete s-other9", "archive-delete x-gone02", "restore s-mine01"} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startResumeUDS(t, dir, map[string]canned{broker.PathOperatorAgentArchives: archivesWith})
		res, out, err := remoteExec(t, command, "")
		if exitCodeOf(err) != exitWorkerRefused || res.OK || res.Reason != broker.ReasonNotSpawned {
			t.Fatalf("%s: %v: %s", command, err, out)
		}
		for _, r := range rec.all() {
			if r.path != broker.PathOperatorAgentArchives {
				t.Fatalf("%s: sent %s", command, r.path)
			}
		}
	}
	// A broker refusal reaches the caller as a fixed text.
	for _, reason := range []string{broker.ReasonArchiveRefused, broker.ReasonNoArchive, broker.ReasonMemoryBusy} {
		dir := msgDir(t)
		t.Chdir(dir)
		startResumeUDS(t, dir, map[string]canned{
			broker.PathOperatorAgentArchives: archivesWith,
			broker.PathOperatorAgentRestore: {status: http.StatusUnprocessableEntity, body: "the workspace archive of s-gone01 was refused: \"/home/op/../x\" is absolute",
				header: map[string]string{broker.ReasonHeader: reason}},
		})
		res, out, err := remoteExec(t, "restore s-gone01", "")
		if exitCodeOf(err) == 0 || res.OK || res.Reason != reason || res.Error != remoteRefusal(reason) || strings.Contains(out, "/home/op") {
			t.Fatalf("%s: %v: %s", reason, err, out)
		}
	}
}

// The operator's restore sends the archive and the flags given; archives
// prints a table; archive-delete needs --force.
func TestAgentRestoreArchivesAndDelete(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startResumeUDS(t, dir, map[string]canned{
		broker.PathOperatorAgentArchives:      archivesWith,
		broker.PathOperatorAgentRestore:       {body: `{"name":"s-new234","template":"review","phase":"running","memory":"16GiB","restored":{"from":"s-gone01","files":3,"skipped":1,"transcripts":".lever/conversation/s-gone01"}}`},
		broker.PathOperatorAgentArchiveDelete: {body: `{"name":"s-gone01","freed_bytes":3072}`},
	})
	root := func() *stubBackend { return &stubBackend{runner: proc.NewFakeRunner()} }
	out, err := clitest.Exec(t, stubRoot(root()), "agent", "restore", "s-gone01", "--template", "review", "--memory", "16GiB")
	if err != nil || !strings.Contains(out, "agent s-new234 restored from s-gone01 into template review with 16GiB of memory") || !strings.Contains(out, "transcripts in .lever/conversation/s-gone01") {
		t.Fatalf("%v: %s", err, out)
	}
	out, err = clitest.Exec(t, stubRoot(root()), "agent", "archives")
	if err != nil || !strings.Contains(out, "s-gone01") || !strings.Contains(out, "2KiB") || !strings.Contains(out, "ssh:factory") || !strings.Contains(out, "no: /home/op") {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := clitest.Exec(t, stubRoot(root()), "agent", "archive-delete", "s-gone01"); exitCodeOf(err) != exitWorkerRefused {
		t.Fatalf("no --force: %v", err)
	}
	out, err = clitest.Exec(t, stubRoot(root()), "agent", "archive-delete", "s-gone01", "--force", "--json")
	var res agentResult
	if err != nil || json.Unmarshal([]byte(strings.TrimSpace(out)), &res) != nil || !res.OK || res.Freed != 3072 {
		t.Fatalf("%v: %s", err, out)
	}
	var sent []string
	for _, r := range rec.all() {
		sent = append(sent, r.path+" "+strings.TrimSpace(r.body))
	}
	want := []string{
		broker.PathOperatorAgentRestore + ` {"archive":"s-gone01","template":"review","memory":"16GiB"}`,
		broker.PathOperatorAgentArchives + " ",
		broker.PathOperatorAgentArchiveDelete + ` {"name":"s-gone01"}`,
	}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent:\n%s", strings.Join(sent, "\n"))
	}
}
