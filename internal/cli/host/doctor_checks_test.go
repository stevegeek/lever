package host

import (
	"context"
	"errors"
	"fmt"
	"github.com/stevegeek/lever/internal/jail"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

func okDial(string) error   { return nil }
func failDial(string) error { return errors.New("connection refused") }

// okProbes/failProbes are doctorProbes whose TCP dial always succeeds/fails.
// Every other probe panics if reached: a check that should never leave the
// host must not silently run a real subprocess or HTTP request in a test.
var (
	okProbes   = doctorProbes{dial: okDial}
	failProbes = doctorProbes{dial: failDial}
)

// healthyRemoteProbes makes the remote-access chain look green. Every probe is
// stubbed: a test must never reach a proxy running on this host.
func healthyRemoteProbes() doctorProbes {
	return doctorProbes{
		dial:          okDial,
		remoteHealthz: func(healthzProbe) (int, error) { return 200, nil },
	}
}

func writePIDFile(t *testing.T, st state.State, path string, pid int) {
	t.Helper()
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeBrokerPID(t *testing.T, st state.State, pid int) { writePIDFile(t, st, st.PID(), pid) }
func writeRemotePID(t *testing.T, st state.State, pid int) {
	writePIDFile(t, st, st.RemotePID(), pid)
}

func TestCheckBrokerAliveNotStarted(t *testing.T) {
	r := checkBrokerAlive(state.ForConfig(t.TempDir()), 8443, okProbes)
	if r.ok {
		t.Fatal("no broker.pid must fail the check")
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("fix should point at lever apply/up: %q", r.fix)
	}
}

func TestCheckBrokerAliveStalePID(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	writeBrokerPID(t, st, 2147483646) // no such process
	r := checkBrokerAlive(st, 8443, okProbes)
	if r.ok {
		t.Fatal("a stale pid (process gone) must fail even if a dial would succeed")
	}
	if !strings.Contains(r.detail, "gone") {
		t.Fatalf("detail should say the process is gone: %q", r.detail)
	}
}

func TestCheckBrokerAliveAliveButNotListening(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	writeBrokerPID(t, st, os.Getpid()) // alive
	r := checkBrokerAlive(st, 8443, failProbes)
	if r.ok {
		t.Fatal("alive process but nothing on the jail port must fail")
	}
	if !strings.Contains(r.detail, "listening") {
		t.Fatalf("detail should mention nothing is listening: %q", r.detail)
	}
}

func TestCheckBrokerAliveHealthy(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	writeBrokerPID(t, st, os.Getpid())
	r := checkBrokerAlive(st, 8443, okProbes)
	if !r.ok {
		t.Fatalf("alive process + listening port must pass; got %+v", r)
	}
}

func TestCheckToolBackendsNoneDeclared(t *testing.T) {
	r := checkToolBackends(nil, t.TempDir(), failProbes)
	if !r.ok {
		t.Fatalf("no tools declared => pass (nothing to probe); got %+v", r)
	}
}

func TestCheckToolBackendsAllReachable(t *testing.T) {
	tools := []config.Tool{
		{Name: "tasks", External: true, Backend: "127.0.0.1:3300"},
		{Name: "notes", External: true, Backend: "127.0.0.1:3901/mcp"},
		{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:3201"},
	}
	r := checkToolBackends(tools, t.TempDir(), okProbes)
	if !r.ok {
		t.Fatalf("all external backends reachable + supervised command resolvable => pass; got %+v", r)
	}
}

func TestCheckToolBackendsSomeDown(t *testing.T) {
	var dialed []string
	dial := func(addr string) error {
		dialed = append(dialed, addr)
		if addr == "127.0.0.1:3300" {
			return errors.New("refused")
		}
		return nil
	}
	tools := []config.Tool{
		{Name: "tasks", External: true, Backend: "127.0.0.1:3300"},
		{Name: "notes", External: true, Backend: "127.0.0.1:3901/mcp"},
	}
	r := checkToolBackends(tools, t.TempDir(), doctorProbes{dial: dial})
	if r.ok {
		t.Fatal("a down backend must fail the check")
	}
	if !strings.Contains(r.detail, "tasks") {
		t.Fatalf("detail must name the down tool: %q", r.detail)
	}
	found := false
	for _, a := range dialed {
		if a == "127.0.0.1:3901" {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes backend path must be stripped for the dial; dialed=%v", dialed)
	}
}

func TestCheckToolBackendsSupervisedMissing(t *testing.T) {
	tools := []config.Tool{{Name: "db", Command: []string{"definitely-not-on-path-xyz"}}}
	got := checkToolBackends(tools, t.TempDir(), okProbes)
	if got.ok {
		t.Fatalf("supervised tool with missing binary should fail the check")
	}
}

func TestCheckToolBackendsExternalDown(t *testing.T) {
	tools := []config.Tool{{Name: "x", External: true, Backend: "127.0.0.1:59999"}}
	got := checkToolBackends(tools, t.TempDir(), failProbes)
	if got.ok {
		t.Fatalf("down external backend should fail the check")
	}
}

// TestCheckToolBackendsSupervisedNeverDialed: a supervised tool's Backend is
// never dialed; the dial always fails here, so dialing would fail the check.
func TestCheckToolBackendsSupervisedNeverDialed(t *testing.T) {
	tools := []config.Tool{{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:59999"}}
	if got := checkToolBackends(tools, t.TempDir(), failProbes); !got.ok {
		t.Fatalf("supervised tool's Backend must never be dialed; got %+v", got)
	}
}

// A missing absolute-path supervised binary fails like a bare name not on
// PATH.
func TestCheckToolBackendsSupervisedAbsolutePathMissing(t *testing.T) {
	tools := []config.Tool{{Name: "db", Command: []string{"/nonexistent/definitely-not-here-xyz"}}}
	got := checkToolBackends(tools, t.TempDir(), okProbes)
	if got.ok {
		t.Fatalf("supervised tool with a missing absolute-path command should fail the check")
	}
}

// A relative supervised command resolves against the instance dir (where
// the supervisor runs the tool), not against doctor's own cwd.
func TestCheckToolBackendsRelativeCommandUsesInstanceDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := []config.Tool{{Name: "rel", Command: []string{"bin/tool"}}}
	if got := checkToolBackends(tools, dir, failProbes); !got.ok {
		t.Fatalf("bin/tool under the instance dir should pass: %+v", got)
	}
	if got := checkToolBackends(tools, t.TempDir(), failProbes); got.ok {
		t.Fatal("bin/tool under another dir should fail")
	}
}

func TestCheckProjectSharedDirsNone(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.SharedDir, error) { return nil, nil }
	r := checkProjectSharedDirs(context.Background(), "lever", list)
	if !r.ok {
		t.Fatalf("no shared dirs => pass; got %+v", r)
	}
}

func TestCheckProjectSharedDirsFlagsScratchpad(t *testing.T) {
	// scion's default: a writable dir mounted into every agent must fail, name
	// itself and offer a fix.
	list := func(context.Context, string) ([]hubapi.SharedDir, error) {
		return []hubapi.SharedDir{{Name: "scratchpad"}}, nil
	}
	r := checkProjectSharedDirs(context.Background(), "lever", list)
	if r.ok {
		t.Fatalf("a shared dir mounted into every agent must fail; got %+v", r)
	}
	if !strings.Contains(r.detail, "scratchpad") || !strings.Contains(r.detail, "lever") {
		t.Errorf("detail should name the dir and the project, got %q", r.detail)
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Errorf("fix should point at the strip, got %q", r.fix)
	}
}

func TestCheckProjectSharedDirsReadOnlyIsLabelled(t *testing.T) {
	// A read-only entry is still a channel, so it fails, but the detail says it is
	// not writable.
	list := func(context.Context, string) ([]hubapi.SharedDir, error) {
		return []hubapi.SharedDir{{Name: "refs", ReadOnly: true}}, nil
	}
	r := checkProjectSharedDirs(context.Background(), "lever", list)
	if r.ok {
		t.Fatalf("a read-only shared dir must still fail; got %+v", r)
	}
	if !strings.Contains(r.detail, "read-only") {
		t.Errorf("detail should label a read-only entry, got %q", r.detail)
	}
}

func TestCheckProjectSharedDirsSkipsOnUnreachableHub(t *testing.T) {
	// An unreachable hub passes (the broker check covers it) but says it was
	// skipped.
	list := func(context.Context, string) ([]hubapi.SharedDir, error) {
		return nil, errors.New("connection refused")
	}
	r := checkProjectSharedDirs(context.Background(), "lever", list)
	if !r.ok {
		t.Fatalf("an unreachable hub must not be a finding; got %+v", r)
	}
	if !strings.Contains(r.detail, "not checked") {
		t.Errorf("detail must say the check was skipped, got %q", r.detail)
	}
}

func TestCheckProjectSharedDirsFailsWhenTheHubAnswered(t *testing.T) {
	// A 403 or an unlisted project is not a down instance: anything the hub
	// answered must fail rather than hide behind "not checked".
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", &hubapi.APIError{Status: 403, Msg: "GET /api/v1/projects: HTTP 403: Forbidden"}},
		{"no such project", &hubapi.APIError{Msg: `no project named "lever" at hub http://127.0.0.1:8080`}},
		{"undecodable body", &hubapi.APIError{Msg: "GET /api/v1/projects: decoding response: invalid character '<'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := func(context.Context, string) ([]hubapi.SharedDir, error) { return nil, tc.err }
			r := checkProjectSharedDirs(context.Background(), "lever", list)
			if r.ok {
				t.Fatalf("an answer from the hub must not pass as skipped; got %+v", r)
			}
			if !strings.Contains(r.detail, tc.err.Error()) {
				t.Errorf("detail should carry the hub's answer, got %q", r.detail)
			}
			if r.fix == "" {
				t.Error("a finding must offer a fix")
			}
		})
	}
}

func TestHubProjectKeyMatchesTheTokenMintKey(t *testing.T) {
	// The strip and the check must use the key ensureControllerPAT mints with.
	if got := hubProjectKey("/lever"); got != "lever" {
		t.Fatalf("hubProjectKey(/lever) = %q, want lever", got)
	}
	if hubProjectKey("/lever") != filepath.Base(apply.JailPath("/anything", "/anything", "/lever")) {
		t.Error("hubProjectKey must match the key ensureControllerPAT mints the PAT with")
	}
}

func TestCheckScionProjectConsistent(t *testing.T) {
	st := types.ScionProjectState{
		MarkerPresent: true,
		Entries:       []types.ScionProjectEntry{{Name: "lever__abc", WorkspacePath: "/lever"}},
	}
	if r := checkScionProject(st, "/lever"); !r.ok {
		t.Fatalf("one registration + marker present => pass; got %+v", r)
	}
}

func TestCheckScionProjectNoRegistration(t *testing.T) {
	// A worker's registration for a different path must not implicate /lever.
	st := types.ScionProjectState{
		MarkerPresent: false,
		Entries:       []types.ScionProjectEntry{{Name: "scratch__x", WorkspacePath: "/lever/workers/scratch"}},
	}
	if r := checkScionProject(st, "/lever"); !r.ok {
		t.Fatalf("no registration for the tree => pass; got %+v", r)
	}
}

func TestCheckScionProjectRegisteredButMarkerGone(t *testing.T) {
	st := types.ScionProjectState{
		MarkerPresent: false,
		Entries: []types.ScionProjectEntry{
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
	// A duplicate registration that scion init trips over, even with the marker
	// present.
	st := types.ScionProjectState{
		MarkerPresent: true,
		Entries: []types.ScionProjectEntry{
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

// TestCheckMcpJsonInTree: Claude loads a .mcp.json under the tree as project
// scope in every jailed agent, colliding with the brokered user-scope tools.
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

// TestCheckGoToolchain: an unresolvable asdf shim otherwise fails as "exit
// status 126" deep inside apply.
func TestCheckGoToolchainBuildNotRequired(t *testing.T) {
	r := checkGoToolchain(config.ScionConfig{}, false, doctorProbes{})
	if !r.ok {
		t.Fatalf("no source and no version pinned => no build => pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "not required") {
		t.Fatalf("detail should say a build isn't required: %q", r.detail)
	}
}

func TestCheckGoToolchainProbeOK(t *testing.T) {
	p := doctorProbes{goVersion: func() (string, error) { return "go version go1.26.4 darwin/arm64\n", nil }}

	r := checkGoToolchain(config.ScionConfig{Version: "666333f9"}, false, p)
	if !r.ok {
		t.Fatalf("a working go on PATH must pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "go1.26.4") {
		t.Fatalf("detail should report the go version: %q", r.detail)
	}
}

func TestCheckGoToolchainNotNeededForAPrebuiltScion(t *testing.T) {
	p := doctorProbes{goVersion: func() (string, error) { return "", errors.New("executable file not found") }}
	r := checkGoToolchain(config.ScionConfig{Version: scionbin.ReleasePin}, true, p)
	if !r.ok || !strings.Contains(r.detail, "prebuilt") {
		t.Fatalf("a downloaded scion needs no Go; got %+v", r)
	}
	ok := doctorProbes{goVersion: func() (string, error) { return "go version go1.26.4 linux/amd64\n", nil }}
	if r := checkGoToolchain(config.ScionConfig{Version: scionbin.ReleasePin}, true, ok); !r.ok || !strings.Contains(r.detail, "go1.26.4") {
		t.Fatalf("a working go is still reported; got %+v", r)
	}
}

func TestScionPrebuiltFalseWithoutAMatchingRelease(t *testing.T) {
	app := &config.App{Scion: config.ScionConfig{Version: "666333f9"}}
	if scionPrebuilt(app) {
		t.Fatal("a version this lever carries no release for is built")
	}
}

func TestCheckGoToolchainProbeError(t *testing.T) {
	p := doctorProbes{goVersion: func() (string, error) { return "", errors.New("exit status 126") }}

	r := checkGoToolchain(config.ScionConfig{Source: "/home/devuser/scion"}, false, p)
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
	stateDir := state.ForConfig(root)

	res := checkOperatorSkills(app, stateDir)
	if res.ok {
		t.Fatalf("unscaffolded must fail: %+v", res)
	}
	if !strings.Contains(res.fix, "lever init") {
		t.Fatalf("fix must mention lever init: %+v", res)
	}

	if _, err := syncSkills(app, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureClaudeMDBlock(tree, stateDir, false, false); err != nil {
		t.Fatal(err)
	}
	if res = checkOperatorSkills(app, stateDir); !res.ok {
		t.Fatalf("scaffolded must pass: %+v", res)
	}

	// Owner edit fails, and the hint names both --adopt and --force. The content
	// carries a current lever-version stamp: an adopted file's stamp attests the
	// baseline it was reviewed against.
	op := filepath.Join(tree, ".claude", "skills", "lever-operator", "SKILL.md")
	edited := "---\nname: custom\nlever-version: " + cli.Version + "\n---\nmy own guidance\n"
	if err := os.WriteFile(op, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if res = checkOperatorSkills(app, stateDir); res.ok || !strings.Contains(res.fix, "--force") || !strings.Contains(res.fix, "--adopt") {
		t.Fatalf("owner-edit must fail with --adopt and --force hints: %+v", res)
	}

	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}
	if res = checkOperatorSkills(app, stateDir); !res.ok {
		t.Fatalf("adopted must pass: %+v", res)
	}
	if !strings.Contains(res.detail, "adopted") {
		t.Fatalf("detail should name the adoption: %+v", res)
	}

	// An adoption stamped with an old version fails: the baseline may have moved
	// past security-relevant guidance. The "unknown" label covers a missing stamp.
	stale := "---\nname: custom\nlever-version: 0.3.1\n---\nmy own guidance\n"
	if err := os.WriteFile(op, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}
	res = checkOperatorSkills(app, stateDir)
	if res.ok || !strings.Contains(res.detail, "0.3.1") || !strings.Contains(res.detail, cli.Version) {
		t.Fatalf("stale adopted baseline must fail naming both versions: %+v", res)
	}
	if !strings.Contains(res.fix, "--adopt") || !strings.Contains(res.fix, "--force") {
		t.Fatalf("stale-baseline fix must offer re-adopt and reclaim: %+v", res)
	}

	if err := os.WriteFile(op, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}

	// Drift past the adopted baseline fails with tamper-aware wording.
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

// writeDirectivesConfig omits operator.allowed_signers when signersRel is "".
func writeDirectivesConfig(t *testing.T, signersRel string) *config.App {
	t.Helper()
	if signersRel == "" {
		return loadInstance(t, "")
	}
	return loadInstance(t, "operator:\n  allowed_signers: "+signersRel+"\n")
}

// writeAllowedSigners writes a real ed25519 key for principal
// "operator@demo".
func writeAllowedSigners(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(t.TempDir(), "opkey")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-C", "op", "-q").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(priv + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(pub)) // type key comment
	line := "operator@demo " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDirectivesNotConfigured(t *testing.T) {
	app := writeDirectivesConfig(t, "")
	st := state.ForConfig(t.TempDir())
	r := checkDirectives(app, st)
	if !r.ok {
		t.Fatalf("unset allowed_signers must pass (channel just isn't configured): %+v", r)
	}
	if !strings.Contains(r.detail, "not configured") || !strings.Contains(r.detail, "allowed_signers") {
		t.Fatalf("detail should say not configured and name allowed_signers: %q", r.detail)
	}
}

func TestCheckDirectivesMissingFile(t *testing.T) {
	app := writeDirectivesConfig(t, "operator/allowed_signers")
	st := state.ForConfig(t.TempDir())
	r := checkDirectives(app, st)
	if r.ok {
		t.Fatal("configured but missing allowed_signers file must fail")
	}
	if !strings.Contains(r.fix, "allowed_signers") {
		t.Fatalf("fix should mention allowed_signers: %q", r.fix)
	}
}

func TestCheckDirectivesEmptyFile(t *testing.T) {
	app := writeDirectivesConfig(t, "operator/allowed_signers")
	path := app.OperatorAllowedSignersPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# no keys yet\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := state.ForConfig(t.TempDir())
	r := checkDirectives(app, st)
	if r.ok {
		t.Fatal("an allowed_signers file with zero key lines must fail")
	}
	if !strings.Contains(r.fix, "allowed_signers") {
		t.Fatalf("fix should mention allowed_signers: %q", r.fix)
	}
}

func TestCheckDirectivesHappyPathBrokerNotRunning(t *testing.T) {
	app := writeDirectivesConfig(t, "operator/allowed_signers")
	writeAllowedSigners(t, app.OperatorAllowedSignersPath())
	st := state.ForConfig(t.TempDir()) // no broker.pid => broker not running
	r := checkDirectives(app, st)
	if !r.ok {
		t.Fatalf("a real key + no broker running must pass: %+v", r)
	}
	if !strings.Contains(r.detail, "1 key") {
		t.Fatalf("detail should report the key count: %q", r.detail)
	}
}

func TestCheckDirectivesBrokerRunningSocketPresent(t *testing.T) {
	app := writeDirectivesConfig(t, "operator/allowed_signers")
	writeAllowedSigners(t, app.OperatorAllowedSignersPath())
	st := state.ForConfig(t.TempDir())
	writeBrokerPID(t, st, os.Getpid())
	if err := os.WriteFile(st.DirectiveSock(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := checkDirectives(app, st)
	if !r.ok {
		t.Fatalf("broker alive + socket present must pass: %+v", r)
	}
	if !strings.Contains(r.detail, "socket present") {
		t.Fatalf("detail should say socket present: %q", r.detail)
	}
}

func TestCheckDirectivesBrokerRunningSocketAbsent(t *testing.T) {
	app := writeDirectivesConfig(t, "operator/allowed_signers")
	writeAllowedSigners(t, app.OperatorAllowedSignersPath())
	st := state.ForConfig(t.TempDir())
	writeBrokerPID(t, st, os.Getpid()) // alive, but directive.sock never created
	r := checkDirectives(app, st)
	if r.ok {
		t.Fatal("broker alive but directive socket absent must fail")
	}
	if !strings.Contains(r.detail, "socket") {
		t.Fatalf("failure should be about the missing directive socket: %+v", r)
	}
}

func writeRemotePAT(t *testing.T, st state.State, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.RemotePAT(), []byte("scion_pat_x"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRemoteDisabled(t *testing.T) {
	app := loadInstance(t, "")
	st := state.ForConfig(t.TempDir())
	r := checkRemote(context.Background(), app, st, okProbes, nil)
	if !r.ok {
		t.Fatalf("remote disabled must pass: %+v", r)
	}
	if !strings.Contains(r.detail, "disabled") {
		t.Fatalf("detail should say disabled: %q", r.detail)
	}
}

func TestCheckRemoteNoPIDFile(t *testing.T) {
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := state.ForConfig(t.TempDir()) // no remote.pid — never started
	r := checkRemote(context.Background(), app, st, okProbes, nil)
	if r.ok {
		t.Fatal("enabled but never started (no remote.pid) must fail")
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("fix should point at lever apply: %q", r.fix)
	}
}

func TestCheckRemotePATMissing(t *testing.T) {
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := state.ForConfig(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	r := checkRemote(context.Background(), app, st, okProbes, nil)
	if r.ok {
		t.Fatal("missing remote.pat must fail")
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("fix should point at lever apply: %q", r.fix)
	}
}

// A group/other-accessible remote.pat fails, as for checkCredentialFile.
func TestCheckRemotePATBadPermissions(t *testing.T) {
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := state.ForConfig(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o644)
	r := checkRemote(context.Background(), app, st, okProbes, nil)
	if r.ok {
		t.Fatal("a group/other-readable remote.pat must fail")
	}
	if strings.Contains(r.detail, "scion_pat_x") {
		t.Fatalf("detail leaked the PAT value: %q", r.detail)
	}
}

func TestCheckRemoteHealthz500(t *testing.T) {
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := state.ForConfig(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	p := healthyRemoteProbes()
	p.remoteHealthz = func(healthzProbe) (int, error) { return 500, nil }

	r := checkRemote(context.Background(), app, st, p, nil)
	if r.ok {
		t.Fatal("a non-200 from the healthz probe must fail")
	}
	if !strings.Contains(r.detail, "500") {
		t.Fatalf("detail should mention the bad status: %q", r.detail)
	}
}

func TestCheckRemoteHealthy(t *testing.T) {
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := state.ForConfig(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	r := checkRemote(context.Background(), app, st, healthyRemoteProbes(), nil)
	if !r.ok {
		t.Fatalf("pid alive + listening + PAT present + healthz 200 must pass: %+v", r)
	}
}

// The proxy's allowed_users gate trusts the identity header. Doctor runs
// host-side, as trusted as the PAT file, so it sends the first allowed user, or
// a pinned instance would 403 its own check.
func TestCheckRemoteHealthzProbeUsesFirstAllowedUser(t *testing.T) {
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n  allowed_users: [\"steve@example.com\", \"other@example.com\"]\n")
	st := state.ForConfig(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	var got healthzProbe
	p := healthyRemoteProbes()
	p.remoteHealthz = func(hp healthzProbe) (int, error) {
		got = hp
		return 200, nil
	}

	if r := checkRemote(context.Background(), app, st, p, nil); !r.ok {
		t.Fatalf("expected pass, got %+v", r)
	}
	if got.Login != "steve@example.com" || got.Header != "Tailscale-User-Login" {
		t.Fatalf("probe should carry the first allowed_users entry in Tailscale-User-Login, got %+v", got)
	}
	if got.Addr != "127.0.0.1:8445" || got.Port != 8445 {
		t.Fatalf("probe should dial the loopback listener, got %+v", got)
	}
}

// With another front, the probe speaks its header and dials the bind address:
// a non-loopback bind has no 127.0.0.1 listener.
func TestCheckRemoteHealthzProbeFollowsIdentityHeaderAndBind(t *testing.T) {
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://vm.exe.xyz:8445\"\n"+
		"  identity_header: x-exedev-email\n  bind: 10.0.0.5\n  allowed_users: [\"me@example.com\"]\n")
	st := state.ForConfig(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	var got healthzProbe
	var dialed string
	p := healthyRemoteProbes()
	p.dial = func(addr string) error { dialed = addr; return nil }
	p.remoteHealthz = func(hp healthzProbe) (int, error) {
		got = hp
		return 200, nil
	}
	if r := checkRemote(context.Background(), app, st, p, nil); !r.ok {
		t.Fatalf("expected pass, got %+v", r)
	}
	if dialed != "10.0.0.5:8445" || got.Addr != "10.0.0.5:8445" {
		t.Fatalf("liveness dialed %q, healthz dialed %q; want the bind address", dialed, got.Addr)
	}
	if got.Header != "X-Exedev-Email" || got.Login != "me@example.com" {
		t.Fatalf("probe identity = %+v, want the configured header in canonical form", got)
	}
}

// The probe's Host is the loopback name the proxy's Host gate admits for
// host-side probes, even when it dials a non-loopback bind address.
func TestRemoteHealthzProbeSendsTheLoopbackHost(t *testing.T) {
	var gotHost, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotHeader = r.Host, r.Header.Get("X-Exedev-Email")
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	status, err := remoteHealthzProbe(healthzProbe{Addr: addr, Port: 8445, Header: "X-Exedev-Email", Login: "me@example.com"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("probe: %d, %v", status, err)
	}
	if gotHost != "127.0.0.1:8445" || gotHeader != "me@example.com" {
		t.Fatalf("Host %q, header %q", gotHost, gotHeader)
	}
}

// Each weakening setting gets a warning row, never a failure; a default
// remote block passes plainly.
func TestCheckRemoteExposure(t *testing.T) {
	base := "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n"
	if r := checkRemoteExposure(loadInstance(t, base)); !r.ok || r.fix != "" {
		t.Fatalf("default remote: want a plain pass, got %+v", r)
	}
	if r := checkRemoteExposure(loadInstance(t, "")); !r.ok || r.fix != "" {
		t.Fatalf("remote off: want a plain pass, got %+v", r)
	}
	for name, extra := range map[string]string{
		"non-loopback bind":    "  bind: 10.0.0.5\n",
		"wildcard bind":        "  bind: 0.0.0.0\n  allow_wildcard_bind: true\n  identity_header: X-ExeDev-Email\n",
		"trust_forwarded_host": "  trust_forwarded_host: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := checkRemoteExposure(loadInstance(t, base+extra))
			if !r.ok || r.fix == "" || r.detail == "" {
				t.Fatalf("want a warning row (ok with a fix), got %+v", r)
			}
		})
	}
}

func TestCheckClaudeVersion(t *testing.T) {
	probes := func(v string, err error) doctorProbes {
		return doctorProbes{claudeVersion: func(string) (string, error) { return v, err }}
	}
	if got := checkClaudeVersion("img", "", probes("2.1.207", nil)); !got.ok || !strings.Contains(got.detail, "2.1.207") {
		t.Fatalf("expected pass reporting the version, got %+v", got)
	}

	if got := checkClaudeVersion("img", "", probes("", nil)); !got.ok {
		t.Fatalf("missing label should be informational, not a failure, got %+v", got)
	}

	if got := checkClaudeVersion("img", "", probes("", fmt.Errorf("no such image"))); got.ok {
		t.Fatalf("inspect error should fail the check")
	}
}

// With an image_tar the label comes from the archive and host docker is never
// consulted; a tar missing the ref fails with the tar named.
func TestCheckClaudeVersionFromTar(t *testing.T) {
	dockerProbe := func(string) (string, error) { return "", fmt.Errorf("docker: command not found") }
	p := doctorProbes{
		claudeVersion:    dockerProbe,
		claudeVersionTar: func(tar, ref string) (string, error) { return "2.1.240", nil },
	}
	got := checkClaudeVersion("img", "/inst/images/img.tar", p)
	if !got.ok || !strings.Contains(got.detail, "2.1.240") || !strings.Contains(got.detail, "img.tar") {
		t.Fatalf("expected pass naming the version and the tar, got %+v", got)
	}
	p.claudeVersionTar = func(tar, ref string) (string, error) { return "", fmt.Errorf("image %q is not in the tar", ref) }
	got = checkClaudeVersion("img", "/inst/images/img.tar", p)
	if got.ok || !strings.Contains(got.detail, "img.tar") {
		t.Fatalf("a tar read failure must fail the check naming the tar, got %+v", got)
	}
}

// TestCheckLeverVersion: the image tag names only the arch, so the label says
// which lever built the in-jail binaries. A different release fails; no label
// or an unreadable image is "not checked".
func TestCheckLeverVersion(t *testing.T) {
	const host, hostFull = "0.28.1", "0.28.1 (21ff051f651f)"
	for _, tc := range []struct {
		name   string
		label  string
		err    error
		ok     bool
		detail string
		fix    string
	}{
		{"same build", "0.28.1 (21ff051f651f)", nil, true, "the same as the host", ""},
		{"same release, other commit", "0.28.1 (0123456789ab-dirty)", nil, true, "same release 0.28.1 (the release is compared, not the commit)", ""},
		{"same release, bare label", "0.28.1\n", nil, true, "same release 0.28.1", ""},
		{"older release", "0.7.0 (26c87cd47dcc)", nil, false, "carries lever 0.7.0 (26c87cd47dcc) but the host lever is 0.28.1 (21ff051f651f)", "make lever-image"},
		{"newer release", "0.29.0", nil, false, "carries lever 0.29.0 but the host lever is", "lever up --fresh"},
		{"release is a prefix of the host's", "0.28", nil, false, "carries lever 0.28 ", "lever apply"},
		{"no label", "", nil, true, "not checked: no lever_version label on img — the image predates the label", ""},
		{"blank label", "  \n", nil, true, "not checked: no lever_version label", ""},
		{"inspect fails", "", fmt.Errorf("no such image\nsecond line"), true, "not checked (could not inspect image img): no such image", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := doctorProbes{leverVersion: func(string) (string, error) { return tc.label, tc.err }}
			r := checkLeverVersion("img", "", host, hostFull, 3, p)
			if r.name != "agent lever version" || r.ok != tc.ok || !strings.Contains(r.detail, tc.detail) || !strings.Contains(r.fix, tc.fix) {
				t.Fatalf("got %+v, want ok=%v detail~%q fix~%q", r, tc.ok, tc.detail, tc.fix)
			}
			if tc.ok && r.fix != "" {
				t.Fatalf("a passing row carries no fix: %+v", r)
			}
			if strings.Contains(r.detail, "second line") {
				t.Fatalf("only the first line of an inspect error belongs in the detail: %q", r.detail)
			}
		})
	}
}

// With a lever_jail_abi label the ABI decides, not the release: an older
// release with unchanged in-jail code passes and names the difference; a
// different ABI fails.
func TestCheckLeverVersionJailABI(t *testing.T) {
	const host, hostFull, hostABI = "0.34.2", "0.34.2 (21ff051f651f)", 4
	for _, tc := range []struct {
		name    string
		version string
		abi     string
		ok      bool
		detail  string
		not     string
		fix     string
	}{
		{"same ABI, earlier release", "0.34.1 (0123456789ab)", "4\n", true,
			"carries jail ABI 4, built from lever 0.34.1 (0123456789ab), the same as the host lever; host lever is 0.34.2 (21ff051f651f) (info:", "--fresh", ""},
		{"same ABI, same build", hostFull, "4", true, "the same build as the host", "info:", ""},
		{"same ABI, no version label", "", "4", true, "carries jail ABI 4, the same as the host lever", "built from", ""},
		{"other ABI, same release", "0.34.2 (0123456789ab)", "3", false,
			"carries jail ABI 3, built from lever 0.34.2 (0123456789ab) but the host lever (0.34.2 (21ff051f651f)) has jail ABI 4", "", "lever up --fresh"},
		{"newer ABI", "0.35.0", "5", false, "carries jail ABI 5", "", "make lever-image"},
		{"blank ABI falls back to the release", "0.34.1 (0123456789ab)", "  ", false,
			"carries lever 0.34.1 (0123456789ab) but the host lever is 0.34.2 (21ff051f651f)", "", "make lever-image"},
		{"no ABI label, same release", "0.34.2", "", true, "same release 0.34.2", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := doctorProbes{
				leverVersion: func(string) (string, error) { return tc.version, nil },
				jailABI:      func(string) (string, error) { return tc.abi, nil },
			}
			r := checkLeverVersion("img", "", host, hostFull, hostABI, p)
			if r.ok != tc.ok || !strings.Contains(r.detail, tc.detail) || !strings.Contains(r.fix, tc.fix) ||
				(tc.not != "" && strings.Contains(r.detail+r.fix, tc.not)) {
				t.Fatalf("got %+v, want ok=%v detail~%q (without %q) fix~%q", r, tc.ok, tc.detail, tc.not, tc.fix)
			}
			if tc.ok && r.fix != "" {
				t.Fatalf("a passing row carries no fix: %+v", r)
			}
		})
	}

	p := doctorProbes{
		leverVersion:    func(string) (string, error) { return "", fmt.Errorf("docker: command not found") },
		jailABI:         func(string) (string, error) { return "", fmt.Errorf("docker: command not found") },
		leverVersionTar: func(tar, ref string) (string, error) { return "0.34.1", nil },
		jailABITar:      func(tar, ref string) (string, error) { return "4", nil },
	}
	if r := checkLeverVersion("img", "/inst/images/img.tar", host, hostFull, hostABI, p); !r.ok || !strings.Contains(r.detail, "img.tar") || !strings.Contains(r.detail, "jail ABI 4") {
		t.Fatalf("tar, same ABI: %+v", r)
	}
	p.jailABITar = func(tar, ref string) (string, error) { return "", fmt.Errorf("unreadable") }
	if r := checkLeverVersion("img", "/inst/images/img.tar", host, hostFull, hostABI, p); !r.ok || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("tar, ABI unreadable: %+v", r)
	}
}

func TestCheckLeverVersionFromTar(t *testing.T) {
	p := doctorProbes{
		leverVersion:    func(string) (string, error) { return "", fmt.Errorf("docker: command not found") },
		leverVersionTar: func(tar, ref string) (string, error) { return "0.27.0 (aaaaaaaaaaaa)", nil },
	}
	got := checkLeverVersion("img", "/inst/images/img.tar", "0.28.1", "0.28.1 (21ff051f651f)", 3, p)
	if got.ok || !strings.Contains(got.detail, "0.27.0") || !strings.Contains(got.detail, "img.tar") {
		t.Fatalf("expected a failure naming the version and the tar, got %+v", got)
	}
	p.leverVersionTar = func(tar, ref string) (string, error) { return "", fmt.Errorf("image %q is not in the tar", ref) }
	got = checkLeverVersion("img", "/inst/images/img.tar", "0.28.1", "0.28.1 (21ff051f651f)", 3, p)
	if !got.ok || !strings.Contains(got.detail, "not checked") || !strings.Contains(got.detail, "img.tar") {
		t.Fatalf("a tar read failure is not-checked, naming the tar, got %+v", got)
	}
}

func rolesYes(context.Context) (bool, error) { return true, nil }
func rolesNo(context.Context) (bool, error)  { return false, nil }

func TestCheckAgentListSize(t *testing.T) {
	sized := func(n int, err error) agentListSizer {
		return func(context.Context, string) (int, error) { return n, err }
	}
	ctx := context.Background()
	if r := checkAgentListSize(ctx, "lever", sized(200<<10, nil)); !r.ok || r.fix != "" || !strings.Contains(r.detail, "200 KiB of the 8 MiB") {
		t.Errorf("a small list passes: %+v", r)
	}
	if r := checkAgentListSize(ctx, "lever", sized(hubapi.AgentListReplyCap/2, nil)); r.fix != "" {
		t.Errorf("exactly half the cap does not warn: %+v", r)
	}
	if r := checkAgentListSize(ctx, "lever", sized(hubapi.AgentListReplyCap/2+1, nil)); !r.ok || r.fix == "" {
		t.Errorf("past half the cap warns without failing: %+v", r)
	}
	tooLarge := &hubapi.APIError{TooLarge: true, Msg: "GET /api/v1/agents: too large"}
	if r := checkAgentListSize(ctx, "lever", sized(0, tooLarge)); r.ok || r.fix == "" {
		t.Errorf("a page over the cap fails: %+v", r)
	}
	lookup := fmt.Errorf("looking up project lever: %v", &hubapi.APIError{TooLarge: true, Msg: "GET /api/v1/projects: too large"})
	if r := checkAgentListSize(ctx, "lever", sized(0, lookup)); !r.ok {
		t.Errorf("a project list over its cap is not blamed on the agent list: %+v", r)
	}
	if r := checkAgentListSize(ctx, "lever", sized(0, errors.New("down"))); !r.ok || !strings.Contains(r.detail, "not checked") {
		t.Errorf("an unreachable hub is not checked: %+v", r)
	}
}

func TestCheckAgentRolesAllStored(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.Agent, error) {
		return []hubapi.Agent{{Slug: "assistant", Role: "baseline"}}, nil
	}
	r := checkAgentRoles(context.Background(), "lever", rolesYes, list)
	if !r.ok {
		t.Fatalf("every record carries a role => pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "baseline") {
		t.Errorf("detail should say what the records hold, got %q", r.detail)
	}
}

// The live hazard: this scion reads an unset stored role as full.
func TestCheckAgentRolesFlagsUnrolledOnRolesAwareScion(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.Agent, error) {
		return []hubapi.Agent{{Slug: "assistant"}, {Slug: "scratch", Role: "baseline"}}, nil
	}
	r := checkAgentRoles(context.Background(), "lever", rolesYes, list)
	if r.ok {
		t.Fatalf("an unrolled record on a roles-aware scion must fail; got %+v", r)
	}
	if !strings.Contains(r.detail, "assistant") || strings.Contains(r.detail, "scratch") {
		t.Errorf("detail should name only the unrolled record, got %q", r.detail)
	}
	if !strings.Contains(r.fix, "delete") {
		t.Errorf("fix should state the only route, got %q", r.fix)
	}
}

// On a scion without roles the records are harmless, so the row must not fail,
// but it warns that a bump will promote them.
func TestCheckAgentRolesWarnsAheadOfABumpWithoutFailing(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.Agent, error) {
		return []hubapi.Agent{{Slug: "assistant"}}, nil
	}
	r := checkAgentRoles(context.Background(), "lever", rolesNo, list)
	if !r.ok {
		t.Fatalf("unrolled records on a pre-roles scion are normal; got %+v", r)
	}
	if !strings.Contains(r.detail, "assistant") {
		t.Errorf("detail should still name the record, got %q", r.detail)
	}
	if !strings.Contains(r.detail, "scion#1089") {
		t.Errorf("detail should say what a bump would do, got %q", r.detail)
	}
}

func TestCheckAgentRolesUnreachableHubIsNotAFinding(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.Agent, error) {
		return nil, errors.New("jail is down")
	}
	r := checkAgentRoles(context.Background(), "lever", rolesYes, list)
	if !r.ok {
		t.Fatalf("a down instance already shows in the broker check; got %+v", r)
	}
}

func TestCheckAgentRolesHubAnsweredIsAFinding(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.Agent, error) {
		return nil, &hubapi.APIError{Status: 403, Msg: "forbidden"}
	}
	r := checkAgentRoles(context.Background(), "lever", rolesYes, list)
	if r.ok {
		t.Fatalf("a hub that answered 403 must fail the check; got %+v", r)
	}
}

func TestCheckAgentRolesProbeFailureIsNotChecked(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.Agent, error) {
		return []hubapi.Agent{{Slug: "assistant"}}, nil
	}
	probeErr := func(context.Context) (bool, error) { return false, errors.New("scion not installed") }
	r := checkAgentRoles(context.Background(), "lever", probeErr, list)
	if !r.ok {
		t.Fatalf("an unanswerable probe is not a finding here; got %+v", r)
	}
	if !strings.Contains(r.detail, "not checked") {
		t.Errorf("detail should say it could not check, got %q", r.detail)
	}
}

func TestCheckNodeToolchainNotRequired(t *testing.T) {
	for _, c := range []struct {
		name string
		app  *config.App
	}{
		{"remote off", &config.App{Scion: config.ScionConfig{Version: "e82a2a08"}}},
		// No scion source to build the SPA from; the operator's binary may embed it.
		{"binary mode", &config.App{
			Scion:  config.ScionConfig{Binary: "/host/scion"},
			Remote: config.Remote{Enabled: true},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := checkNodeToolchain(c.app, doctorProbes{})
			if !r.ok {
				t.Fatalf("no web UI to build => pass; got %+v", r)
			}
			if !strings.Contains(r.detail, "not required") {
				t.Fatalf("detail should say a build isn't required: %q", r.detail)
			}
		})
	}
}

func TestCheckNodeToolchainProbeOK(t *testing.T) {
	p := doctorProbes{nodeToolchain: func() (string, error) { return "v25.9.0", nil }}

	r := checkNodeToolchain(&config.App{
		Scion:  config.ScionConfig{Version: "e82a2a08"},
		Remote: config.Remote{Enabled: true},
	}, p)
	if !r.ok {
		t.Fatalf("a working node on PATH must pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "v25.9.0") {
		t.Fatalf("detail should report the node version: %q", r.detail)
	}
}

func TestCheckNodeToolchainProbeError(t *testing.T) {
	p := doctorProbes{nodeToolchain: func() (string, error) {
		return "", fmt.Errorf("%w: node --version: exit status 126", webassets.ErrNodeToolchain)
	}}

	r := checkNodeToolchain(&config.App{
		Scion:  config.ScionConfig{Source: "/home/devuser/scion"},
		Remote: config.Remote{Enabled: true},
	}, p)
	if r.ok {
		t.Fatal("a broken node (e.g. a dead asdf shim) must fail the check")
	}
	if !strings.Contains(r.detail, "126") {
		t.Fatalf("detail should name the underlying error: %q", r.detail)
	}
	if !strings.Contains(r.fix, "PATH") {
		t.Fatalf("fix should point at PATH: %q", r.fix)
	}
}

func TestStateRel(t *testing.T) {
	st := state.ForConfig(filepath.Join(t.TempDir(), "inst"))
	if got := stateRel(st, st.RemoteLog()); got != ".lever-state/remote.log" {
		t.Fatalf("stateRel = %q", got)
	}
	if got := stateRel(st, st.Log()); got != ".lever-state/broker.log" {
		t.Fatalf("stateRel = %q", got)
	}
	if got := stateDirName(); got != ".lever-state" {
		t.Fatalf("stateDirName = %q", got)
	}
}

func TestCheckListeningProcess(t *testing.T) {
	status := func(pid int, found, alive bool) func() (int, bool, bool) {
		return func() (int, bool, bool) { return pid, found, alive }
	}
	for _, tc := range []struct {
		name   string
		status func() (int, bool, bool)
		dial   dialFunc
		ok     bool
		detail string
		fix    string
	}{
		{"never started", status(0, false, false), okDial, false, "no x.pid — the thing was never started", "start it"},
		{"stale pid", status(42, true, false), okDial, false, "x.pid names pid 42, but that process is gone", "start it"},
		{"not listening", status(42, true, true), failDial, false, "pid 42 is alive but nothing is listening on 127.0.0.1:1", "inspect .lever-state/x.log, then restart"},
		{"healthy", status(42, true, true), okDial, true, "pid 42, serving on 127.0.0.1:1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkListeningProcess("n", "x.pid", "the thing", ".lever-state/x.log", "start it", tc.status, "127.0.0.1:1", tc.dial)
			if r.ok != tc.ok || !strings.Contains(r.detail, tc.detail) || !strings.Contains(r.fix, tc.fix) {
				t.Fatalf("got %+v, want ok=%v detail~%q fix~%q", r, tc.ok, tc.detail, tc.fix)
			}
		})
	}
}

func TestImageLabelProbe(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("docker image inspect --format {{index .Config.Labels \"claude_code_version\"}} labelled", proc.Result{Stdout: "2.1.207\n"})
	r.Script("docker image inspect --format {{index .Config.Labels \"claude_code_version\"}} bare", proc.Result{Stdout: "<no value>\n"})
	r.Script("docker image inspect --format {{index .Config.Labels \"lever_version\"}} labelled", proc.Result{Stdout: "0.28.1 (21ff051f651f)\n"})
	if v, err := imageLabelProbe(r, "labelled", claudeVersionLabel); err != nil || v != "2.1.207" {
		t.Fatalf("labelled: %q, %v", v, err)
	}
	if v, err := imageLabelProbe(r, "labelled", leverVersionLabel); err != nil || v != "0.28.1 (21ff051f651f)" {
		t.Fatalf("lever label: %q, %v", v, err)
	}
	if v, err := imageLabelProbe(r, "bare", claudeVersionLabel); err != nil || v != "" {
		t.Fatalf("bare: %q, %v", v, err)
	}
	if _, err := imageLabelProbe(r, "missing", claudeVersionLabel); err == nil {
		t.Fatal("an inspect failure must be an error")
	}
}

// A nil probe would panic the first time a real check reached it.
func TestProductionProbesWiresEveryField(t *testing.T) {
	p := productionProbes(proc.NewFakeRunner())
	if p.dial == nil || p.goVersion == nil || p.nodeToolchain == nil || p.claudeVersion == nil ||
		p.claudeVersionTar == nil || p.leverVersion == nil || p.leverVersionTar == nil ||
		p.jailABI == nil || p.jailABITar == nil ||
		p.remoteHealthz == nil || p.portHolder == nil {
		t.Fatalf("productionProbes left a probe nil: %+v", p)
	}
}

func TestCheckManagerLive(t *testing.T) {
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	cases := []struct {
		label      string
		list       agentLister
		ok         bool
		wantDetail string
		wantFix    string
	}{
		{"running", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days"}), true, "Up 4 days", ""},
		// Crashed and offline fail even with the container up. Stalled passes: the
		// stall sweeper marks an idle manager stalled too.
		{"running, completed", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "completed", LastActivityEvent: now.Add(-3 * time.Minute)}), true, "activity completed, 3m0s ago", ""},
		{"running, working long", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "working", LastActivityEvent: now.Add(-40 * time.Minute)}), true, "activity working, 40m0s ago", ""},
		{"running, no event time", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "waiting_for_input"}), true, "activity waiting_for_input)", ""},
		{"running, stalled", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "stalled", LastActivityEvent: now.Add(-6 * time.Minute)}), true, "idle at its prompt, or a turn that never finished", ""},
		{"running, stalled for days", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "stalled", LastActivityEvent: now.Add(-47 * time.Hour)}), true, "activity stalled, 47h0m0s ago", ""},
		{"running, offline", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "offline"}), false, "offline", "lever attach"},
		{"running, crashed", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "crashed"}), false, "crashed", "lever attach"},
		{"absent", listing(scion.Agent{Slug: "scratch", Phase: "running", ContainerStatus: "Up 1 second"}), false, "no record", "lever up"},
		{"crashed", listing(scion.Agent{Slug: "assistant", Phase: "error", ContainerStatus: "Exited (1) 3 seconds ago"}), false, `phase "error"`, "--force"},
		{"suspended", listing(scion.Agent{Slug: "assistant", Phase: "suspended", ContainerStatus: "stopped"}), false, `phase "suspended"`, "resume"},
		{"running record, dead container", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Exited (1) 2 seconds ago"}), false, "Exited (1)", "lever up"},
		// A blank column is "cannot tell", as `lever up` reads it.
		{"running record, blank column", listing(scion.Agent{Slug: "assistant", Phase: "running"}), true, "no container status", ""},
	}
	for _, c := range cases {
		r := checkManagerLive(context.Background(), "/lever", "assistant", c.list, nil, nil, now)
		if r.ok != c.ok {
			t.Fatalf("%s: ok=%v, want %v (%+v)", c.label, r.ok, c.ok, r)
		}
		if !strings.Contains(r.detail, c.wantDetail) {
			t.Errorf("%s: detail %q should mention %q", c.label, r.detail, c.wantDetail)
		}
		if c.wantFix != "" && !strings.Contains(r.fix, c.wantFix) {
			t.Errorf("%s: fix %q should mention %q", c.label, r.fix, c.wantFix)
		}
	}
	// A listing failure is "not checked", never a pass and never a finding.
	r := checkManagerLive(context.Background(), "/lever", "assistant", func(context.Context, string) ([]scion.Agent, error) {
		return nil, errors.New("connection refused\nusage: scion list ...")
	}, nil, nil, now)
	if !r.ok || !strings.Contains(r.detail, "not checked") || strings.Contains(r.detail, "usage") {
		t.Fatalf("list error must read as not-checked with only the first line: %+v", r)
	}
}

