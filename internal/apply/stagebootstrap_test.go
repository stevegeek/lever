package apply

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The manager owns <tree>/.lever: whatever it leaves at bootstrap.json is
// replaced, never written through or opened.
func TestStageBootstrapMaterialReplacesAgentPlantedEntries(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, tree, leaf string) func(t *testing.T){
		"hard link to a worker file": func(t *testing.T, tree, leaf string) func(t *testing.T) {
			other := filepath.Join(tree, "workers", "w1", "notes.txt")
			if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(other, []byte("worker data"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(other, leaf); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T) {
				if b, _ := os.ReadFile(other); string(b) != "worker data" {
					t.Fatalf("the worker's file now holds %q", b)
				}
			}
		},
		"FIFO": func(t *testing.T, tree, leaf string) func(t *testing.T) {
			if err := syscall.Mkfifo(leaf, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			return func(*testing.T) {}
		},
	} {
		t.Run(name, func(t *testing.T) {
			tree := t.TempDir()
			leaf := filepath.Join(tree, ".lever", "bootstrap.json")
			if err := os.Mkdir(filepath.Dir(leaf), 0o700); err != nil {
				t.Fatal(err)
			}
			check := plant(t, tree, leaf)
			done := make(chan error, 1)
			go func() { done <- StageBootstrapMaterial(tree, BootstrapMaterial{Ticket: "manager-ticket"}) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("staging blocked")
			}
			check(t)
			fi, err := os.Lstat(leaf)
			if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
				t.Fatalf("bootstrap.json = %v, %v", fi, err)
			}
			if b, _ := os.ReadFile(leaf); !strings.Contains(string(b), "manager-ticket") {
				t.Fatalf("bootstrap.json holds %q", b)
			}
		})
	}
}
