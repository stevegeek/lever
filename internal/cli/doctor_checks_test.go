package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
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
	// A worker's registration for a different path must not implicate /lever.
	st := backend.ScionProjectState{
		MarkerPresent: false,
		Entries:       []backend.ScionProjectEntry{{Name: "scratch__x", WorkspacePath: "/lever/workers/scratch"}},
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
			{Name: "scratch__x", WorkspacePath: "/lever/workers/scratch"},
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

// TestCheckMcpJsonInTree covers the real bug Stephen hit: a .mcp.json
// anywhere under the instance tree is auto-loaded by Claude as PROJECT
// scope inside every jailed agent, colliding with the brokered USER-scope
// tools lever-agent registers (duplicate localhost:PORT endpoints).
func TestCheckMcpJsonInTreeNone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkMcpJsonInTree(dir)
	if !r.ok {
		t.Fatalf("no .mcp.json anywhere in the tree => pass; got %+v", r)
	}
}

func TestCheckMcpJsonInTreeAtRoot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkMcpJsonInTree(dir)
	if r.ok {
		t.Fatal(".mcp.json at the tree root must fail the check")
	}
	if !strings.Contains(r.detail, p) {
		t.Fatalf("detail must name the offending path: %q", r.detail)
	}
	if !strings.Contains(r.fix, "user scope") {
		t.Fatalf("fix should explain the user-scope collision: %q", r.fix)
	}
}

func TestCheckMcpJsonInTreeNested(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "workspace", "assistant")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, ".mcp.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkMcpJsonInTree(dir)
	if r.ok {
		t.Fatal("a nested .mcp.json must fail the check (walk, not just top-level)")
	}
	if !strings.Contains(r.detail, p) {
		t.Fatalf("detail must name the nested offending path: %q", r.detail)
	}
}

// TestCheckGoToolchain covers the real pain point Stephen hit: `lever
// up`/`apply` cross-compile scion and shell out to `go`, and an asdf shim on
// PATH that isn't actually resolvable blows up as "exit status 126" deep
// inside apply instead of an up-front, actionable diagnosis.
func TestCheckGoToolchainBuildNotRequired(t *testing.T) {
	r := checkGoToolchain(config.ScionConfig{})
	if !r.ok {
		t.Fatalf("no source and no version pinned => no build => pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "not required") {
		t.Fatalf("detail should say a build isn't required: %q", r.detail)
	}
}

func TestCheckGoToolchainProbeOK(t *testing.T) {
	orig := goVersionProbe
	defer func() { goVersionProbe = orig }()
	goVersionProbe = func() (string, error) { return "go version go1.26.4 darwin/arm64\n", nil }

	r := checkGoToolchain(config.ScionConfig{Version: "666333f9"})
	if !r.ok {
		t.Fatalf("a working go on PATH must pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "go1.26.4") {
		t.Fatalf("detail should report the go version: %q", r.detail)
	}
}

func TestCheckGoToolchainProbeError(t *testing.T) {
	orig := goVersionProbe
	defer func() { goVersionProbe = orig }()
	goVersionProbe = func() (string, error) { return "", errors.New("exit status 126") }

	r := checkGoToolchain(config.ScionConfig{Source: "/Users/stephen/ai/scion"})
	if r.ok {
		t.Fatal("a broken go (e.g. a dead asdf shim) must fail the check")
	}
	if !strings.Contains(r.detail, "126") {
		t.Fatalf("detail should name the underlying error: %q", r.detail)
	}
	if !strings.Contains(r.fix, "PATH") {
		t.Fatalf("fix should point at PATH: %q", r.fix)
	}
}

func TestCheckOperatorSkills(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(tree, "workers", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := &config.App{Tree: tree, Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}}}
	stateDir := filepath.Join(root, ".lever-state")

	// Unscaffolded → fail with `lever init` hint.
	res := checkOperatorSkills(app, stateDir)
	if res.ok {
		t.Fatalf("unscaffolded must fail: %+v", res)
	}
	if !strings.Contains(res.fix, "lever init") {
		t.Fatalf("fix must mention lever init: %+v", res)
	}

	// Scaffold → pass.
	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureClaudeMDBlock(tree, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	if res = checkOperatorSkills(app, stateDir); !res.ok {
		t.Fatalf("scaffolded must pass: %+v", res)
	}

	// Owner edit → fail, informational wording (mentions --force).
	op := filepath.Join(tree, ".claude", "skills", "lever-operator", "SKILL.md")
	if err := os.WriteFile(op, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res = checkOperatorSkills(app, stateDir); res.ok || !strings.Contains(res.fix, "--force") {
		t.Fatalf("owner-edit must fail with --force hint: %+v", res)
	}

	// Adopt the customization → pass again, detail names the adoption.
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}
	if res = checkOperatorSkills(app, stateDir); !res.ok {
		t.Fatalf("adopted must pass: %+v", res)
	}
	if !strings.Contains(res.detail, "adopted") {
		t.Fatalf("detail should name the adoption: %+v", res)
	}

	// Drift PAST the adopted baseline → fail with tamper-aware wording.
	if err := os.WriteFile(op, []byte("edited again"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = checkOperatorSkills(app, stateDir)
	if res.ok || !strings.Contains(res.detail, "modified since adoption") {
		t.Fatalf("post-adoption drift must fail with adoption wording: %+v", res)
	}
	if !strings.Contains(res.fix, "--adopt") || !strings.Contains(res.fix, "--force") {
		t.Fatalf("fix must offer re-adopt and restore: %+v", res)
	}
}