type stubSession struct {
	tok      map[string]jail.HubTokenTimes
	tokErr   error
	alive    bool
	aliveErr error
	refs     []string
}

func (s *stubSession) HubToken(_ context.Context, ref string) (jail.HubTokenTimes, error) {
	s.refs = append(s.refs, ref)
	if s.tokErr != nil {
		return jail.HubTokenTimes{}, s.tokErr
	}
	t, ok := s.tok[ref]
	if !ok {
		return jail.HubTokenTimes{}, errors.New("exit status 1")
	}
	return t, nil
}

func (s *stubSession) HarnessAlive(_ context.Context, ref string) (bool, error) {
	s.refs = append(s.refs, ref)
	return s.alive, s.aliveErr
}

// TestCheckManagerLiveSessionFaults: an expired hub token fails even while
// activity reads stalled, and a phase stopped over a live container says
// whether claude still runs.
func TestCheckManagerLiveSessionFaults(t *testing.T) {
	now := time.Date(2026, 10, 6, 19, 30, 0, 0, time.UTC)
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	stalled := scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "stalled", LastActivityEvent: now.Add(-6 * time.Minute)}
	expired := map[string]jail.HubTokenTimes{"lever--assistant": {Expiry: now.Add(-20 * time.Minute), Now: now}}
	valid := map[string]jail.HubTokenTimes{"lever--assistant": {Expiry: now.Add(5 * time.Hour), Now: now}}

	r := checkManagerLive(context.Background(), "/lever", "assistant", listing(stalled), &stubSession{tok: expired}, nil, now)
	if r.ok || !strings.Contains(r.detail, "hub token expired at 2026-10-06T19:10:00Z (20m0s ago") || !strings.Contains(r.detail, "401") ||
		!strings.Contains(r.fix, "lever apply") || !strings.Contains(r.fix, "scion reset-auth assistant -g /lever") {
		t.Fatalf("expired token under a stalled activity must fail with the reset-auth fix: %+v", r)
	}
	r = checkManagerLive(context.Background(), "/lever", "assistant", listing(stalled), &stubSession{tok: valid}, nil, now)
	if !r.ok || !strings.Contains(r.detail, "idle at its prompt") {
		t.Fatalf("a valid token keeps the idle-manager pass: %+v", r)
	}
	// An unreadable token is no finding.
	r = checkManagerLive(context.Background(), "/lever", "assistant", listing(stalled), &stubSession{tokErr: errors.New("exit status 1")}, nil, now)
	if !r.ok {
		t.Fatalf("an unreadable token must not fail the row: %+v", r)
	}

	stoppedLive := scion.Agent{Slug: "assistant", Phase: "stopped", ContainerStatus: "Up 3 hours"}
	r = checkManagerLive(context.Background(), "/lever", "assistant", listing(stoppedLive), &stubSession{alive: true}, nil, now)
	if r.ok || !strings.Contains(r.detail, "claude still runs in its container") || !strings.Contains(r.detail, "SessionEnd") ||
		!strings.Contains(r.fix, "lever apply") {
		t.Fatalf("stopped over a live harness: %+v", r)
	}
	r = checkManagerLive(context.Background(), "/lever", "assistant", listing(stoppedLive), &stubSession{alive: false}, nil, now)
	if r.ok || !strings.Contains(r.detail, "claude has exited") || !strings.Contains(r.fix, "/resume") {
		t.Fatalf("stopped with claude gone: %+v", r)
	}
	r = checkManagerLive(context.Background(), "/lever", "assistant", listing(stoppedLive), &stubSession{aliveErr: errors.New("exit status 125")}, nil, now)
	if r.ok || !strings.Contains(r.detail, `phase "stopped"`) || !strings.Contains(r.fix, "new claude session") {
		t.Fatalf("stopped, harness unknown: %+v", r)
	}
	s := &stubSession{alive: true}
	r = checkManagerLive(context.Background(), "/lever", "assistant", listing(scion.Agent{Slug: "assistant", Phase: "stopped", ContainerStatus: "stopped"}), s, nil, now)
	if r.ok || len(s.refs) != 0 || !strings.Contains(r.fix, "/resume") {
		t.Fatalf("stopped, no container: %+v (probed %v)", r, s.refs)
	}
}

