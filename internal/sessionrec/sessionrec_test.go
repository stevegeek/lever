package sessionrec

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/skills"
)

func stateDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestLatestIsTheLastStartOfEachAgent: the newest record of each agent wins,
// across the rotated copy too, and the file is 0600.
func TestLatestIsTheLastStartOfEachAgent(t *testing.T) {
	p := filepath.Join(stateDir(t), "sessions.jsonl")
	t0 := time.Now().UTC()
	for _, r := range []Record{
		{Agent: "hello", SkillHash: "old", Version: "0.27.0", Started: t0},
		{Agent: "scratch", SkillHash: "s1", Version: "0.28.0", Started: t0},
		{Agent: "hello", SkillHash: "new", Version: "0.28.0", Started: t0.Add(time.Minute)},
	} {
		if err := Append(p, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Latest(p)
	if err != nil {
		t.Fatal(err)
	}
	if got["hello"].SkillHash != "new" || got["scratch"].SkillHash != "s1" || len(got) != 2 {
		t.Fatalf("latest = %+v", got)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record file: %v %v", fi, err)
	}
	// A rotated copy is read too.
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	if got, err := Latest(p); err != nil || got["hello"].SkillHash != "new" {
		t.Fatalf("after rotation: %+v %v", got, err)
	}
}

// TestMissingRecordIsNoRecords: no directory or no file is no records.
func TestMissingRecordIsNoRecords(t *testing.T) {
	for _, p := range []string{
		filepath.Join(t.TempDir(), "absent", "sessions.jsonl"),
		filepath.Join(stateDir(t), "sessions.jsonl"),
	} {
		if got, err := Latest(p); err != nil || len(got) != 0 {
			t.Fatalf("%s: %v %v", p, got, err)
		}
	}
}

// TestUnsafeRecordIsRefused: a record another user could have written (a
// group-writable file or directory, a symlink) proves nothing, and nothing
// is appended into an unsafe directory.
func TestUnsafeRecordIsRefused(t *testing.T) {
	line := []byte(`{"agent":"hello","skill_hash":"h","started":"2026-09-29T10:00:00Z"}` + "\n")
	for name, plant := range map[string]func(t *testing.T, dir, p string){
		"group-writable file": func(t *testing.T, _, p string) {
			if err := os.WriteFile(p, line, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o620); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, dir, p string) {
			other := filepath.Join(dir, "other")
			if err := os.WriteFile(other, line, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, p); err != nil {
				t.Fatal(err)
			}
		},
		"group-writable directory": func(t *testing.T, dir, p string) {
			if err := os.WriteFile(p, line, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o770); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			p := filepath.Join(dir, "sessions.jsonl")
			plant(t, dir, p)
			if _, err := Latest(p); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("Latest: %v, want ErrUnsafe", err)
			}
		})
	}
	dir := stateDir(t)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := Append(filepath.Join(dir, "sessions.jsonl"), Record{Agent: "hello", SkillHash: "h"}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("Append into an unsafe dir: %v, want ErrUnsafe", err)
	}
}

// TestSkillHashReadsTheAgentsSkill: the manager's skill is lever-operator,
// a worker's is lever-agent in its dir, and anything else is not an agent.
func TestSkillHashReadsTheAgentsSkill(t *testing.T) {
	tree := t.TempDir()
	app := &config.App{Name: "hello", Tree: tree, Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}}}
	for agent, body := range map[string]string{"hello": "operator skill", "scratch": "agent skill"} {
		rel, ok := SkillRel(app, agent)
		if !ok {
			t.Fatalf("%s: no skill path", agent)
		}
		if err := fsutil.WriteInTree(tree, rel, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if h, err := SkillHash(app, agent); err != nil || h != skills.Hash([]byte(body)) {
			t.Fatalf("%s: %q %v", agent, h, err)
		}
	}
	if rel, _ := SkillRel(app, "scratch"); rel != "workers/scratch/.claude/skills/lever-agent/SKILL.md" {
		t.Fatalf("worker skill at %q", rel)
	}
	if _, ok := SkillRel(app, "nobody"); ok {
		t.Fatal("an undeclared agent has a skill path")
	}
	if _, err := SkillHash(app, "nobody"); err == nil {
		t.Fatal("an undeclared agent hashed")
	}
}
