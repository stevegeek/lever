package guest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// profileSandbox runs guestProfileScript against a temp root with a stub
// systemctl that keeps unit state in files: units/<u> holds the
// is-enabled answer (a missing file is a unit the guest does not have),
// active/<u> exists while the unit runs, fail/<u> makes every change of
// that unit fail.
type profileSandbox struct {
	t     *testing.T
	root  string
	paths profilePaths
	bin   string
	log   string
}

func newProfileSandbox(t *testing.T) *profileSandbox {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := t.TempDir()
	s := &profileSandbox{t: t, root: root, bin: filepath.Join(root, "bin"), log: filepath.Join(root, "calls.log")}
	s.paths = profilePaths{
		State:      filepath.Join(root, "var/lib/lever/guest-profile-disabled-units"),
		ZramUnit:   filepath.Join(root, "etc/systemd/system/"+ZramUnitName),
		ZramHelper: filepath.Join(root, "usr/local/sbin/lever-zram-swap"),
		ZramDev:    filepath.Join(root, "run/lever-zram-swap.dev"),
	}
	for _, d := range []string{s.bin, filepath.Join(root, "units"), filepath.Join(root, "active"), filepath.Join(root, "fail")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := shellSingleQuote(root)
	stub := `echo "systemctl $*" >> ` + shellSingleQuote(s.log) + `
D=` + d + `
cmd=$1; shift
now=0
if [ "${1:-}" = --now ]; then now=1; shift; fi
if [ "${1:-}" = --quiet ]; then shift; fi
case "$cmd" in
list-unit-files)
  for a in "$@"; do case "$a" in --*) ;; *) [ -f "$D/units/$a" ] && echo "$a $(cat "$D/units/$a")" ;; esac; done
  exit 0 ;;
is-enabled) cat "$D/units/$1" 2>/dev/null || { echo not-found; exit 1; } ;;
is-active) [ -f "$D/active/$1" ] ;;
disable) [ -f "$D/fail/$1" ] && exit 1; echo disabled > "$D/units/$1"; rm -f "$D/active/$1" ;;
enable) [ -f "$D/fail/$1" ] && exit 1; echo enabled > "$D/units/$1"; [ "$now" = 1 ] && touch "$D/active/$1"; exit 0 ;;
start) [ -f "$D/fail/$1" ] && exit 1; touch "$D/active/$1" ;;
daemon-reload) ;;
*) exit 1 ;;
esac`
	if err := os.WriteFile(filepath.Join(s.bin, "systemctl"), []byte("#!/bin/sh\n"+stub+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// unit sets a unit's is-enabled state ("" removes the unit) and whether it runs.
func (s *profileSandbox) unit(name, state string, active bool) {
	s.t.Helper()
	p := filepath.Join(s.root, "units", name)
	if state == "" {
		_ = os.Remove(p)
	} else if err := os.WriteFile(p, []byte(state+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
	a := filepath.Join(s.root, "active", name)
	if active {
		_ = os.WriteFile(a, nil, 0o644)
	} else {
		_ = os.Remove(a)
	}
}

func (s *profileSandbox) failing(name string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.root, "fail", name), nil, 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *profileSandbox) state(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(s.root, "units", name))
	_, aerr := os.Stat(filepath.Join(s.root, "active", name))
	return strings.TrimSpace(string(b)), err == nil && aerr == nil
}

func (s *profileSandbox) run(profile string) (GuestReport, string) {
	s.t.Helper()
	_ = os.Remove(s.log)
	cmd := exec.Command("bash", "-c", guestProfileScript(profile, s.paths))
	cmd.Env = []string{"PATH=" + s.bin + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("script failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(s.log)
	return parseGuestReport(string(out)), string(calls)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func TestGuestProfileSmallTrimsAndAddsZram(t *testing.T) {
	s := newProfileSandbox(t)
	s.unit("snapd.service", "enabled", true)
	s.unit("snapd.socket", "enabled", true)
	s.unit("apt-daily.timer", "enabled", true)
	s.unit("unattended-upgrades.service", "enabled", true)
	s.unit("multipathd.service", "disabled", false) // the operator's choice: never touched
	s.unit("snapd.seeded.service", "static", false) // not enabled: not lever's to disable
	// ModemManager.service is absent.

	rep, calls := s.run(GuestProfileSmall)
	if len(rep.Warn) != 0 || len(rep.Reboot) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	for _, u := range []string{"snapd.service", "snapd.socket", "apt-daily.timer", "unattended-upgrades.service"} {
		if st, active := s.state(u); st != "disabled" || active {
			t.Errorf("%s: state %q active %t, want disabled and stopped", u, st, active)
		}
	}
	if got := readLines(t, s.paths.State); strings.Join(got, " ") != "snapd.socket snapd.service unattended-upgrades.service apt-daily.timer" {
		t.Errorf("recorded units = %v", got)
	}
	for _, untouched := range []string{"multipathd.service", "snapd.seeded.service", "ModemManager.service"} {
		if strings.Contains(calls, "disable --now "+untouched) {
			t.Errorf("%s was disabled, but lever did not enable it", untouched)
		}
	}
	for path, mode := range map[string]os.FileMode{s.paths.ZramHelper: 0o755, s.paths.ZramUnit: 0o644} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v mode %v, want %v", path, err, fi, mode)
		}
	}
	unit, _ := os.ReadFile(s.paths.ZramUnit)
	if !strings.Contains(string(unit), "ExecStart="+s.paths.ZramHelper+" start") || !strings.Contains(string(unit), "WantedBy=swap.target") {
		t.Errorf("zram unit:\n%s", unit)
	}
	if st, active := s.state(ZramUnitName); st != "enabled" || !active {
		t.Errorf("zram swap: state %q active %t, want enabled and running", st, active)
	}
	if !strings.Contains(calls, "daemon-reload") {
		t.Error("a new unit file needs a daemon-reload")
	}

	// A second run changes nothing and reloads nothing.
	rep, calls = s.run(GuestProfileSmall)
	if len(rep.Warn) != 0 || len(rep.Reboot) != 0 || strings.Contains(calls, "disable") || strings.Contains(calls, "daemon-reload") || strings.Contains(calls, "start ") {
		t.Fatalf("second run: report %+v calls:\n%s", rep, calls)
	}
	if got := readLines(t, s.paths.State); len(got) != 4 {
		t.Errorf("a re-run must not record a unit twice: %v", got)
	}
}

func TestGuestProfileSmallWarnings(t *testing.T) {
	s := newProfileSandbox(t)
	s.unit("snapd.service", "enabled", true)
	s.failing("snapd.service")
	s.failing(ZramUnitName)
	rep, _ := s.run(GuestProfileSmall)
	joined := strings.Join(rep.Warn, "\n")
	if !strings.Contains(joined, "could not disable snapd.service") || !strings.Contains(joined, "zram swap did not start") {
		t.Fatalf("warnings = %q", rep.Warn)
	}
	if got := readLines(t, s.paths.State); len(got) != 0 {
		t.Errorf("a unit lever could not disable must not be recorded: %v", got)
	}
}

// A changed swap unit on a guest where the old one runs is not restarted
// under the agents: it applies at the next boot.
func TestGuestProfileSmallChangedZramWaitsForReboot(t *testing.T) {
	s := newProfileSandbox(t)
	if err := os.MkdirAll(filepath.Dir(s.paths.ZramUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.paths.ZramUnit, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.unit(ZramUnitName, "enabled", true)
	rep, calls := s.run(GuestProfileSmall)
	if len(rep.Reboot) != 1 || !strings.Contains(rep.Reboot[0], "zram") {
		t.Fatalf("report = %+v", rep)
	}
	if strings.Contains(calls, "start "+ZramUnitName) {
		t.Fatalf("a running swap must not be restarted:\n%s", calls)
	}
}

// Back to standard: exactly the units lever disabled come back, and the
// swap goes.
func TestGuestProfileStandardUndoesSmall(t *testing.T) {
	s := newProfileSandbox(t)
	s.unit("snapd.service", "enabled", true)
	s.unit("multipathd.service", "disabled", false)
	s.run(GuestProfileSmall)
	s.unit("apt-daily.timer", "disabled", false) // disabled by the operator after the fact

	rep, calls := s.run(GuestProfileStandard)
	if len(rep.Warn) != 0 {
		t.Fatalf("warnings = %q", rep.Warn)
	}
	if st, active := s.state("snapd.service"); st != "enabled" || !active {
		t.Errorf("snapd.service: %q active %t, want enabled and running again", st, active)
	}
	if st, _ := s.state("multipathd.service"); st != "disabled" {
		t.Errorf("multipathd.service: %q; lever never disabled it, so it must not enable it", st)
	}
	if strings.Contains(calls, "apt-daily.timer") {
		t.Errorf("apt-daily.timer was not lever's to restore:\n%s", calls)
	}
	if !strings.Contains(calls, "disable --now "+ZramUnitName) {
		t.Errorf("the swap was not stopped:\n%s", calls)
	}
	for _, p := range []string{s.paths.State, s.paths.ZramUnit, s.paths.ZramHelper} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists (%v)", p, err)
		}
	}
	// And standard on a guest small never touched calls nothing.
	_, calls = s.run(GuestProfileStandard)
	if calls != "" {
		t.Fatalf("an untouched guest got systemctl calls:\n%s", calls)
	}
}

// A unit that will not come back stays recorded, so the next apply retries it.
func TestGuestProfileStandardKeepsUnitsItCouldNotRestore(t *testing.T) {
	s := newProfileSandbox(t)
	s.unit("snapd.service", "enabled", true)
	s.unit("apt-daily.timer", "enabled", true)
	s.run(GuestProfileSmall)
	s.failing("snapd.service")
	rep, _ := s.run(GuestProfileStandard)
	if len(rep.Warn) != 1 || !strings.Contains(rep.Warn[0], "snapd.service") {
		t.Fatalf("warnings = %q", rep.Warn)
	}
	if got := readLines(t, s.paths.State); strings.Join(got, " ") != "snapd.service" {
		t.Fatalf("record after a failed restore = %v, want only snapd.service", got)
	}
	if st, _ := s.state("apt-daily.timer"); st != "enabled" {
		t.Errorf("apt-daily.timer: %q, want enabled", st)
	}
	_ = os.Remove(filepath.Join(s.root, "fail", "snapd.service"))
	if rep, _ = s.run(GuestProfileStandard); len(rep.Warn) != 0 {
		t.Fatalf("retry warnings = %q", rep.Warn)
	}
	if _, err := os.Stat(s.paths.State); !os.IsNotExist(err) {
		t.Fatalf("record not removed after the retry (%v)", err)
	}
}

func TestEnsureGuestProfileRunsAsRoot(t *testing.T) {
	shape := prefixShapes("lever-x")[1]
	f := proc.NewFakeRunner()
	f.Script(strings.Join(shape.rootPrefix, " "), proc.Result{Stdout: "warn: w\n"})
	g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}
	rep, err := g.EnsureGuestProfile(context.Background(), GuestProfileSmall)
	if err != nil || len(rep.Warn) != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if len(f.Calls) != 1 || !equalPrefix(f.Calls[0].Args, append(append([]string{}, shape.rootPrefix[1:]...), "bash", "-c")) {
		t.Fatalf("want one root bash call, got %+v", f.Calls)
	}
	script := f.Calls[0].Args[len(f.Calls[0].Args)-1]
	if strings.Contains(script, "apt-get") || strings.Contains(script, "systemctl reboot") {
		t.Error("the profile script must install nothing and never reboot")
	}
	// No profile (no config): no guest call at all.
	if _, err := g.EnsureGuestProfile(context.Background(), ""); err != nil || len(f.Calls) != 1 {
		t.Fatalf("empty profile: err=%v calls=%d", err, len(f.Calls))
	}
	if _, err := g.EnsureGuestProfile(context.Background(), "tiny"); err == nil {
		t.Fatal("an unknown profile must fail")
	}
	f.Script(strings.Join(shape.rootPrefix, " "), proc.Result{Code: 1, Stderr: "read-only file system"})
	if _, err := g.EnsureGuestProfile(context.Background(), GuestProfileStandard); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("want the failure, got %v", err)
	}
}

// The helper is valid sh and refuses an unknown verb.
func TestZramHelperSyntax(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	p := filepath.Join(t.TempDir(), "h")
	if err := os.WriteFile(p, []byte(zramHelper(realProfilePaths)), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	if err := exec.Command("sh", p, "bogus").Run(); err == nil {
		t.Fatal("an unknown verb must fail")
	}
	// stop with nothing recorded is a no-op success.
	pp := realProfilePaths
	pp.ZramDev = filepath.Join(t.TempDir(), "none")
	if err := os.WriteFile(p, []byte(zramHelper(pp)), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", p, "stop").CombinedOutput(); err != nil {
		t.Fatalf("stop with no device: %v\n%s", err, out)
	}
}