// TestCheckManagerLiveNoConversation: an error-phase manager whose log says
// claude found no conversation is named as such; the log is never printed and
// only an error-phase record's log is read.
func TestCheckManagerLiveNoConversation(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	crashed := scion.Agent{Slug: "assistant", Phase: "error", ContainerStatus: "Exited (1) 3 seconds ago"}
	var refs []string
	tail := func(log string, err error) containerLogReader {
		return func(_ context.Context, ref string) (string, error) {
			refs = append(refs, ref)
			return log, err
		}
	}
	const log = "SECRET-LINE\n\x1b[31mNo conversation found to continue\x1b[0m\n"
	r := checkManagerLive(context.Background(), "/lever", "assistant", listing(crashed), nil, tail(log, nil), now)
	if r.ok || !strings.Contains(r.detail, "no conversation to continue") || r.fix != noConversationFix {
		t.Fatalf("no conversation not named: %+v", r)
	}
	if strings.Contains(r.detail+r.fix, "SECRET") || len(refs) != 1 || refs[0] != "lever--assistant" {
		t.Fatalf("log repeated or read wrongly: %+v (refs %v)", r, refs)
	}
	for _, w := range []string{"lever up", "--fresh", "backs up", "holds no transcript"} {
		if !strings.Contains(noConversationFix, w) {
			t.Errorf("fix lacks %q", w)
		}
	}
	for _, tr := range []containerLogReader{tail("Error: other", nil), tail("", errors.New("no container")), nil} {
		r = checkManagerLive(context.Background(), "/lever", "assistant", listing(crashed), nil, tr, now)
		if r.ok || !strings.Contains(r.fix, "--force") {
			t.Fatalf("plain error row changed: %+v", r)
		}
	}
	refs = nil
	for _, a := range []scion.Agent{
		{Slug: "assistant", Phase: "suspended", ContainerStatus: "stopped"},
		{Slug: "assistant", Phase: "stopped", ContainerStatus: "stopped"},
		{Slug: "assistant", Phase: "running", ContainerStatus: "Exited (1) 2 seconds ago"},
	} {
		r = checkManagerLive(context.Background(), "/lever", "assistant", listing(a), nil, tail(log, nil), now)
		if r.fix == noConversationFix {
			t.Errorf("phase %s got the no-conversation fix", a.Phase)
		}
	}
	if len(refs) != 0 {
		t.Fatalf("read the log of %v", refs)
	}
}

