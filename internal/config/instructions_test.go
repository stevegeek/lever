package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// instructions_file is host-only boot material like prompt_file: it must stay
// inside the instance root so a compromised agent in the mount cannot point
// the manager's next standing instructions at a file it controls.
func TestValidateRejectsUnconfinedInstructionsFile(t *testing.T) {
	cases := map[string]string{
		"manager traversal": "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  instructions_file: ../manual.md\n",
		"manager absolute":  "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  instructions_file: /etc/manual.md\n",
		"worker traversal":  "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\nworkers:\n  - name: w\n    dir: ws/w\n    instructions_file: ../manual.md\n",
		"worker absolute":   "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\nworkers:\n  - name: w\n    dir: ws/w\n    instructions_file: /etc/manual.md\n",
	}
	for label, body := range cases {
		p := writeConfig(t, body)
		_, err := LoadNoHostChecks(p)
		if err == nil {
			t.Fatalf("%s should be rejected", label)
		}
		if !strings.Contains(err.Error(), "instructions_file") {
			t.Fatalf("%s: error %q must name instructions_file", label, err)
		}
	}
}

func TestInstructionsPathsAreRootRelativeAndNotInherited(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CanonicalName)
	body := "name: demo\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\n" +
		"manager:\n  instructions_file: manual.md\n" +
		"workers:\n  - name: w\n    dir: workers/w\n    instructions_file: worker-manual.md\n  - name: bare\n    dir: workers/bare\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := LoadNoHostChecks(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want, _ := filepath.Abs(filepath.Join(dir, "manual.md")) // root, NOT under workspace/
	if got := app.ManagerInstructionsPath(); got != want {
		t.Fatalf("manager instructions path = %q, want %q (root-relative, outside the mount)", got, want)
	}
	wantW, _ := filepath.Abs(filepath.Join(dir, "worker-manual.md"))
	if got := app.WorkerInstructionsPath(app.Workers[0]); got != wantW {
		t.Fatalf("worker instructions path = %q, want %q", got, wantW)
	}
	// Deliberately NOT inherited from the manager (unlike image/model): the
	// manager's manual describes orchestration authority a worker must not read.
	if got := app.WorkerInstructionsPath(app.Workers[1]); got != "" {
		t.Fatalf("worker without instructions_file must resolve to \"\", got %q", got)
	}
}

func TestNoInstructionsFileMeansNoPath(t *testing.T) {
	p := writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager: {}\n")
	app, err := LoadNoHostChecks(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := app.ManagerInstructionsPath(); got != "" {
		t.Fatalf("unset instructions_file must resolve to \"\", got %q", got)
	}
}

// Root confinement is not enough: the tree is inside the root and is the one
// place agents can write. A boot file inside it would let the agent author its
// own next task or standing instructions, which is exactly what these keys
// exist to prevent.
func TestLoadRejectsBootFilesInsideTheTree(t *testing.T) {
	cases := map[string]string{
		"manager prompt in tree":       "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  prompt_file: ws/boot.md\n",
		"manager instructions in tree": "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  instructions_file: ws/manual.md\n",
		"instructions AT the tree":     "name: demo\nbackend: orbstack\ntree: ws\nmanager:\n  instructions_file: ws\n",
		"worker instructions in tree":  "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\nworkers:\n  - name: w\n    dir: w\n    instructions_file: ws/w/manual.md\n",
	}
	for label, body := range cases {
		p := writeConfig(t, body)
		_, err := LoadNoHostChecks(p)
		if err == nil || !strings.Contains(err.Error(), "inside the mounted tree") {
			t.Fatalf("%s: want rejection naming the tree, got %v", label, err)
		}
	}
	// A sibling that merely shares the tree's name as a prefix is OUTSIDE it.
	p := writeConfig(t, "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\nmanager:\n  prompt_file: ws-notes/boot.md\n  instructions_file: ws-notes/manual.md\n")
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("a prefix-sibling of the tree must be accepted: %v", err)
	}
}
