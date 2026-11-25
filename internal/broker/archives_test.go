package broker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/convbackup"
)

// tarEnt is one entry of a test archive.
type tarEnt struct {
	name string
	typ  byte
	body string
	link string
	size int64 // header size when body is empty (0: len(body))
	mode int64
}

func makeTarGz(t *testing.T, ents ...tarEnt) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: cmpInt(e.mode, 0o644), ModTime: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
		if e.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func cmpInt(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// withConversations gives the env a real conversation store: every backup
// destroy takes holds one transcript, and gc's removal deletes the store.
func withConversations(t *testing.T, e *spawnEnv) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "conversations")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.b.conversationDir = dir
	e.b.backupConversation = func(_ context.Context, agent string) (string, error) {
		raw := makeTarGz(t,
			tarEnt{name: "projects/", typ: tar.TypeDir},
			tarEnt{name: "projects/-workspace/", typ: tar.TypeDir},
			tarEnt{name: "projects/-workspace/abc.jsonl", body: `{"msg":"hello from ` + agent + `"}` + "\n"})
		m, err := convbackup.Store{Dir: filepath.Join(dir, agent)}.Save(convbackup.Meta{Agent: agent, Reason: "destroy"}, func(w io.Writer) (convbackup.Stats, error) {
			return convbackup.Copy(w, bytes.NewReader(raw), "projects", convbackup.DefaultLimits)
		})
		return m.ID, err
	}
	e.b.removeConversations = func(agent string, dryRun bool) (int64, error) {
		if dryRun {
			return 0, nil
		}
		return 0, os.RemoveAll(filepath.Join(dir, agent))
	}
	old := freeBytes
	freeBytes = func(string) (uint64, error) { return 1 << 50, nil }
	t.Cleanup(func() { freeBytes = old })
	return dir
}

func (e *spawnEnv) destroy(t *testing.T, name string) DestroyResponse {
	t.Helper()
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentDestroy, `{"name":"`+name+`","stop":true}`)
	var got DestroyResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("destroy: %d %s", rec.Code, rec.Body)
	}
	return got
}

func (e *spawnEnv) archiveList(t *testing.T) []ArchiveEntry {
	t.Helper()
	rec := callOperator(t, e.b, http.MethodGet, PathOperatorAgentArchives, "")
	var got ArchiveListResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("archives: %d %s", rec.Code, rec.Body)
	}
	return got.Archives
}

func (e *spawnEnv) restore(t *testing.T, body string) SpawnResponse {
	t.Helper()
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, body)
	var got SpawnResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	return got
}

func private(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Fatalf("%s is %v, want %v", p, fi.Mode().Perm(), want)
	}
}