func TestCheckAgentHubTokens(t *testing.T) {
	now := time.Date(2026, 10, 6, 19, 30, 0, 0, time.UTC)
	fleet := func(context.Context, string) ([]scion.Agent, error) {
		return []scion.Agent{
			{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days"},
			{Slug: "scratch", Phase: "running", ContainerStatus: "Up 1 hour"},
			{Slug: "idle", Phase: "suspended", ContainerStatus: "stopped"},
		}, nil
	}
	agents := []string{"assistant", "scratch", "idle"}
	tok := func(left time.Duration) jail.HubTokenTimes {
		return jail.HubTokenTimes{Expiry: now.Add(left), Now: now}
	}

	r := checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, fleet, &stubSession{tok: map[string]jail.HubTokenTimes{
		"lever--assistant": tok(7 * time.Hour), "lever--scratch": tok(9 * time.Hour)}})
	if !r.ok || r.fix != "" || !strings.Contains(r.detail, "assistant (valid 7h0m0s)") || strings.Contains(r.detail, "idle") {
		t.Fatalf("all valid: %+v", r)
	}
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, fleet, &stubSession{tok: map[string]jail.HubTokenTimes{
		"lever--assistant": tok(7 * time.Hour), "lever--scratch": tok(-3 * time.Minute)}})
	if r.ok || !strings.Contains(r.detail, "scratch (expired 3m0s ago)") || !strings.Contains(r.fix, "scion reset-auth scratch -g /lever") {
		t.Fatalf("worker expired: %+v", r)
	}
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, fleet, &stubSession{tok: map[string]jail.HubTokenTimes{
		"lever--assistant": tok(90 * time.Minute), "lever--scratch": tok(9 * time.Hour)}})
	if !r.ok || r.fix == "" || !strings.Contains(r.detail, "assistant (refresh due 30m0s ago, expires in 1h30m0s)") {
		t.Fatalf("overdue refresh is a warning: %+v", r)
	}
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, fleet, &stubSession{tok: map[string]jail.HubTokenTimes{
		"lever--assistant": tok(7 * time.Hour)}})
	if !r.ok || !strings.Contains(r.detail, "not checked (token unreadable): scratch (error)") {
		t.Fatalf("unreadable worker token: %+v", r)
	}
	// The running manager's expired token is the manager row's failure; here it
	// only warns.
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, fleet, &stubSession{tok: map[string]jail.HubTokenTimes{
		"lever--assistant": tok(-10 * time.Minute), "lever--scratch": tok(9 * time.Hour)}})
	if !r.ok || r.fix == "" || !strings.Contains(r.detail, "the manager agent row reports it") {
		t.Fatalf("manager expired: %+v", r)
	}
	// A phase apply does not heal warns rather than fails: its fix would not work.
	errFleet := func(context.Context, string) ([]scion.Agent, error) {
		return []scion.Agent{
			{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days"},
			{Slug: "scratch", Phase: "error", ContainerStatus: "Up 1 hour"},
		}, nil
	}
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, errFleet, &stubSession{tok: map[string]jail.HubTokenTimes{
		"lever--assistant": tok(7 * time.Hour), "lever--scratch": tok(-time.Hour)}})
	if !r.ok || !strings.Contains(r.detail, `scratch (expired, phase "error")`) || !strings.Contains(r.fix, "resume") {
		t.Fatalf("expired under phase error: %+v", r)
	}
	stoppedFleet := func(context.Context, string) ([]scion.Agent, error) {
		return []scion.Agent{{Slug: "scratch", Phase: "stopped", ContainerStatus: "Up 1 hour"}}, nil
	}
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, stoppedFleet, &stubSession{tok: map[string]jail.HubTokenTimes{
		"lever--scratch": tok(-time.Hour)}})
	if r.ok || !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("expired under phase stopped: %+v", r)
	}
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, func(context.Context, string) ([]scion.Agent, error) {
		return nil, errors.New("hub down\nusage")
	}, &stubSession{})
	if !r.ok || !strings.Contains(r.detail, "not checked") || strings.Contains(r.detail, "usage") {
		t.Fatalf("list error: %+v", r)
	}
	r = checkAgentHubTokens(context.Background(), "/lever", "assistant", agents, func(context.Context, string) ([]scion.Agent, error) { return nil, nil }, &stubSession{})
	if !r.ok || r.detail != "no running agent" {
		t.Fatalf("nothing running: %+v", r)
	}
}

