package lima

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/common"
	"github.com/stevegeek/lever/internal/proc"
)

const panicTrace = "[   12.000001] BUG: kernel NULL pointer dereference, address: 0000000000000000\n" +
	"[   12.000100] Kernel panic - not syncing: Fatal exception\n"

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listSets(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

var t0 = time.Date(2026, 10, 15, 3, 4, 5, 0, time.UTC)

func TestSaveGuestLogsCopiesNonEmptyLogsPrivately(t *testing.T) {
	inst, state := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(inst, "serial.log"), "boot\n"+panicTrace)
	writeFile(t, filepath.Join(inst, "serialv.log"), "")
	writeFile(t, filepath.Join(inst, "ha.stderr.log"), "{\"msg\":\"ssh\"}\n")
	dir := filepath.Join(state, "guest-logs")

	path, err := SaveGuestLogs(inst, dir, t0)
	if err != nil {
		t.Fatalf("SaveGuestLogs: %v", err)
	}
	if want := filepath.Join(dir, "20261015T030405Z-panic"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("guest-logs mode %o, want 700", m)
	}
	if m := mode(t, path); m != 0o700 {
		t.Errorf("set mode %o, want 700", m)
	}
	got := listSets(t, path)
	if strings.Join(got, ",") != "ha.stderr.log,serial.log" {
		t.Fatalf("saved %v, want ha.stderr.log and serial.log (empty serialv.log skipped)", got)
	}
	for _, n := range got {
		if m := mode(t, filepath.Join(path, n)); m != 0o600 {
			t.Errorf("%s mode %o, want 600", n, m)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(path, "serial.log")); string(b) != "boot\n"+panicTrace {
		t.Errorf("serial.log copy = %q", b)
	}
}

func TestSaveGuestLogsNothingToSave(t *testing.T) {
	inst, state := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(inst, "serial.log"), "")
	dir := filepath.Join(state, "guest-logs")
	path, err := SaveGuestLogs(inst, dir, t0)
	if err != nil || path != "" {
		t.Fatalf("SaveGuestLogs = %q, %v; want nothing saved", path, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("guest-logs created with nothing to save: %v", err)
	}
}

func TestSaveGuestLogsKeepsTheTail(t *testing.T) {
	inst, state := t.TempDir(), t.TempDir()
	big := bytes.Repeat([]byte("x"), GuestLogCap+100)
	copy(big[len(big)-4:], "END\n")
	if err := os.WriteFile(filepath.Join(inst, "serial.log"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := SaveGuestLogs(inst, filepath.Join(state, "guest-logs"), t0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(path, "serial.log"))
	if len(b) != GuestLogCap || !bytes.HasSuffix(b, []byte("END\n")) {
		t.Fatalf("copy is %d bytes (want %d) ending %q", len(b), GuestLogCap, b[len(b)-4:])
	}
}

func TestSaveGuestLogsNeverFollowsLinks(t *testing.T) {
	inst, state, other := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(other, "secret")
	writeFile(t, secret, "SECRET\n")
	if err := os.Symlink(secret, filepath.Join(inst, "serial.log")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(inst, "serialv.log"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inst, "ha.stderr.log"), "ha\n")
	path, err := SaveGuestLogs(inst, filepath.Join(state, "guest-logs"), t0)
	if err == nil {
		t.Error("a linked serial.log and a FIFO serialv.log should be reported")
	}
	if got := listSets(t, path); strings.Join(got, ",") != "ha.stderr.log" {
		t.Fatalf("saved %v, want only ha.stderr.log", got)
	}

	// A link in place of guest-logs is refused, and nothing lands at its target.
	state2 := t.TempDir()
	if err := os.Symlink(other, filepath.Join(state2, "guest-logs")); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveGuestLogs(inst, filepath.Join(state2, "guest-logs"), t0); err == nil {
		t.Error("a linked guest-logs directory should be refused")
	}
	if got := listSets(t, other); strings.Join(got, ",") != "secret" {
		t.Errorf("wrote through the link: %v", got)
	}
}

func TestSaveGuestLogsPrunesPerKind(t *testing.T) {
	inst, state := t.TempDir(), t.TempDir()
	dir := filepath.Join(state, "guest-logs")
	writeFile(t, filepath.Join(inst, "serial.log"), panicTrace)
	if _, err := SaveLivePanic(inst, dir, t0); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "operator-notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(inst, "serial.log"), "a clean boot\n")
	for i := range 7 {
		if _, err := SaveGuestLogs(inst, dir, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// Two in the same second sort after the first.
	if _, err := SaveGuestLogs(inst, dir, t0.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := "20261015T030405Z-panic,20261015T030705Z,20261015T030805Z,20261015T030905Z,20261015T031005Z,20261015T031005Z-2,operator-notes"
	if got := strings.Join(listSets(t, dir), ","); got != want {
		t.Fatalf("sets\n got %s\nwant %s", got, want)
	}
	if got := LatestGuestLogSet(dir); got != filepath.Join(dir, "20261015T031005Z-2") {
		t.Errorf("LatestGuestLogSet = %q", got)
	}
}

func TestSaveLivePanicSavesOneTraceOnce(t *testing.T) {
	inst, state := t.TempDir(), t.TempDir()
	dir := filepath.Join(state, "guest-logs")
	serial := filepath.Join(inst, "serial.log")
	writeFile(t, serial, "boot\n[    3.1] Out of memory: Killed process 42 (Kernel panic -) total-vm:1kB\n")
	if path, err := SaveLivePanic(inst, dir, t0); err != nil || path != "" {
		t.Fatalf("no panic: SaveLivePanic = %q, %v; want nothing", path, err)
	}
	f, err := os.OpenFile(serial, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(panicTrace + "[    0.000000] Linux version 7.0\n")
	path, err := SaveLivePanic(inst, dir, t0)
	if err != nil || !strings.HasSuffix(path, "-panic") {
		t.Fatalf("SaveLivePanic = %q, %v; want a panic set", path, err)
	}
	f.WriteString("more boot lines\n")
	if again, err := SaveLivePanic(inst, dir, t0.Add(time.Minute)); err != nil || again != "" {
		t.Fatalf("same trace saved again: %q, %v", again, err)
	}
	// One live save per VM run: a forged line cannot evict saved sets.
	f.WriteString("[  99.1] Oops: general protection fault\n")
	f.Close()
	if next, err := SaveLivePanic(inst, dir, t0.Add(2*time.Minute)); err != nil || next != "" {
		t.Fatalf("a second live save in one VM run: %q, %v", next, err)
	}
	// The next VM run's serial log is a new file.
	if err := os.Rename(serial, serial+".old"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, serial, panicTrace)
	if next, err := SaveLivePanic(inst, dir, t0.Add(3*time.Minute)); err != nil || next == "" {
		t.Fatalf("the next VM run's trace was not saved: %q, %v", next, err)
	}
	if got := PanicLineIn(path); got != "serial.log: [   12.000001] BUG: kernel NULL pointer dereference, address: 0000000000000000" {
		t.Errorf("PanicLineIn = %q", got)
	}
}

func TestPanicLineREAnchorsAtTheKernelMessage(t *testing.T) {
	for line, want := range map[string]bool{
		"[   12.000100] Kernel panic - not syncing: Fatal exception": true,
		"Kernel panic - not syncing: VFS: Unable to mount root fs":   true,
		"[  1.2] Oops: 0002 [#1] PREEMPT SMP":                        true,
		"[  1.2][  T123] BUG: unable to handle page fault":           true,
		"[  1.2] general protection fault: 0000 [#1] SMP":            true,
		"[  1.2] traps: PANIC: double fault, error_code: 0x0":        true,
		"\r[  1.2] Kernel panic - not syncing: x":                    true,
		"[  3.1] Out of memory: Killed process 4 (Kernel panic -)":   false,
		"lima-jail login: [ 900.0] BUG: soft lockup":                 true,
		"[  3.1] INFO: task BUG: x:42 blocked for more than 120 s":   false,
		"Ubuntu 26.04 lever ttyS0":                                   false,
		"Oopsie":                                                     false,
	} {
		if got := panicLineRE.MatchString(strings.TrimLeft(line, "\r")); got != want {
			t.Errorf("%q: match %v, want %v", line, got, want)
		}
	}
}

func TestPanicLineInIsSafeToPrint(t *testing.T) {
	set := t.TempDir()
	writeFile(t, filepath.Join(set, "serial.log"), "[ 1.0] Kernel panic - not syncing: Fatal exception\r\n")
	if got := PanicLineIn(set); got != "serial.log: [ 1.0] Kernel panic - not syncing: Fatal exception" {
		t.Errorf("CRLF line = %q", got)
	}
	evil := "[ 1.0] Kernel panic - not syncing: \x1b]52;c;ZWNobyBoaQ==\x07\x1b[2J" + strings.Repeat("A", 5000) + "\n"
	writeFile(t, filepath.Join(set, "serial.log"), evil)
	got := PanicLineIn(set)
	if strings.ContainsAny(got, "\x1b\x07\n") {
		t.Fatalf("control bytes survived: %q", got)
	}
	if n := len([]rune(got)); n > panicLineMax+len("serial.log: ")+1 {
		t.Fatalf("line is %d runes, want at most %d", n, panicLineMax)
	}
}

func TestEnsureUpSavesGuestLogsBeforeStartingAStoppedVM(t *testing.T) {
	inst, state := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(inst, "serial.log"), panicTrace)
	f := proc.NewFakeRunner()
	limaVersionScript(f)
	scriptList(f, listStopped)
	scriptLifecycle(f)
	scriptRealizedConfig(f, vm, strings.Replace(matchingRealizedConfigJSON(vm, tree), "{", `{"dir":"`+inst+`",`, 1))
	limaGuest.ScriptProvision(f, "501", backendtest.AhostsV4)
	l := New(f, vm, common.Options{})
	dir := filepath.Join(state, "guest-logs")
	if err := l.EnsureUp(context.Background(), backend.Config{MachineName: vm, ProjectTree: tree, GuestLogDir: dir}); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	sets := listSets(t, dir)
	if len(sets) != 1 || !strings.HasSuffix(sets[0], "-panic") {
		t.Fatalf("sets = %v, want one set, kept with the panic sets", sets)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, sets[0], "serial.log")); string(b) != panicTrace {
		t.Errorf("saved serial.log = %q", b)
	}
}

func TestEnsureUpSavesALivePanicOnARunningVM(t *testing.T) {
	inst, state := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(inst, "serial.log"), panicTrace+"[    0.0] Linux version 7.0\n")
	f := proc.NewFakeRunner()
	scriptedVM(f)
	scriptRealizedConfig(f, vm, strings.Replace(matchingRealizedConfigJSON(vm, tree), "{", `{"dir":"`+inst+`",`, 1))
	l := New(f, vm, common.Options{})
	dir := filepath.Join(state, "guest-logs")
	var warned []string
	cfg := backend.Config{MachineName: vm, ProjectTree: tree, GuestLogDir: dir,
		Warn: func(format string, args ...any) { warned = append(warned, format) }}
	if err := l.EnsureUp(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	if sets := listSets(t, dir); len(sets) != 1 || !strings.HasSuffix(sets[0], "-panic") {
		t.Fatalf("sets = %v, want one panic set", sets)
	}
	if !strings.Contains(strings.Join(warned, "\n"), "kernel panic") {
		t.Errorf("no warning about the panic: %q", warned)
	}
}
