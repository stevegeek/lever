package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
)

func okDial(string) error   { return nil }
func failDial(string) error { return errors.New("connection refused") }

func writeBrokerPID(t *testing.T, st brokerctl.State, pid int) {
	t.Helper()
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.PID(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckBrokerAliveNotStarted(t *testing.T) {
	r := checkBrokerAlive(brokerctl.StateDir(t.TempDir()), 8443, okDial)
	if r.ok {
		t.Fatal("no broker.pid must fail the check")
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("fix should point at lever apply/up: %q", r.fix)
	}
}

func TestCheckBrokerAliveStalePID(t *testing.T) {
	st := brokerctl.StateDir(t.TempDir())
	writeBrokerPID(t, st, 2147483646) // no such process
	r := checkBrokerAlive(st, 8443, okDial)
	if r.ok {
		t.Fatal("a stale pid (process gone) must fail even if a dial would succeed")
	}
	if !strings.Contains(r.detail, "gone") {
		t.Fatalf("detail should say the process is gone: %q", r.detail)
	}
}

func TestCheckBrokerAliveAliveButNotListening(t *testing.T) {
	st := brokerctl.StateDir(t.TempDir())
	writeBrokerPID(t, st, os.Getpid()) // alive
	r := checkBrokerAlive(st, 8443, failDial)
	if r.ok {
		t.Fatal("alive process but nothing on the jail port must fail")
	}
	if !strings.Contains(r.detail, "listening") {
		t.Fatalf("detail should mention nothing is listening: %q", r.detail)
	}
}

func TestCheckBrokerAliveHealthy(t *testing.T) {
	st := brokerctl.StateDir(t.TempDir())
	writeBrokerPID(t, st, os.Getpid())
	r := checkBrokerAlive(st, 8443, okDial)
	if !r.ok {
		t.Fatalf("alive process + listening port must pass; got %+v", r)
	}
}

func TestCheckExternalBackendsNoneDeclared(t *testing.T) {
	// A supervised (non-external) tool must not be probed — a down/absent probe
	// for it would be a false alarm.
	tools := []config.Tool{{Name: "db", Command: []string{"lever-tool-db"}, Backend: "127.0.0.1:3201"}}
	r := checkExternalBackends(tools, failDial)
	if !r.ok {
		t.Fatalf("no external tools => pass (nothing to probe); got %+v", r)
	}
}

func TestCheckExternalBackendsAllReachable(t *testing.T) {
	tools := []config.Tool{
		{Name: "things3", External: true, Backend: "127.0.0.1:3300"},
		{Name: "qmd", External: true, Backend: "127.0.0.1:3101/mcp"},
	}
	r := checkExternalBackends(tools, okDial)
	if !r.ok {
		t.Fatalf("all backends reachable => pass; got %+v", r)
	}
}

func TestCheckExternalBackendsSomeDown(t *testing.T) {
	var dialed []string
	dial := func(addr string) error {
		dialed = append(dialed, addr)
		if addr == "127.0.0.1:3300" {
			return errors.New("refused")
		}
		return nil
	}
	tools := []config.Tool{
		{Name: "things3", External: true, Backend: "127.0.0.1:3300"},
		{Name: "qmd", External: true, Backend: "127.0.0.1:3101/mcp"},
	}
	r := checkExternalBackends(tools, dial)
	if r.ok {
		t.Fatal("a down backend must fail the check")
	}
	if !strings.Contains(r.detail, "things3") {
		t.Fatalf("detail must name the down tool: %q", r.detail)
	}
	// qmd's path must be stripped before dialing (dial a host:port, not a URL path).
	found := false
	for _, a := range dialed {
		if a == "127.0.0.1:3101" {
			found = true
		}
	}
	if !found {
		t.Fatalf("qmd backend path must be stripped for the dial; dialed=%v", dialed)
	}
}

func TestCheckScionProjectConsistent(t *testing.T) {
	st := backend.ScionProjectState{
		MarkerPresent: true,
		Entries:       []backend.ScionProjectEntry{{Name: "lever__abc", WorkspacePath: "/lever"}},
	}
	if r := checkScionProject(st, "/lever"); !r.ok {
		t.Fatalf("one registration + marker present => pass; got %+v", r)
	}
}

func TestCheckScionProjectNoRegistration(t *testing.T) {
	// A grove's registration for a different path must not implicate /lever.
	st := backend.ScionProjectState{
		MarkerPresent: false,
		Entries:       []backend.ScionProjectEntry{{Name: "scratch__x", WorkspacePath: "/lever/groves/scratch"}},
	}
	if r := checkScionProject(st, "/lever"); !r.ok {
		t.Fatalf("no registration for the tree => pass; got %+v", r)
	}
}

func TestCheckScionProjectRegisteredButMarkerGone(t *testing.T) {
	// The exact bad-teardown bug: registered for /lever, but the marker is gone.
	st := backend.ScionProjectState{
		MarkerPresent: false,
		Entries: []backend.ScionProjectEntry{
			{Name: "lever__abc", WorkspacePath: "/lever"},
			{Name: "scratch__x", WorkspacePath: "/lever/groves/scratch"},
		},
	}
	r := checkScionProject(st, "/lever")
	if r.ok {
		t.Fatal("registered for /lever but marker gone must fail")
	}
	if !strings.Contains(r.detail, "lever__abc") || !strings.Contains(r.detail, "marker") {
		t.Fatalf("detail should name the entry + the missing marker: %q", r.detail)
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("fix should point at lever apply: %q", r.fix)
	}
}

func TestCheckScionProjectDuplicateRegistrations(t *testing.T) {
	// Two entries for /lever even with the marker present — a duplicate that
	// scion init trips over.
	st := backend.ScionProjectState{
		MarkerPresent: true,
		Entries: []backend.ScionProjectEntry{
			{Name: "lever__old", WorkspacePath: "/lever"},
			{Name: "lever__new", WorkspacePath: "/lever"},
		},
	}
	r := checkScionProject(st, "/lever")
	if r.ok {
		t.Fatal("two registrations for /lever must fail (duplicate)")
	}
	if !strings.Contains(r.detail, "duplicate") {
		t.Fatalf("detail should say duplicate: %q", r.detail)
	}
}

func TestCheckCredentialFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("tok"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{"unset is a pass", "", true},
		{"present 0600 non-empty", good, true},
		{"missing file", filepath.Join(dir, "absent"), false},
		{"empty file", empty, false},
		{"group/other readable", loose, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCredentialFile(tc.path)
			if got.ok != tc.ok {
				t.Fatalf("ok = %v (detail: %s), want %v", got.ok, got.detail, tc.ok)
			}
			if strings.Contains(got.detail, "tok") {
				t.Fatalf("detail leaked file contents: %s", got.detail)
			}
		})
	}
}