// TestCheckManagerImage: a resume never moves the record's image, so a changed
// manager.image looks healthy on the version row alone.
func TestCheckManagerImage(t *testing.T) {
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	cases := []struct {
		label      string
		list       agentLister
		ok         bool
		wantDetail string
		wantFix    string
	}{
		{"same", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "lever.local/lever-claude:arm64"}), true, "lever.local/lever-claude:arm64", ""},
		{"created by an older lever on the bare name", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "scionlocal/lever-claude:arm64"}), true, "assistant (scionlocal/lever-claude:arm64) created on an unqualified image name", "lever up --fresh --keep-conversation"},
		{"same, podman-qualified bare", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "localhost/scionlocal/lever-claude:arm64"}), true, "unqualified", "--keep-conversation"},
		{"a worker on a bare name", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "lever.local/lever-claude:arm64"}, scion.Agent{Slug: "w", Image: "scionlocal/lever-claude:arm64"}), true, "w (scionlocal/lever-claude:arm64)", "recycling"},
		{"a worker on a digest", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "lever.local/lever-claude:arm64"}, scion.Agent{Slug: "w", Image: "x@sha256:aa"}), true, "as configured", ""},
		{"drifted", listing(scion.Agent{Slug: "assistant", Phase: "suspended", Image: "scionlocal/lever-claude:v1"}), false, "scionlocal/lever-claude:v1", "lever up --fresh"},
		{"no manager record, a worker on a bare name", listing(scion.Agent{Slug: "w", Image: "scionlocal/lever-claude:arm64"}), true, "w (scionlocal/lever-claude:arm64) created on an unqualified", "recycling"},
		{"drifted manager, a worker on a bare name", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "lever.local/other:arm64"}, scion.Agent{Slug: "w", Image: "scionlocal/lever-claude:arm64"}), false, "w (scionlocal/lever-claude:arm64)", "lever up --fresh"},
		{"no record", listing(scion.Agent{Slug: "scratch", Phase: "running", Image: "lever.local/x:1"}), true, "not checked", ""},
		{"record without image", listing(scion.Agent{Slug: "assistant", Phase: "running"}), true, "not checked", ""},
		{"list fails", func(context.Context, string) ([]scion.Agent, error) { return nil, fmt.Errorf("hub down") }, true, "not checked", ""},
	}
	for _, c := range cases {
		r := checkManagerImage(context.Background(), "/lever", "assistant", "scionlocal/lever-claude:arm64", c.list)
		if c.label == "same" && r.fix != "" {
			t.Fatalf("same: a fix %q on a healthy row", r.fix)
		}
		if r.ok != c.ok {
			t.Fatalf("%s: ok=%v, want %v (%+v)", c.label, r.ok, c.ok, r)
		}
		if !strings.Contains(r.detail, c.wantDetail) {
			t.Errorf("%s: detail %q should mention %q", c.label, r.detail, c.wantDetail)
		}
		if !strings.Contains(r.fix, c.wantFix) {
			t.Errorf("%s: fix %q should mention %q", c.label, r.fix, c.wantFix)
		}
	}
}

