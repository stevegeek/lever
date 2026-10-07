package fizzytool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIRunEnvArgvCwd(t *testing.T) {
	bin, logPath := fakeFizzy(t)
	c := newCLI(t, bin)
	t.Setenv("FAKE_VAR", "leaked")
	data, err := c.Run(context.Background(), "card", "show", "17")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"number":17`) {
		t.Fatalf("data %s", data)
	}
	log := readLog(t, logPath)
	work, _ := filepath.EvalSymlinks(c.Work) // macOS: /var → /private/var
	for _, want := range []string{"ARGV [card] [show] [17] [--agent] [--json]", "CWD " + work, "HOME " + c.Home, "TOKEN tok_SECRET", "ACCOUNT 6182510", "API https://app.fizzy.do", "FAKEVAR \n"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "--token") {
		t.Errorf("token passed as a flag:\n%s", log)
	}
}

func TestCLIOutputCapReturnsFast(t *testing.T) {
	bin, _ := fakeFizzy(t)
	c := newCLI(t, bin)
	// A long timeout and a bound far under it: a child left blocked on a
	// full pipe holds Run for the whole timeout, while a drained one ends
	// in well under a second (seconds under -race on a loaded runner).
	c.Timeout = 60 * time.Second
	start := time.Now()
	_, err := c.Run(context.Background(), "big", "out")
	if err == nil || !strings.Contains(err.Error(), "output larger") {
		t.Fatalf("want an output-cap error, got %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("cap must not wait for the timeout (took %s)", time.Since(start))
	}
}

func TestCheckNoLocalConfig(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "a", "b")
	os.MkdirAll(work, 0o700)
	if err := CheckNoLocalConfig(work); err != nil {
		t.Fatalf("clean tree: %v", err)
	}
	os.WriteFile(filepath.Join(root, "a", ".fizzy.yaml"), []byte("api_url: https://evil.example\n"), 0o600)
	if err := CheckNoLocalConfig(work); err == nil || !strings.Contains(err.Error(), ".fizzy.yaml") {
		t.Fatalf("ancestor config must be refused, got %v", err)
	}
}

func TestCheckVersion(t *testing.T) {
	bin, _ := fakeFizzy(t)
	if err := CheckVersion(context.Background(), newCLI(t, bin)); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRunErrorScrubbed(t *testing.T) {
	bin, _ := fakeFizzy(t)
	c := newCLI(t, bin)
	_, err := c.Run(context.Background(), "fail", "now")
	if err == nil || strings.Contains(err.Error(), "tok_SECRET") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckNoLocalConfigRelative(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o700)
	os.WriteFile(filepath.Join(root, ".fizzy.yaml"), []byte("x: y\n"), 0o600)
	t.Chdir(filepath.Join(root, "a", "b"))
	for _, w := range []string{".", "../b"} {
		if err := CheckNoLocalConfig(w); err == nil || !strings.Contains(err.Error(), ".fizzy.yaml") {
			t.Fatalf("relative %q: ancestor config must be refused, got %v", w, err)
		}
	}
}

func TestCheckNoLocalConfigSymlinkRealAncestor(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real", "w")
	os.MkdirAll(realDir, 0o700)
	os.WriteFile(filepath.Join(root, "real", ".fizzy.yaml"), []byte("x: y\n"), 0o600)
	clean := t.TempDir()
	link := filepath.Join(clean, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckNoLocalConfig(link); err == nil || !strings.Contains(err.Error(), ".fizzy.yaml") {
		t.Fatalf("real ancestor config must be refused via symlink, got %v", err)
	}
}

func TestCheckNoLocalConfigMissingDir(t *testing.T) {
	if err := CheckNoLocalConfig(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("unresolvable work dir must fail closed")
	}
}
