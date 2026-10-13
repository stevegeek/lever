package guest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCoreSandbox(t *testing.T) (corePaths, string, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	p := corePaths{
		Handler:  filepath.Join(root, "usr/local/libexec/lever-core-handler"),
		Sysctl:   filepath.Join(root, "etc/sysctl.d/99-lever-core.conf"),
		Dir:      filepath.Join(root, "var/lib/lever/cores"),
		OwnerUID: os.Getuid(),
	}
	bin := filepath.Join(root, "bin")
	log := filepath.Join(root, "calls.log")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("sysctl", `echo "sysctl $*" >> `+shellSingleQuote(log))
	stub("systemctl", systemctlStub(log, filepath.Join(root, "apport.masked")))
	return p, bin, log
}

// systemctlStub logs its argv and plays an installed apport.service that
// mask marks masked (masked is a state file); without APPORT_INSTALLED there
// is no unit.
func systemctlStub(log, masked string) string {
	return `echo "systemctl $*" >> ` + shellSingleQuote(log) + `
case "$1" in
is-enabled) if [ -e ` + shellSingleQuote(masked) + ` ]; then echo masked; else echo enabled; fi ;;
cat) [ -n "${APPORT_INSTALLED:-}" ] ;;
mask) : > ` + shellSingleQuote(masked) + ` ;;
esac
exit 0`
}

func runCoreScript(t *testing.T, p corePaths, bin, log string, env ...string) (GuestReport, string) {
	t.Helper()
	_ = os.Remove(log)
	cmd := exec.Command("bash", "-c", coreCaptureScript(p))
	cmd.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(log)
	return parseGuestReport(string(out)), string(calls)
}

func TestCoreCaptureScriptWritesHandlerAndPattern(t *testing.T) {
	p, bin, log := newCoreSandbox(t)
	rep, calls := runCoreScript(t, p, bin, log, "APPORT_INSTALLED=1")
	if len(rep.Warn) != 0 || len(rep.Reboot) != 0 {
		t.Fatalf("unexpected report %+v", rep)
	}
	st, err := os.Stat(p.Handler)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("handler: %v %v", st, err)
	}
	if st, err := os.Stat(p.Dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("core dir: %v %v", st, err)
	}
	sys, _ := os.ReadFile(p.Sysctl)
	want := "kernel.core_pattern = |" + p.Handler + " %P %t %e"
	if !strings.Contains(string(sys), want) {
		t.Fatalf("sysctl file lacks %q:\n%s", want, sys)
	}
	if !strings.Contains(calls, "sysctl -q -w kernel.core_pattern=|"+p.Handler+" %P %t %e") {
		t.Fatalf("core_pattern not set live: %s", calls)
	}
	// One handler at a time, set before the pattern pipes to it.
	if !strings.Contains(string(sys), "kernel.core_pipe_limit = 1\n") {
		t.Fatalf("sysctl file lacks the pipe limit:\n%s", sys)
	}
	lim := strings.Index(calls, "sysctl -q -w kernel.core_pipe_limit=1")
	if lim < 0 || lim > strings.Index(calls, "kernel.core_pattern=") {
		t.Fatalf("core_pipe_limit not set live before core_pattern: %s", calls)
	}
	if !strings.Contains(calls, "systemctl stop apport.service") || !strings.Contains(calls, "systemctl mask apport.service") {
		t.Fatalf("apport not stopped and masked: %s", calls)
	}
	// Idempotent; apport already masked is left alone.
	before, _ := os.ReadFile(p.Handler)
	_, calls = runCoreScript(t, p, bin, log, "APPORT_INSTALLED=1")
	after, _ := os.ReadFile(p.Handler)
	if !bytes.Equal(before, after) || strings.Contains(calls, "mask apport") || strings.Contains(calls, "stop") {
		t.Fatalf("second run changed the handler or masked apport again: %s", calls)
	}
}

// runHandler runs the handler as the kernel would, with core on stdin.
func runHandler(t *testing.T, dir string, maxBytes, keep int, core []byte, args ...string) {
	t.Helper()
	runHandlerGap(t, dir, maxBytes, keep, 0, core, args...)
}

// runHandlerGap is runHandler with the least time between kept cores.
func runHandlerGap(t *testing.T, dir string, maxBytes, keep, gap int, core []byte, args ...string) {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not available")
	}
	h := filepath.Join(t.TempDir(), "handler")
	if err := os.WriteFile(h, []byte(coreHandlerScript(dir, os.Getuid(), maxBytes, keep, gap)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(h, args...)
	cmd.Stdin = bytes.NewReader(core)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("handler failed: %v\n%s", err, out)
	}
}

func coreFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "core.") {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestCoreHandlerKeepsATmuxServerCore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cores")
	runHandler(t, dir, 1<<20, 2, []byte("ELF-core"), "4242", "1760000000", "tmux: server")
	b, err := os.ReadFile(filepath.Join(dir, "core.tmux.1760000000.4242"))
	if err != nil || string(b) != "ELF-core" {
		t.Fatalf("core = %q, %v", b, err)
	}
	st, _ := os.Stat(filepath.Join(dir, "core.tmux.1760000000.4242"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("core mode %v, want 0600", st.Mode().Perm())
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", st.Mode().Perm())
	}
	// Older kernels split the name into words.
	runHandler(t, dir, 1<<20, 2, []byte("x"), "4243", "1760000001", "tmux:", "server")
	if _, err := os.Stat(filepath.Join(dir, "core.tmux.1760000001.4243")); err != nil {
		t.Fatalf("split name not kept: %v", err)
	}
}

// A core directory that is a symlink, or not the expected owner's, gets
// nothing written through it.
func TestCoreHandlerRefusesASymlinkedDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "cores")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	runHandler(t, dir, 1<<20, 2, []byte("core"), "1", "2", "tmux: server")
	if got := coreFiles(t, target); len(got) != 0 {
		t.Fatalf("wrote %v through a symlink", got)
	}
}