// The advice must name the verb that recreates the manager: `lever stop &&
// lever up` resumes on the old image.
func TestCheckClaudeVersionAdvisesFresh(t *testing.T) {
	p := doctorProbes{claudeVersion: func(string) (string, error) { return "2.1.240", nil }}
	got := checkClaudeVersion("img", "", p)
	if !strings.Contains(got.detail, "lever up --fresh") || strings.Contains(got.detail, "stop && lever up") {
		t.Fatalf("detail = %q", got.detail)
	}
}

// Doctor is read-only: a missing tree fails pointing at `lever init` and is
// never created.
func TestCheckOperatorSkillsMissingTreeFailsWithoutCreating(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	app := &config.App{Tree: tree, Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}}}
	res := checkOperatorSkills(app, state.ForConfig(root))
	if res.ok {
		t.Fatalf("missing tree must fail: %+v", res)
	}
	if !strings.Contains(res.fix, "lever init") {
		t.Fatalf("fix must point at lever init: %+v", res)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("doctor must not create the tree (err=%v)", err)
	}
}

func TestCheckWorkerTicketMounts(t *testing.T) {
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	mountsOf := func(m map[string][]string, broken ...string) mountLister {
		return func(_ context.Context, ref string) ([]string, error) {
			if slices.Contains(broken, ref) {
				return nil, fmt.Errorf("podman exploded")
			}
			targets, ok := m[ref]
			if !ok {
				return nil, fmt.Errorf("inspect %s: %w", ref, jail.ErrNoContainer)
			}
			return targets, nil
		}
	}
	both := listing(
		scion.Agent{Slug: "a", Phase: "running", ContainerID: "ca"},
		scion.Agent{Slug: "b", Phase: "suspended", ContainerID: "cb"},
		scion.Agent{Slug: "assistant", Phase: "running", ContainerID: "cm"})
	cases := []struct {
		label      string
		workers    []string
		list       agentLister
		mounts     mountLister
		ok         bool
		wantDetail string
		wantFix    string
	}{
		{"all mounted", []string{"a", "b"}, both,
			mountsOf(map[string][]string{"ca": {"/workspace", "/run/lever"}, "cb": {"/run/lever"}}), true, "2 worker container(s) mount /run/lever", ""},
		{"one pre-channel", []string{"a", "b"}, both,
			mountsOf(map[string][]string{"ca": {"/workspace", "/run/lever"}, "cb": {"/workspace"}}), false, "worker b was created before", "lever worker purge b --force"},
		{"both pre-channel", []string{"a", "b"}, both,
			mountsOf(map[string][]string{"ca": {"/workspace"}, "cb": {"/workspace"}}), false, "worker {a,b}", "purge {a,b}"},
		{"manager mount never inspected", []string{"a"}, both,
			mountsOf(map[string][]string{"ca": {"/run/lever"}}), true, "1 worker", ""},
		{"no record", []string{"ghost"}, both, mountsOf(nil), true, "no worker container", ""},
		// No id in the listing: the container is found by scion's name.
		{"no container id, found by name", []string{"a"}, listing(scion.Agent{Slug: "a", Phase: "running"}),
			mountsOf(map[string][]string{"lever--a": {"/workspace"}}), false, "worker a was created before", "purge a"},
		{"no container at all", []string{"a"}, listing(scion.Agent{Slug: "a", Phase: "stopped"}), mountsOf(nil), true, "no worker container", ""},
		{"inspect fails", []string{"a"}, both, mountsOf(map[string][]string{}, "ca"), true, "not checked for worker a", ""},
		{"list fails", []string{"a"}, func(context.Context, string) ([]scion.Agent, error) { return nil, fmt.Errorf("hub down") }, mountsOf(nil), true, "not checked", ""},
		{"no workers", nil, both, mountsOf(nil), true, "no workers declared", ""},
		{"nil probes", []string{"a"}, nil, nil, true, "not checked", ""},
	}
	for _, c := range cases {
		r := checkWorkerTicketMounts(context.Background(), "/lever", c.workers, c.list, c.mounts)
		if r.name != "worker ticket mounts" {
			t.Fatalf("%s: name = %q", c.label, r.name)
		}
		if r.ok != c.ok {
			t.Fatalf("%s: ok=%v, want %v (%+v)", c.label, r.ok, c.ok, r)
		}
		if !strings.Contains(r.detail, c.wantDetail) {
			t.Errorf("%s: detail %q should mention %q", c.label, r.detail, c.wantDetail)
		}
		if !strings.Contains(r.fix, c.wantFix) {
			t.Errorf("%s: fix %q should mention %q", c.label, r.fix, c.wantFix)
		}
	}
}

func TestCheckManagerReadOnly(t *testing.T) {
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	inspectOf := func(m map[string][]jail.Mount, broken ...string) mountInspector {
		return func(_ context.Context, ref string) ([]jail.Mount, error) {
			if slices.Contains(broken, ref) {
				return nil, fmt.Errorf("podman exploded")
			}
			mounts, ok := m[ref]
			if !ok {
				return nil, fmt.Errorf("inspect %s: %w", ref, jail.ErrNoContainer)
			}
			return mounts, nil
		}
	}
	// probeOf: targets in replaced cover another directory than the host's.
	probeOf := func(err error, replaced ...string) liveMountChecker {
		return func(_ context.Context, _ string, want []jail.LiveMount) ([]jail.LiveMountProblem, error) {
			if err != nil {
				return nil, err
			}
			var out []jail.LiveMountProblem
			for _, w := range want {
				if slices.Contains(replaced, w.Target) {
					out = append(out, jail.LiveMountProblem{Target: w.Target, Reason: jail.LiveMountReplaced})
				}
			}
			return out, nil
		}
	}
	refused := probeOf(nil)
	want := []config.TreeMount{{Rel: "assistant"}, {Rel: "workers", WorkerPin: true}, {Rel: "assistant/tools", ReadOnly: true}, {Rel: "workers/w", WorkerPin: true}}
	mgr := listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 3 hours", ContainerID: "cm"})
	ws := jail.Mount{Source: "/lever", Destination: "/workspace", RW: true}
	pin := jail.Mount{Source: "/lever/assistant", Destination: "/workspace/assistant", RW: true}
	ro := jail.Mount{Source: "/lever/assistant/tools", Destination: "/workspace/assistant/tools", RW: false}
	wpins := []jail.Mount{
		{Source: "/lever/workers", Destination: "/workspace/workers", RW: true},
		{Source: "/lever/workers/w", Destination: "/workspace/workers/w", RW: true},
	}
	full := append([]jail.Mount{ws, pin, ro}, wpins...)
	with := func(ms ...jail.Mount) []jail.Mount { return append(append([]jail.Mount{}, ms...), wpins...) }
	// kb is an entry the config dropped, its dir kept; /lever/gone has no dir.
	tree := t.TempDir()
	for _, d := range []string{"assistant/tools", "workers/w", "kb"} {
		if err := os.MkdirAll(filepath.Join(tree, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kb := jail.Mount{Source: "/lever/kb", Destination: "/workspace/kb"}
	gone := jail.Mount{Source: "/lever/gone", Destination: "/workspace/gone"}
	cases := []struct {
		label      string
		want       []config.TreeMount
		list       agentLister
		inspect    mountInspector
		probe      liveMountChecker
		ok         bool
		warn       bool
		wantDetail string
		wantFix    string
	}{
		{"all mounted, probe refused", want, mgr, inspectOf(map[string][]jail.Mount{"cm": full}), refused, true, false, "1 path(s) read-only in \"assistant\", 3 pin(s) mounted (checked live from the guest", ""},
		{"created before the setting", want, mgr, inspectOf(map[string][]jail.Mount{"cm": {ws}}), refused, false, false,
			"assistant/tools not mounted read-only: the agent can write it", "lever up --fresh"},
		{"entry pin missing", want, mgr, inspectOf(map[string][]jail.Mount{"cm": with(ws, ro)}), refused, false, false, "assistant not pinned: the agent can rename it away", "--keep-conversation"},
		{"worker pin missing", want, mgr, inspectOf(map[string][]jail.Mount{"cm": {ws, pin, ro, wpins[0]}}), refused, false, false,
			"workers/w (a worker dir) not pinned: the manager can replace it with a symbolic link and have that worker mount a protected directory", "lever up --fresh"},
		{"entry mounted rw", want, mgr, inspectOf(map[string][]jail.Mount{"cm": with(ws, pin, jail.Mount{Source: "/lever/assistant/tools", Destination: "/workspace/assistant/tools", RW: true})}), refused, false, false,
			"assistant/tools mounted read-write", "lever up --fresh"},
		{"entry from another source", want, mgr, inspectOf(map[string][]jail.Mount{"cm": with(ws, pin, jail.Mount{Source: "/elsewhere", Destination: "/workspace/assistant/tools"})}), refused, false, false,
			"assistant/tools mounted from another source", "lever up --fresh"},
		// The false OK the inspect alone gave: the mount is listed, but the host
		// replaced the directory and writes land in the new one.
		{"entry replaced on the host", want, mgr, inspectOf(map[string][]jail.Mount{"cm": full}), probeOf(nil, "/workspace/assistant/tools"), false, false,
			"assistant/tools covers another directory than the host has there now", "edit protected directories in place"},
		{"pin replaced on the host", want, mgr, inspectOf(map[string][]jail.Mount{"cm": full}), probeOf(nil, "/workspace/assistant"), false, false,
			"assistant covers another directory than the host has there now", "edit protected directories in place"},
		{"probe cannot run", want, mgr, inspectOf(map[string][]jail.Mount{"cm": full}), probeOf(errors.New("cannot read the running container's mounts from the guest")), true, true, "live check could not run", "re-run doctor"},
		// The first process sits in a namespace made inside the container: a staged
		// view.
		{"foreign namespace", want, mgr, inspectOf(map[string][]jail.Mount{"cm": full}), probeOf(jail.ErrForeignNamespace), false, false, "mount namespace made inside the container", "compromised"},
		{"probe not wired", want, mgr, inspectOf(map[string][]jail.Mount{"cm": full}), nil, true, true, "live check is not wired", "lever wiring gap"},
		{"container not running", want, listing(scion.Agent{Slug: "assistant", Phase: "suspended", ContainerStatus: "Exited (0) 2 minutes ago", ContainerID: "cm"}),
			inspectOf(map[string][]jail.Mount{"cm": full}), refused, true, true, "live check did not run (the manager container is not running)", "lever up"},
		// No id in the listing: the container is found by scion's name.
		{"no container id, found by name", want, listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "running"}),
			inspectOf(map[string][]jail.Mount{"lever--assistant": full}), refused, true, false, "checked live from the guest", ""},
		{"none configured", nil, mgr, inspectOf(nil), refused, true, false, "none configured", ""},
		{"none configured, nothing mounted", nil, mgr, inspectOf(map[string][]jail.Mount{"cm": {ws}}), refused, true, false, "none configured", ""},
		{"none configured, list fails", nil, func(context.Context, string) ([]scion.Agent, error) { return nil, fmt.Errorf("hub down") }, inspectOf(nil), refused, true, false, "none configured", ""},
		// The record keeps the mounts of its create.
		{"dropped entry still mounted", nil, mgr, inspectOf(map[string][]jail.Mount{"cm": {ws, kb}}), refused, true, false,
			`"kb" still mounted read-only although no longer in manager.read_only`, "keep these directories"},
		{"mounted dir gone", nil, mgr, inspectOf(map[string][]jail.Mount{"cm": {ws, gone}}), refused, false, false,
			`"gone" mounted by the manager record but no longer on the host`, "recreate each missing directory"},
		{"plan held, dropped entry too", want, mgr, inspectOf(map[string][]jail.Mount{"cm": append(append([]jail.Mount{}, full...), kb)}), refused, true, false,
			`"kb" still mounted read-only`, "lever up --fresh"},
		{"gap and gone dir", want, mgr, inspectOf(map[string][]jail.Mount{"cm": {ws, gone}}), refused, false, false,
			`; also "gone" mounted by the manager record`, "recreate each missing directory"},
		{"no record", want, listing(), inspectOf(nil), refused, true, false, "not checked (no manager record)", ""},
		{"no container", want, mgr, inspectOf(nil), refused, true, false, "not checked (no manager container)", ""},
		{"inspect fails", want, mgr, inspectOf(map[string][]jail.Mount{}, "cm"), refused, true, false, "not checked (could not inspect", ""},
		{"list fails", want, func(context.Context, string) ([]scion.Agent, error) { return nil, fmt.Errorf("hub down") }, inspectOf(nil), refused, true, false, "not checked", ""},
		{"nil probes", want, nil, nil, nil, true, false, "not checked", ""},
	}
	for _, c := range cases {
		r := checkManagerReadOnly(context.Background(), "/lever", tree, "assistant", c.want, c.list, c.inspect, nil, c.probe)
		if r.name != "manager read-only paths" {
			t.Fatalf("%s: name = %q", c.label, r.name)
		}
		if r.ok != c.ok {
			t.Fatalf("%s: ok=%v, want %v (%+v)", c.label, r.ok, c.ok, r)
		}
		if c.warn && (r.fix == "" || !strings.Contains(r.detail, "not") || strings.Contains(r.detail, "refused,")) {
			t.Errorf("%s: a probe that did not run must not read as confirmed: %q", c.label, r.detail)
		}
		if !strings.Contains(r.detail, c.wantDetail) {
			t.Errorf("%s: detail %q should mention %q", c.label, r.detail, c.wantDetail)
		}
		if !strings.Contains(r.fix, c.wantFix) {
			t.Errorf("%s: fix %q should mention %q", c.label, r.fix, c.wantFix)
		}
	}
}

// With no manager container, the row reads the hub record's volumes, which a
// resume would recreate.
func TestCheckManagerReadOnlyFallsBackToTheHubRecord(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := func(context.Context, string) ([]scion.Agent, error) {
		return []scion.Agent{{Slug: "assistant", Phase: "suspended"}}, nil
	}
	noContainer := func(context.Context, string) ([]jail.Mount, error) { return nil, jail.ErrNoContainer }
	refused := func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) { return nil, nil }
	recordOf := func(ms ...jail.Mount) recordVolumeReader {
		return func(_ context.Context, project, agent string) ([]jail.Mount, error) {
			if project != "lever" || agent != "assistant" {
				t.Errorf("record read for %q/%q", project, agent)
			}
			return ms, nil
		}
	}
	noRecord := func(context.Context, string, string) ([]jail.Mount, error) { return nil, errors.New("no hub record") }
	for _, c := range []struct {
		label      string
		record     recordVolumeReader
		ok         bool
		wantDetail string
		wantFix    string
	}{
		{"gone dir in the record", recordOf(jail.Mount{Source: "/lever/gone", Destination: "/workspace/gone"}), false,
			`"gone" mounted by the manager record but no longer on the host`, "recreate each missing directory"},
		{"dropped entry in the record", recordOf(jail.Mount{Source: "/lever/kb", Destination: "/workspace/kb"}), true, "read from the hub record", "keep these directories"},
		{"record current", recordOf(), true, "none configured", ""},
		{"no record", noRecord, true, "none configured", ""},
	} {
		r := checkManagerReadOnly(context.Background(), "/lever", tree, "assistant", nil, mgr, noContainer, c.record, refused)
		if r.ok != c.ok || !strings.Contains(r.detail, c.wantDetail) || !strings.Contains(r.fix, c.wantFix) {
			t.Errorf("%s: %+v", c.label, r)
		}
	}
	r := checkManagerReadOnly(context.Background(), "/lever", tree, "assistant", []config.TreeMount{{Rel: "kb", ReadOnly: true}}, mgr, noContainer, recordOf(), refused)
	if !strings.Contains(r.detail, "not checked (no manager container)") {
		t.Errorf("plan, no container: %+v", r)
	}
}