// Destroy keeps one archive: the workspace and the conversation in one
// private directory, and no separate backups. Restore spawns a new agent
// with the old files, the transcript in its workspace, a task that says
// so, a fresh skill and its template's grants.
func TestDestroyArchiveRestore(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	convs := withConversations(t, e)
	old := e.spawn(t, `{"template":"session","label":"PR 42"}`).Name
	ws := filepath.Join(e.tree, "sessions", old)
	if err := os.MkdirAll(filepath.Join(ws, "src/deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{"work.txt": "product", "src/deep/main.go": "package main", ".claude/skills/lever-agent/SKILL.md": "old skill"} {
		if err := os.WriteFile(filepath.Join(ws, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(ws, "work.txt"), 0o4755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(ws, "passwd")); err != nil {
		t.Fatal(err)
	}
	got := e.destroy(t, old)
	if got.Transcript == "" || len(got.Warnings) != 0 {
		t.Fatalf("%+v", got)
	}
	adir := filepath.Join(e.archives, old)
	private(t, adir, 0o700)
	private(t, filepath.Join(adir, archiveWorkspaceFile), 0o600)
	private(t, filepath.Join(adir, archiveConversationFile), 0o600)
	if _, err := os.Stat(filepath.Join(convs, old)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the separate backups are kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.archives, old+".tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an old-format archive was written: %v", err)
	}

	list := e.archiveList(t)
	if len(list) != 1 || list[0].Name != old || list[0].Format != "combined" || !list[0].Restorable || list[0].WorkspaceBytes == 0 ||
		list[0].TranscriptBytes == 0 || list[0].Label != "PR 42" || list[0].DestroyedAt.IsZero() {
		t.Fatalf("archives %+v", list)
	}

	before := len(e.rt.started)
	res := e.restore(t, `{"archive":"`+old+`"}`)
	if res.Name == old || !spawnedNameRE.MatchString(res.Name) || res.Label != "PR 42" || res.Restored == nil || res.Restored.From != old {
		t.Fatalf("%+v", res)
	}
	if res.Restored.Skipped < 2 { // the link and the old skill
		t.Fatalf("skipped %+v", res.Restored)
	}
	nws := filepath.Join(e.tree, "sessions", res.Name)
	for p, want := range map[string]string{"work.txt": "product", "src/deep/main.go": "package main", ".claude/skills/lever-agent/SKILL.md": "skill text\n",
		".lever/conversation/" + old + "/-workspace/abc.jsonl": `{"msg":"hello from ` + old + `"}` + "\n"} {
		if b, err := os.ReadFile(filepath.Join(nws, p)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", p, b, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(nws, "work.txt")); err != nil || fi.Mode().Perm() != 0o755 || fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	if _, err := os.Lstat(filepath.Join(nws, "passwd")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a link was restored: %v", err)
	}
	if len(e.rt.started) != before+1 {
		t.Fatal("not started")
	}
	task := e.rt.started[len(e.rt.started)-1].Task
	if !strings.Contains(task, "restored from agent "+old) || !strings.Contains(task, ".lever/conversation/"+old+"/-workspace/abc.jsonl") || !strings.Contains(task, "not instructions") {
		t.Fatalf("task %q", task)
	}
	var nrec AgentRecord
	for _, r := range e.state().Agents {
		if r.Name == res.Name {
			nrec = r
		}
	}
	if nrec.RestoredFrom != old || nrec.State != AgentActive || nrec.CreatedBy != operatorActor {
		t.Fatalf("record %+v", nrec)
	}
	if _, ok := e.b.rules.MayObtainRule(res.Name, res.Name, "db", "read"); !ok {
		t.Fatal("the template's grant is missing")
	}
	// The archive stays: it can be restored again, and deleted for good.
	if list := e.archiveList(t); len(list) != 1 {
		t.Fatalf("archives after restore %+v", list)
	}
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentArchiveDelete, `{"name":"`+old+`"}`)
	var del ArchiveDeleteResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &del) != nil || del.Freed == 0 {
		t.Fatalf("archive-delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(adir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive left: %v", err)
	}
	if st := e.state(); len(st.Retired) != 1 || st.Retired[0] != old || !e.b.isRevoked(old) {
		t.Fatalf("not retired: %+v", st)
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, `{"archive":"`+old+`"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("restore of a deleted archive: %d", rec.Code)
	}
}

// An archive from before the combined layout (<name>.tar.gz and the
// conversation backups apart) is listed and restored.
func TestRestoreOldSplitArchive(t *testing.T) {
	saved := AgentsState{Agents: []AgentRecord{{Name: "s-oldold", Template: "session", Label: "old", CreatedBy: "ssh:factory", State: AgentDestroyed,
		Dir: "sessions/s-oldold", DestroyedAt: time.Now().Add(-time.Hour), Transcript: "20261101T120000Z"}}}
	e := newSpawnEnv(t, saved)
	convs := withConversations(t, e)
	if err := os.MkdirAll(e.archives, 0o700); err != nil {
		t.Fatal(err)
	}
	ws := makeTarGz(t, tarEnt{name: "notes/", typ: tar.TypeDir}, tarEnt{name: "notes/todo.md", body: "finish it"})
	if err := os.WriteFile(filepath.Join(e.archives, "s-oldold.tar.gz"), ws, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.backupConversation(context.Background(), "s-oldold"); err != nil {
		t.Fatal(err)
	}
	list := e.archiveList(t)
	if len(list) != 1 || list[0].Format != "split" || !list[0].Restorable || list[0].WorkspaceBytes != int64(len(ws)) || list[0].TranscriptBytes == 0 {
		t.Fatalf("%+v", list)
	}
	// A remote caller restores only its own.
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, `{"archive":"s-oldold","by":"ssh:other"}`); rec.Code != http.StatusNotFound || rec.Header().Get(ReasonHeader) != ReasonNotSpawned {
		t.Fatalf("another caller: %d %s", rec.Code, rec.Body)
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, `{"archive":"s-oldold","by":"ssh:factory","template":"session"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a remote template: %d %s", rec.Code, rec.Body)
	}
	res := e.restore(t, `{"archive":"s-oldold","by":"ssh:factory","label":"again"}`)
	nws := filepath.Join(e.tree, "sessions", res.Name)
	if b, err := os.ReadFile(filepath.Join(nws, "notes/todo.md")); err != nil || string(b) != "finish it" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(nws, ".lever/conversation/s-oldold/-workspace/abc.jsonl")); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.state().Agents {
		if r.Name == res.Name && (r.CreatedBy != "ssh:factory" || r.Label != "again") {
			t.Fatalf("%+v", r)
		}
	}
	// archive-delete takes the old file and the backups.
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentArchiveDelete, `{"name":"s-oldold","by":"ssh:other"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("another caller's delete: %d", rec.Code)
	}
	if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentArchiveDelete, `{"name":"s-oldold","by":"ssh:factory"}`); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{filepath.Join(e.archives, "s-oldold.tar.gz"), filepath.Join(convs, "s-oldold")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left: %v", p, err)
		}
	}
}

// An unsafe archive is refused before a name is drawn: nothing is spawned,
// nothing written in the tree.
func TestRestoreRefusesAnUnsafeArchive(t *testing.T) {
	for name, ents := range map[string][]tarEnt{
		"dot-dot":  {{name: "../escape", body: "x"}},
		"inner ..": {{name: "a/../../escape", body: "x"}},
		"absolute": {{name: "/etc/cron.d/x", body: "x"}},
		"twice":    {{name: "a", body: "x"}, {name: "a", body: "y"}},
		"beneath":  {{name: "a", body: "x"}, {name: "a/b", body: "y"}},
	} {
		saved := AgentsState{Agents: []AgentRecord{{Name: "s-badbad", Template: "session", CreatedBy: "operator", State: AgentDestroyed, Dir: "sessions/s-badbad"}}}
		e := newSpawnEnv(t, saved)
		withConversations(t, e)
		if err := os.MkdirAll(filepath.Join(e.archives, "s-badbad"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.archives, "s-badbad", archiveWorkspaceFile), makeTarGz(t, ents...), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, `{"archive":"s-badbad"}`)
		if rec.Code != http.StatusUnprocessableEntity || rec.Header().Get(ReasonHeader) != ReasonArchiveRefused {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if st := e.state(); len(st.Agents) != 1 || len(e.rt.started) != 0 {
			t.Errorf("%s: drew a name: %+v", name, st)
		}
		if ents, _ := os.ReadDir(e.tree); len(ents) != 0 {
			t.Errorf("%s: the tree holds %v", name, ents)
		}
	}
}

// The seeding rules: links and special files are skipped, never written;
// unsafe names, too many entries and too many bytes refuse the archive.
func TestSeedTarRules(t *testing.T) {
	seed := func(lim seedLimits, ents ...tarEnt) (string, seedStats, error) {
		dir := t.TempDir()
		r, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		st, err := seedTar(context.Background(), bytes.NewReader(makeTarGz(t, ents...)), r, seedOpts{Limits: lim})
		return dir, st, err
	}
	small := seedLimits{MaxBytes: 10, MaxEntries: 3, MaxDepth: 4}
	dir, st, err := seed(defaultSeedLimits,
		tarEnt{name: "./", typ: tar.TypeDir},
		tarEnt{name: "f", body: "data"},
		tarEnt{name: "sym", typ: tar.TypeSymlink, link: "/etc/passwd"},
		tarEnt{name: "hard", typ: tar.TypeLink, link: "f"},
		tarEnt{name: "dev", typ: tar.TypeChar},
		tarEnt{name: "fifo", typ: tar.TypeFifo},
		tarEnt{name: "./d/", typ: tar.TypeDir},
		tarEnt{name: "d/g", body: "more"})
	if err != nil || st.Files != 2 || st.Skipped != 4 || st.Dirs != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	for _, p := range []string{"sym", "hard", "dev", "fifo"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was written: %v", p, err)
		}
	}
	for name, tc := range map[string]struct {
		lim  seedLimits
		ents []tarEnt
	}{
		"dot-dot":          {defaultSeedLimits, []tarEnt{{name: "../x", body: "x"}}},
		"dot-dot dir":      {defaultSeedLimits, []tarEnt{{name: "../", typ: tar.TypeDir}}},
		"absolute":         {defaultSeedLimits, []tarEnt{{name: "/tmp/x", body: "x"}}},
		"unclean":          {defaultSeedLimits, []tarEnt{{name: "a//b", body: "x"}}},
		"dot":              {defaultSeedLimits, []tarEnt{{name: "a/./b", body: "x"}}},
		"too many entries": {small, []tarEnt{{name: "a", body: "1"}, {name: "b", body: "2"}, {name: "c", body: "3"}, {name: "d", body: "4"}}},
		"too big":          {small, []tarEnt{{name: "a", body: "0123456789x"}}},
		"too big in all":   {small, []tarEnt{{name: "a", body: "012345"}, {name: "b", body: "012345"}}},
		"too deep":         {small, []tarEnt{{name: "a/b/c/d/e", body: "x"}}},
		"links count":      {small, []tarEnt{{name: "a", typ: tar.TypeSymlink, link: "x"}, {name: "b", typ: tar.TypeSymlink, link: "x"}, {name: "c", typ: tar.TypeSymlink, link: "x"}, {name: "d", typ: tar.TypeSymlink, link: "x"}}},
	} {
		dir, _, err := seed(tc.lim, tc.ents...)
		if !errors.Is(err, errSeedRefused) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "x")); err == nil {
			t.Errorf("%s: wrote outside", name)
		}
	}
	// Not gzip, or cut short.
	if _, err := seedCheck(context.Background(), strings.NewReader("plain"), seedOpts{}); !errors.Is(err, errSeedRefused) {
		t.Fatal(err)
	}
	full := makeTarGz(t, tarEnt{name: "a", body: strings.Repeat("x", 4096)})
	if _, err := seedCheck(context.Background(), bytes.NewReader(full[:len(full)-8]), seedOpts{}); err == nil {
		t.Fatal("a truncated archive passed")
	}
}

// A seed never writes through a link planted in the new workspace: the
// root handle keeps it inside, and an existing file is not overwritten.
func TestSeedTarStaysInsideItsRoot(t *testing.T) {
	outside := t.TempDir()
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := seedTar(context.Background(), bytes.NewReader(makeTarGz(t, tarEnt{name: "out/x", body: "x"})), r, seedOpts{}); err == nil {
		t.Fatal("wrote through a link")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("outside holds %v", ents)
	}
	if _, err := seedTar(context.Background(), bytes.NewReader(makeTarGz(t, tarEnt{name: "kept", body: "theirs"})), r, seedOpts{}); err == nil {
		t.Fatal("overwrote a file")
	}
}

// A restore whose seed fails after the name is drawn is undone like any
// failed spawn: the workspace is removed and the name destroyed.
func TestRestoreSeedFailureIsUndone(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{})
	w := httptest.NewRecorder()
	e.b.spawnAgentWith(context.Background(), w, SpawnRequest{Template: "session"}, operatorActor, spawnExtras{
		restoredFrom: "s-seedss",
		seed: func(_ context.Context, dir *os.Root) (*RestoreInfo, error) {
			if err := dir.WriteFile("half.txt", []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return nil, errors.New("disk on fire")
		},
	})
	if w.Code != http.StatusUnprocessableEntity || w.Header().Get(ReasonHeader) != ReasonArchiveRefused {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	st := e.state()
	if len(st.Agents) != 1 || st.Agents[0].State != AgentDestroyed || st.Agents[0].RestoredFrom != "s-seedss" || !e.b.isRevoked(st.Agents[0].Name) || len(e.rt.started) != 0 {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(filepath.Join(e.tree, st.Agents[0].Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace left: %v", err)
	}
}

// What lever writes itself wins: an old workspace's file where the skill's
// or the transcripts' folders go is skipped. Into another template, the
// old agent's configuration (hooks, MCP servers, CLAUDE.md, git hooks) is
// left out as well.
func TestRestoreSkipsReservedAndForeignConfig(t *testing.T) {
	for _, tc := range []struct {
		template string
		kept     []string
		skipped  []string
	}{
		{"session", []string{"CLAUDE.md", ".claude/settings.json", "src/a.go"}, []string{".lever/conversation"}},
		{"gone", []string{"src/a.go", "README.md"}, []string{"CLAUDE.md", ".claude/settings.json", ".mcp.json", ".git/hooks/pre-commit", ".git/config", ".lever/notes"}},
	} {
		saved := AgentsState{Agents: []AgentRecord{{Name: "s-seedss", Template: tc.template, CreatedBy: "operator", State: AgentDestroyed, Dir: "sessions/s-seedss"}}}
		e := newSpawnEnv(t, saved)
		withConversations(t, e)
		ents := []tarEnt{{name: "src/", typ: tar.TypeDir}, {name: "src/a.go", body: "package a"}, {name: "README.md", body: "r"}}
		if tc.template == "session" {
			// Files where lever's folders go.
			ents = append(ents, tarEnt{name: "CLAUDE.md", body: "c"}, tarEnt{name: ".claude/", typ: tar.TypeDir},
				tarEnt{name: ".claude/settings.json", body: "{}"}, tarEnt{name: ".lever/", typ: tar.TypeDir}, tarEnt{name: ".lever/conversation", body: "planted"})
		} else {
			ents = append(ents, tarEnt{name: "CLAUDE.md", body: "c"}, tarEnt{name: ".claude/", typ: tar.TypeDir}, tarEnt{name: ".claude/settings.json", body: "{}"},
				tarEnt{name: ".mcp.json", body: "{}"}, tarEnt{name: ".git/", typ: tar.TypeDir}, tarEnt{name: ".git/hooks/", typ: tar.TypeDir},
				tarEnt{name: ".git/hooks/pre-commit", body: "#!/bin/sh", mode: 0o755}, tarEnt{name: ".git/config", body: "[core]"},
				tarEnt{name: ".lever/", typ: tar.TypeDir}, tarEnt{name: ".lever/notes", body: "n"})
		}
		if err := os.MkdirAll(filepath.Join(e.archives, "s-seedss"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.archives, "s-seedss", archiveWorkspaceFile), makeTarGz(t, ents...), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.b.backupConversation(context.Background(), "s-seedss"); err != nil {
			t.Fatal(err)
		}
		res := e.restore(t, `{"archive":"s-seedss","template":"session"}`)
		nws := filepath.Join(e.tree, "sessions", res.Name)
		for _, p := range tc.kept {
			if _, err := os.Stat(filepath.Join(nws, p)); err != nil {
				t.Errorf("%s: %s not restored: %v", tc.template, p, err)
			}
		}
		for _, p := range tc.skipped {
			if fi, err := os.Lstat(filepath.Join(nws, p)); err == nil && !fi.IsDir() {
				t.Errorf("%s: %s restored", tc.template, p)
			}
		}
		if b, err := os.ReadFile(filepath.Join(nws, ".claude/skills/lever-agent/SKILL.md")); err != nil || string(b) != "skill text\n" {
			t.Errorf("%s: skill %q %v", tc.template, b, err)
		}
		if _, err := os.Stat(filepath.Join(nws, ".lever/conversation/s-seedss/-workspace/abc.jsonl")); err != nil {
			t.Errorf("%s: transcript: %v", tc.template, err)
		}
	}
}

// A remote caller's restores are rate-limited before the archive is read,
// even when each is refused.
func TestRemoteRestoreIsRateLimited(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.MaxRunning = 1 })
	withConversations(t, e)
	old := e.spawn(t, `{"template":"session","by":"ssh:factory"}`).Name
	e.destroy(t, old)
	e.spawn(t, `{"template":"session"}`) // the template is now busy
	var reasons []string
	for range 5 {
		rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, `{"archive":"`+old+`","by":"ssh:factory"}`)
		reasons = append(reasons, rec.Header().Get(ReasonHeader))
	}
	if strings.Join(reasons, ",") != "template-busy,template-busy,template-busy,template-busy,rate-limited" {
		t.Fatalf("%v", reasons)
	}
}

// Restore goes through spawn's admission: a template at max_running
// refuses it before a name is drawn.
func TestRestoreIsAdmittedLikeASpawn(t *testing.T) {
	e := newSpawnEnv(t, AgentsState{}, func(t *TemplateSpec) { t.MaxRunning = 1 })
	withConversations(t, e)
	old := e.spawn(t, `{"template":"session"}`).Name
	e.destroy(t, old)
	e.spawn(t, `{"template":"session"}`) // the fake lists it running
	n := len(e.state().Agents)
	rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, `{"archive":"`+old+`"}`)
	if rec.Code != http.StatusConflict || rec.Header().Get(ReasonHeader) != ReasonTemplateBusy {
		t.Fatalf("%d %q %s", rec.Code, rec.Header().Get(ReasonHeader), rec.Body)
	}
	if len(e.state().Agents) != n {
		t.Fatal("a name was drawn")
	}
	// Bad bodies, and names that are not destroyed agents.
	for body, code := range map[string]int{
		`{"archive":"` + old + `","name":"x"}`: http.StatusBadRequest,
		`{"archive":"worker"}`:                 http.StatusNotFound,
		`{"archive":"../x"}`:                   http.StatusNotFound,
		`{"archive":"` + old + `","by":"bad"}`: http.StatusBadRequest,
	} {
		if rec := callOperator(t, e.b, http.MethodPost, PathOperatorAgentRestore, body); rec.Code != code {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
}