func TestCoreHandlerDropsOthersAndBadArgs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cores")
	for _, args := range [][]string{
		{"1", "2", "node"},
		{"1", "2", "tmux: client"},
		{"1", "2", "tmux: server x"},
		{"1/../../x", "2", "tmux: server"},
		{"1", "2;rm", "tmux: server"},
		{"1", "2"},
		{},
	} {
		runHandler(t, dir, 1<<20, 2, []byte("core"), args...)
	}
	if got := coreFiles(t, dir); len(got) != 0 {
		t.Fatalf("kept %v, want nothing", got)
	}
}

func TestCoreHandlerBoundsSizeAndCount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cores")
	big := bytes.Repeat([]byte("a"), 5000)
	for _, ts := range []string{"100", "99", "102", "101"} {
		runHandler(t, dir, 1000, 2, big, "7", ts, "tmux: server")
	}
	got := coreFiles(t, dir)
	if len(got) != 2 {
		t.Fatalf("kept %v, want the newest 2", got)
	}
	for _, n := range got {
		if n != "core.tmux.102.7" && n != "core.tmux.101.7" {
			t.Fatalf("kept %v, want the two newest by dump time", got)
		}
		st, _ := os.Stat(filepath.Join(dir, n))
		if st.Size() != 1000 {
			t.Fatalf("%s is %d bytes, want the 1000-byte cap", n, st.Size())
		}
	}
}

// A dump that finds the lock held is dropped, not queued.
func TestCoreHandlerDropsWhileLocked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cores")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not available")
	}
	hold := exec.Command("flock", filepath.Join(dir, ".lock"), "sleep", "5")
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Process.Kill(); _ = hold.Wait() }()
	lock := filepath.Join(dir, ".lock")
	for i := 0; exec.Command("flock", "-n", lock, "true").Run() == nil; i++ { // until flock holds it
		if i == 100 {
			t.Fatal("the lock was never taken")
		}
		_ = exec.Command("sleep", "0.05").Run()
	}
	runHandler(t, dir, 1<<20, 2, []byte("core"), "9", "200", "tmux: server")
	if got := coreFiles(t, dir); len(got) != 0 {
		t.Fatalf("kept %v while the lock was held", got)
	}
}

// The crash-recovery call converges the core capture too, and a capture
// that cannot be set up is a warning, not a failed bring-up.
func TestCrashRecoveryConvergesCoreCapture(t *testing.T) {
	s := newCrashSandbox(t)
	rep, calls := s.run()
	if _, err := os.Stat(s.paths.Core.Handler); err != nil {
		t.Fatalf("handler not written: %v", err)
	}
	if !strings.Contains(calls, "kernel.core_pattern=|"+s.paths.Core.Handler) {
		t.Fatalf("core_pattern not set: %s", calls)
	}
	for _, w := range rep.Warn {
		if strings.Contains(w, "core") {
			t.Fatalf("unexpected warning %q", w)
		}
	}

	s = newCrashSandbox(t)
	s.write(filepath.Dir(s.paths.Core.Dir), "a file where the directory goes")
	rep, _ = s.run()
	if _, err := os.Stat(s.paths.Sysctl); err != nil {
		t.Fatalf("crash recovery not converged: %v", err)
	}
	found := false
	for _, w := range rep.Warn {
		found = found || strings.Contains(w, "tmux server core capture")
	}
	if !found {
		t.Fatalf("want a core-capture warning, got %+v", rep.Warn)
	}
}

// In the guest the core directory must be root's.
func TestCoreHandlerInTheGuestWantsRoot(t *testing.T) {
	h := coreHandlerScript(realCorePaths.Dir, realCorePaths.OwnerUID, CoreMaxBytes, CoreKeep, CoreMinGap)
	if !strings.Contains(h, `[ "$(stat -c %u "$dir")" = 0 ]`) || !strings.Contains(h, "dir='"+CoreDir+"'") {
		t.Fatalf("guest handler does not require a root-owned %s:\n%s", CoreDir, h)
	}
}

// A crash loop keeps one core per gap: a dump while the newest kept core is
// younger is dropped, and nothing is pruned for it; once the newest is
// older (or dated in the future) the next dump is kept.
func TestCoreHandlerRateLimits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cores")
	runHandlerGap(t, dir, 1<<20, 2, 60, []byte("first"), "1", "100", "tmux: server")
	runHandlerGap(t, dir, 1<<20, 2, 60, []byte("second"), "2", "101", "tmux: server")
	if got := coreFiles(t, dir); len(got) != 1 || got[0] != "core.tmux.100.1" {
		t.Fatalf("kept %v, want the first core alone", got)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "core.tmux.100.1"), old, old); err != nil {
		t.Fatal(err)
	}
	runHandlerGap(t, dir, 1<<20, 2, 60, []byte("third"), "3", "102", "tmux: server")
	if got := coreFiles(t, dir); len(got) != 2 {
		t.Fatalf("kept %v, want the first and the third", got)
	}
	// A core dated in the future does not hold dumps off for good.
	future := time.Now().Add(24 * time.Hour)
	for _, n := range coreFiles(t, dir) {
		if err := os.Chtimes(filepath.Join(dir, n), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, "core.tmux.102.3"), future, future); err != nil {
		t.Fatal(err)
	}
	runHandlerGap(t, dir, 1<<20, 2, 60, []byte("fourth"), "4", "103", "tmux: server")
	if _, err := os.Stat(filepath.Join(dir, "core.tmux.103.4")); err != nil {
		t.Fatalf("a future-dated core held the dump off: %v", err)
	}
}