func TestCheckGuestDNS(t *testing.T) {
	const probe = "timeout 10 getent ahosts api.anthropic.com"
	answered := proc.NewFakeRunner()
	answered.Script(probe, proc.Result{Stdout: "160.79.104.10 STREAM api.anthropic.com\n"})
	if got := checkGuestDNS(context.Background(), false, answered); !got.ok || !strings.Contains(got.detail, "resolves") {
		t.Fatalf("an answered lookup must pass, got %+v", got)
	}

	// getent exits 2 with no output when the name does not resolve.
	silent := proc.NewFakeRunner()
	silent.Script(probe, proc.Result{Code: 2})
	got := checkGuestDNS(context.Background(), false, silent)
	if got.ok || !strings.Contains(got.detail, "cannot resolve") || !strings.Contains(got.fix, "LIMADNS") {
		t.Fatalf("a failed lookup must fail naming the lima resolver path, got %+v", got)
	}

	// A resolver that never answers hits the probe's timeout (exit 124).
	hung := proc.NewFakeRunner()
	hung.Script(probe, proc.Result{Code: 124})
	if got := checkGuestDNS(context.Background(), false, hung); got.ok || !strings.Contains(got.detail, "timed out") {
		t.Fatalf("a hung lookup must fail as a timeout, got %+v", got)
	}

	// Closed egress: DNS is dropped by design, so the row is informational and
	// the guest is never probed.
	never := proc.NewFakeRunner()
	if got := checkGuestDNS(context.Background(), true, never); !got.ok || !strings.Contains(got.detail, "not checked") {
		t.Fatalf("closed egress must be informational, got %+v", got)
	}
	if len(never.Calls) != 0 {
		t.Fatalf("closed egress must not probe the guest: %+v", never.Calls)
	}
}

func TestCheckPATTokensPassesOnACurrentRecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	now := time.Now()
	seedPAT(t, st, "controller", "tok")
	r := checkPATTokens(context.Background(), st, false, nil, now)
	if !r.ok {
		t.Fatalf("a current record must pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "expires in") {
		t.Errorf("detail should say when the token expires, got %q", r.detail)
	}
}

// A token with no record: no agent:message and an unknown 90-day expiry.
func TestCheckPATTokensFailsWithoutARecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := st.SaveControllerPAT("tok"); err != nil {
		t.Fatal(err)
	}
	r := checkPATTokens(context.Background(), st, false, nil, time.Now())
	if r.ok {
		t.Fatalf("a token without a record must fail; got %+v", r)
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Errorf("fix should be a re-apply, got %q", r.fix)
	}
}

func TestCheckPATTokensFailsOnScopeDrift(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "tok")
	if err := st.SaveControllerPATRecord(state.PATRecord{Requested: []string{"agent:manage"}, ExpiresAt: time.Now().Add(200 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r := checkPATTokens(context.Background(), st, false, nil, time.Now())
	if r.ok || !strings.Contains(r.detail, "scopes") {
		t.Fatalf("drifted scopes must fail and say so; got %+v", r)
	}
}

func TestCheckPATTokensFailsNearExpiry(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "tok")
	now := time.Now()
	if err := st.SaveControllerPATRecord(state.PATRecord{Requested: controllerPATScopes(false, false), ExpiresAt: now.Add(3 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r := checkPATTokens(context.Background(), st, false, nil, now)
	if r.ok || !strings.Contains(r.detail, "expire") {
		t.Fatalf("a token inside the renew window must fail; got %+v", r)
	}
}

func TestCheckPATTokensFailsWhenNoTokenExists(t *testing.T) {
	r := checkPATTokens(context.Background(), state.ForConfig(t.TempDir()), false, nil, time.Now())
	if r.ok {
		t.Fatalf("no controller PAT at all must fail; got %+v", r)
	}
}

func TestCheckPATTokensCoversTheRemoteTokenWhenEnabled(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "tok")
	if err := st.SaveRemotePAT("rtok"); err != nil {
		t.Fatal(err)
	}
	r := checkPATTokens(context.Background(), st, true, nil, time.Now())
	if r.ok || !strings.Contains(r.detail, "remote") {
		t.Fatalf("a remote token without a record must fail and name the remote token; got %+v", r)
	}
	if r := checkPATTokens(context.Background(), st, false, nil, time.Now()); !r.ok {
		t.Fatalf("with remote disabled the remote token is nobody's business; got %+v", r)
	}
}

func TestCheckAgentRoleCeilingPassesWhenSet(t *testing.T) {
	read := func(context.Context, string) (hubapi.RoleCeiling, error) {
		return hubapi.RoleCeiling{Max: "baseline", Default: "baseline"}, nil
	}
	r := checkAgentRoleCeiling(context.Background(), "lever", "baseline", rolesYes, read)
	if !r.ok || !strings.Contains(r.detail, "baseline") {
		t.Fatalf("got %+v", r)
	}
}

func TestCheckAgentRoleCeilingFailsWhenUnsetOrWider(t *testing.T) {
	for _, c := range []hubapi.RoleCeiling{{}, {Max: "full", Default: "baseline"}, {Max: "baseline", Default: ""}} {
		read := func(context.Context, string) (hubapi.RoleCeiling, error) { return c, nil }
		r := checkAgentRoleCeiling(context.Background(), "lever", "baseline", rolesYes, read)
		if r.ok {
			t.Fatalf("ceiling %+v must fail", c)
		}
		if !strings.Contains(r.fix, "lever apply") {
			t.Errorf("fix should be a re-apply, got %q", r.fix)
		}
	}
}

func TestCheckAgentRoleCeilingHubDownIsNotChecked(t *testing.T) {
	read := func(context.Context, string) (hubapi.RoleCeiling, error) {
		return hubapi.RoleCeiling{}, errors.New("dial: refused")
	}
	r := checkAgentRoleCeiling(context.Background(), "lever", "baseline", rolesYes, read)
	if !r.ok || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("got %+v", r)
	}
	answered := func(context.Context, string) (hubapi.RoleCeiling, error) {
		return hubapi.RoleCeiling{}, &hubapi.APIError{Status: 403, Msg: "forbidden"}
	}
	if r := checkAgentRoleCeiling(context.Background(), "lever", "baseline", rolesYes, answered); r.ok {
		t.Fatalf("a hub that answered 403 is a finding; got %+v", r)
	}
}

// A record scion's migration promoted to full is the same hazard as an
// unrolled one and must not be listed among the healthy records.
func TestCheckAgentRolesFlagsGrandfatheredRecords(t *testing.T) {
	list := func(context.Context, string) ([]hubapi.Agent, error) {
		return []hubapi.Agent{{Slug: "legacy", Role: "full", RoleGrandfathered: true}, {Slug: "scratch", Role: "baseline"}}, nil
	}
	r := checkAgentRoles(context.Background(), "lever", rolesYes, list)
	if r.ok {
		t.Fatalf("a grandfathered record must fail; got %+v", r)
	}
	if !strings.Contains(r.detail, "legacy") || !strings.Contains(r.detail, "grandfather") {
		t.Errorf("detail should name the record and the migration, got %q", r.detail)
	}
}

// A scion without --role has no ceiling setting: not a finding, and the hub is
// not asked. A probe that cannot answer is "not checked".
func TestCheckAgentRoleCeilingSkipsAPreRolesScion(t *testing.T) {
	asked := false
	read := func(context.Context, string) (hubapi.RoleCeiling, error) {
		asked = true
		return hubapi.RoleCeiling{}, nil
	}
	r := checkAgentRoleCeiling(context.Background(), "lever", "baseline", rolesNo, read)
	if !r.ok || asked || !strings.Contains(r.detail, "predates") {
		t.Fatalf("got %+v asked=%v", r, asked)
	}
	probeErr := func(context.Context) (bool, error) { return false, errors.New("exec: scion: not found") }
	r = checkAgentRoleCeiling(context.Background(), "lever", "baseline", probeErr, read)
	if !r.ok || asked || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("got %+v asked=%v", r, asked)
	}
}

func TestCheckScionTelemetry(t *testing.T) {
	settings := func(s string) settingsReader {
		return func(context.Context) ([]byte, error) { return []byte(s), nil }
	}
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	mgr := listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerID: "c1"})
	envIs := func(v string, set bool) envReader {
		return func(_ context.Context, ref, key string) (string, bool, error) {
			if ref != "c1" || key != "SCION_TELEMETRY_ENABLED" {
				return "", false, fmt.Errorf("unexpected %s %s", ref, key)
			}
			return v, set, nil
		}
	}
	const off = "telemetry:\n  enabled: false\n"
	cases := []struct {
		label      string
		mode       config.ScionTelemetryMode
		read       settingsReader
		list       agentLister
		env        envReader
		ok         bool
		wantDetail string
		wantFix    string
	}{
		{"off, converged, manager matches", config.ScionTelemetryOff, settings(off), mgr, envIs("false", true), true, "SCION_TELEMETRY_ENABLED=false", ""},
		{"off, settings missing the block", config.ScionTelemetryOff, settings("version: 1\n"), mgr, envIs("false", true), false, "scion's default", "lever apply"},
		{"off, manager predates it", config.ScionTelemetryOff, settings(off), mgr, envIs("", false), false, "unset", "lever stop && lever up"},
		{"off, manager started with true", config.ScionTelemetryOff, settings(off), mgr, envIs("true", true), false, "=true", "lever stop && lever up"},
		{"off, no manager record", config.ScionTelemetryOff, settings(off), listing(), envIs("", false), true, "no manager record", ""},
		{"off, no container", config.ScionTelemetryOff, settings(off), mgr,
			func(context.Context, string, string) (string, bool, error) { return "", false, jail.ErrNoContainer }, true, "no manager container", ""},
		{"scion-default leaves scion's telemetry alone", config.ScionTelemetryScionDefault, settings(""), mgr, envIs("", false), true, "scion's default", ""},
		{"settings unreadable", config.ScionTelemetryOff,
			func(context.Context) ([]byte, error) { return nil, fmt.Errorf("machine down") }, mgr, envIs("", false), true, "not checked", ""},
		{"settings garbage", config.ScionTelemetryOff, settings("telemetry:\n  enabled: maybe\n"), mgr, envIs("", false), false, "cannot be read", "lever apply"},
	}
	for _, c := range cases {
		r := checkScionTelemetry(context.Background(), c.mode, c.read, "/lever", "assistant", c.list, c.env)
		if r.ok != c.ok {
			t.Fatalf("%s: ok=%v, want %v (%+v)", c.label, r.ok, c.ok, r)
		}
		if !strings.Contains(r.detail, c.wantDetail) {
			t.Errorf("%s: detail %q should mention %q", c.label, r.detail, c.wantDetail)
		}
		if !strings.Contains(r.fix, c.wantFix) {
			t.Errorf("%s: fix %q should mention %q", c.label, r.fix, c.wantFix)
		}
	}
}

func TestCheckAgentNetwork(t *testing.T) {
	listing := func(agents ...scion.Agent) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) { return agents, nil }
	}
	modesOf := func(m map[string]string, broken ...string) netModeLister {
		return func(_ context.Context, ref string) (string, error) {
			if slices.Contains(broken, ref) {
				return "", fmt.Errorf("podman exploded")
			}
			mode, ok := m[ref]
			if !ok {
				return "", fmt.Errorf("inspect %s: %w", ref, jail.ErrNoContainer)
			}
			return mode, nil
		}
	}
	fleet := listing(
		scion.Agent{Slug: "assistant", Phase: "running", ContainerID: "cm"},
		scion.Agent{Slug: "w1", Phase: "running", ContainerID: "c1"},
		scion.Agent{Slug: "w2", Phase: "running"}) // no id: found by container name
	agents := []string{"assistant", "w1", "w2", "never-started"}
	cases := []struct {
		label      string
		list       agentLister
		modes      netModeLister
		ok         bool
		wantDetail string
		wantFix    string
	}{
		{"all pasta", fleet, modesOf(map[string]string{"cm": "pasta", "c1": "pasta", "proj--w2": "pasta"}), true, "3 agent container(s) run pasta", ""},
		{"slirp4netns manager (lever#35)", fleet, modesOf(map[string]string{"cm": "slirp4netns", "c1": "pasta"}), false, "assistant (slirp4netns)", "`lever stop` and `lever up`"},
		{"host network worker by container name", fleet, modesOf(map[string]string{"cm": "pasta", "proj--w2": "host"}), false, "w2 (host)", "recreates"},
		{"no containers", fleet, modesOf(nil), true, "no agent containers", ""},
		{"inspect failure is not a finding", fleet, modesOf(map[string]string{"cm": "pasta"}, "c1"), true, "could not inspect w1", ""},
		{"list failure", func(context.Context, string) ([]scion.Agent, error) { return nil, fmt.Errorf("hub down") }, modesOf(nil), true, "not checked (could not list agents)", ""},
		{"no probes", nil, nil, true, "not checked", ""},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			r := checkAgentNetwork(context.Background(), "/lever/proj", agents, false, c.list, c.modes)
			if r.ok != c.ok || !strings.Contains(r.detail, c.wantDetail) || !strings.Contains(r.fix, c.wantFix) {
				t.Fatalf("got ok=%v detail=%q fix=%q; want ok=%v detail~%q fix~%q", r.ok, r.detail, r.fix, c.ok, c.wantDetail, c.wantFix)
			}
		})
	}
}

func TestCheckAgentNetworkForceHostNetwork(t *testing.T) {
	list := func(context.Context, string) ([]scion.Agent, error) {
		return []scion.Agent{{Slug: "assistant", ContainerID: "cm"}}, nil
	}
	modes := func(context.Context, string) (string, error) { return "host", nil }
	r := checkAgentNetwork(context.Background(), "/lever/proj", []string{"assistant"}, true, list, modes)
	if !r.ok || !strings.Contains(r.detail, jail.ForceHostNetworkEnv) {
		t.Fatalf("got ok=%v detail=%q; the escape hatch is deliberate, not a finding", r.ok, r.detail)
	}
}

func TestNetworkCheckedAgentsIncludesManager(t *testing.T) {
	got := networkCheckedAgents("assistant", []string{"w1", "w2"})
	if !slices.Equal(got, []string{"assistant", "w1", "w2"}) {
		t.Fatalf("got %v; the manager must be inspected too", got)
	}
}

// TestCheckWorkerTreeBootstraps: a bootstrap.json under a worker's tree fails
// and names the file; the tree root's is the manager's. A path through a
// planted symlink fails too but gets no rm line, which could delete the
// manager's bootstrap.
func TestCheckWorkerTreeBootstraps(t *testing.T) {
	tree := t.TempDir()
	dirs := map[string]string{"a": filepath.Join(tree, "workers", "a"), "b": filepath.Join(tree, "workers", "b"), "c": filepath.Join(tree, "workers", "c")}
	for _, d := range []string{tree, dirs["a"], dirs["b"]} {
		if err := os.MkdirAll(filepath.Join(d, ".lever"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	managers := filepath.Join(tree, ".lever", "bootstrap.json")
	if err := os.WriteFile(managers, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkWorkerTreeBootstraps(tree, dirs); !r.ok {
		t.Fatalf("no worker copy: %+v, want a pass", r)
	}
	stray := filepath.Join(dirs["b"], ".lever", "bootstrap.json")
	if err := os.WriteFile(stray, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := checkWorkerTreeBootstraps(tree, dirs)
	if r.ok || !strings.Contains(r.detail, "worker b ") || !strings.Contains(r.fix, "rm -- "+shellQuote(stray)) {
		t.Fatalf("a copy in worker b's tree: %+v, want a failure that names b and the quoted file", r)
	}
	// Worker c plants `.lever` as a link to the manager's directory.
	if err := os.MkdirAll(dirs["c"], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tree, ".lever"), filepath.Join(dirs["c"], ".lever")); err != nil {
		t.Fatal(err)
	}
	r = checkWorkerTreeBootstraps(tree, dirs)
	viaLink := filepath.Join(dirs["c"], ".lever", "bootstrap.json")
	if r.ok || !strings.Contains(r.detail, "symbolic link") || strings.Contains(r.fix, viaLink) || !strings.Contains(r.fix, "do NOT delete through the path") {
		t.Fatalf("a planted .lever link: %+v, want a failure with no rm line for %s", r, viaLink)
	}
	if !strings.Contains(r.fix, shellQuote(stray)) {
		t.Fatalf("the real copy in worker b must still get its rm line: %q", r.fix)
	}
	if _, err := os.Stat(managers); err != nil {
		t.Fatalf("the manager's bootstrap must be untouched: %v", err)
	}
	if r := checkWorkerTreeBootstraps(tree, nil); !r.ok {
		t.Fatalf("no workers: %+v", r)
	}
}

// TestShellQuote: a pasted rm line must not expand anything a path contains.
func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b":             `'/a/b'`,
		"/a/$(touch x)/b":  `'/a/$(touch x)/b'`,
		"/a/it's`id`/b":    `'/a/it'\''s` + "`id`" + `/b'`,
		"/a/new\nline -rf": `'/a/new` + "\n" + `line -rf'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCheckNestedVirt(t *testing.T) {
	ctx := context.Background()
	if r := checkNestedVirt(ctx, false, false, nil, "", nil); !r.ok || !strings.Contains(r.detail, "not applicable") {
		t.Fatalf("off, not lima: %+v", r)
	}
	// guestOK answers the legacy drop-in test with exit 1 (absent).
	guestOK := func() *proc.FakeRunner {
		f := proc.NewFakeRunner()
		f.Script("stat -c %F %a /dev/kvm", proc.Result{Stdout: "character special file 666\n"})
		f.Script("sh -c", proc.Result{Code: 1})
		return f
	}
	has := func(ctx context.Context, ref, p string) (bool, error) { return true, nil }
	lacks := func(ctx context.Context, ref, p string) (bool, error) { return false, nil }

	if r := checkNestedVirt(ctx, true, true, guestOK(), "p--m", has); !r.ok {
		t.Fatalf("all good: %+v", r)
	}
	if r := checkNestedVirt(ctx, true, true, guestOK(), "p--m", lacks); r.ok || !strings.Contains(r.fix, "lever up --fresh --keep-conversation") {
		t.Fatalf("container lacks kvm: %+v", r)
	}
	bad := proc.NewFakeRunner()
	bad.Script("stat -c %F %a /dev/kvm", proc.Result{Stdout: "character special file 660\n"})
	if r := checkNestedVirt(ctx, true, true, bad, "p--m", has); r.ok || !strings.Contains(r.detail, "660") {
		t.Fatalf("wrong mode: %+v", r)
	}
	legacy := guestOK()
	legacy.Script("sh -c", proc.Result{})
	if r := checkNestedVirt(ctx, true, true, legacy, "p--m", has); r.ok || !strings.Contains(r.detail, "legacy") || !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("legacy drop-in: %+v", r)
	}

	// Off on Lima: the manager created while it was on keeps the device.
	if r := checkNestedVirt(ctx, false, true, guestOK(), "p--m", has); r.ok || !strings.Contains(r.detail, "still has /dev/kvm") || !strings.Contains(r.fix, "lever up --fresh") {
		t.Fatalf("off, manager has kvm: %+v", r)
	}
	if r := checkNestedVirt(ctx, false, true, guestOK(), "p--m", lacks); !r.ok {
		t.Fatalf("off, manager lacks kvm: %+v", r)
	}
	if r := checkNestedVirt(ctx, false, true, legacy, "p--m", lacks); r.ok || !strings.Contains(r.detail, "legacy") {
		t.Fatalf("off, legacy drop-in: %+v", r)
	}
}

func TestCheckRecyclableWorkers(t *testing.T) {
	if r := checkRecyclableWorkers(nil); !r.ok || r.detail != "no workers declared" {
		t.Fatalf("no workers: %+v", r)
	}
	ws := []config.Worker{{Name: "deal-2", Recyclable: true}, {Name: "scratch"}, {Name: "deal-1", Recyclable: true}}
	r := checkRecyclableWorkers(ws)
	if !r.ok || !strings.Contains(r.detail, "worker {deal-1,deal-2} recyclable") || strings.Contains(r.detail, "scratch") {
		t.Fatalf("two recyclable: %+v", r)
	}
	if r := checkRecyclableWorkers(ws[1:2]); !r.ok || !strings.Contains(r.detail, "no worker is recyclable") {
		t.Fatalf("none recyclable: %+v", r)
	}
}
