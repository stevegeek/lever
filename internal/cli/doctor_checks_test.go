package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/remoteproxy"
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

// healthyRemoteProbes makes the remote-access chain look green: the proxy
// answers healthz 200, the local OIDC provider serves discovery with NO
// authorization endpoint (the 404 the whole design rests on — see
// remoteproxy.Provider.handleAuthorize), and the hub starts logins against
// lever's dead authorization endpoint. Every probe is stubbed rather than
// left real: the login probes talk to loopback ports, and a test must never
// reach a proxy running on this host.
func healthyRemoteProbes() doctorProbes {
	return doctorProbes{
		dial:          okDial,
		remoteHealthz: func(int, string) (int, error) { return 200, nil },
		remoteLogin: func(int) (loginProbeResult, error) {
			return loginProbeResult{discovery: 200, authorize: 404, authzURL: "https://lever.invalid/authorize"}, nil
		},
		remoteJailLogin: func(context.Context, leverexec.Runner, string) (int, string, error) {
			return 302, remoteproxy.DeadAuthorizationEndpoint, nil
		},
	}
}

// writePIDFile records pid at path inside st.Dir, creating the state dir.
func writePIDFile(t *testing.T, st brokerctl.State, path string, pid int) {
	t.Helper()
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeBrokerPID(t *testing.T, st brokerctl.State, pid int) { writePIDFile(t, st, st.PID(), pid) }
func writeRemotePID(t *testing.T, st brokerctl.State, pid int) {
	writePIDFile(t, st, st.RemotePID(), pid)
}

// writeDoctorConfig writes a minimal lever.yaml on the orbstack backend with
// extra appended raw (e.g. "remote:\n  enabled: true\n"), or nothing when
// extra is "", and loads it. Mirrors apply_test.go's writeTmpConfig /
// config_test.go's writeTmp.
func writeDoctorConfig(t *testing.T, extra string) *config.App {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, config.CanonicalName)
	body := "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestCheckBrokerAliveNotStarted(t *testing.T) {
	r := checkBrokerAlive(brokerctl.StateDir(t.TempDir()), 8443, okProbes)
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
	r := checkBrokerAlive(st, 8443, okProbes)
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
	r := checkBrokerAlive(st, 8443, failProbes)
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
	r := checkBrokerAlive(st, 8443, okProbes)
	if !r.ok {
		t.Fatalf("alive process + listening port must pass; got %+v", r)
	}
}

func TestCheckToolBackendsNoneDeclared(t *testing.T) {
	r := checkToolBackends(nil, failProbes)
	if !r.ok {
		t.Fatalf("no tools declared => pass (nothing to probe); got %+v", r)
	}
}

func TestCheckToolBackendsAllReachable(t *testing.T) {
	tools := []config.Tool{
		{Name: "things3", External: true, Backend: "127.0.0.1:3300"},
		{Name: "qmd", External: true, Backend: "127.0.0.1:3101/mcp"},
		{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:3201"},
	}
	r := checkToolBackends(tools, okProbes)
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
		{Name: "things3", External: true, Backend: "127.0.0.1:3300"},
		{Name: "qmd", External: true, Backend: "127.0.0.1:3101/mcp"},
	}
	r := checkToolBackends(tools, doctorProbes{dial: dial})
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

func TestCheckToolBackendsSupervisedMissing(t *testing.T) {
	tools := []config.Tool{{Name: "db", Command: []string{"definitely-not-on-path-xyz"}}}
	got := checkToolBackends(tools, okProbes)
	if got.ok {
		t.Fatalf("supervised tool with missing binary should fail the check")
	}
}

func TestCheckToolBackendsExternalDown(t *testing.T) {
	tools := []config.Tool{{Name: "x", External: true, Backend: "127.0.0.1:59999"}}
	got := checkToolBackends(tools, failProbes)
	if got.ok {
		t.Fatalf("down external backend should fail the check")
	}
}

// TestCheckToolBackendsSupervisedNeverDialed pins that a supervised tool's
// Backend (its own MCP listen address, unrelated to spawnability) is never
// TCP-dialed by the check — only external tools are dialed. Pairs a
// resolvable supervised command with an always-failing dial: if the
// supervised branch ever dialed Backend, this would wrongly fail.
func TestCheckToolBackendsSupervisedNeverDialed(t *testing.T) {
	tools := []config.Tool{{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:59999"}}
	if got := checkToolBackends(tools, failProbes); !got.ok {
		t.Fatalf("supervised tool's Backend must never be dialed; got %+v", got)
	}
}

// TestCheckToolBackendsSupervisedAbsolutePathMissing covers the
// slash-containing (absolute path) supervised branch: today an absolute
// command path got zero doctor coverage (only the PATH-scoped branch was
// checked). A missing absolute-path binary must fail the check same as a
// not-on-PATH bare name.
func TestCheckToolBackendsSupervisedAbsolutePathMissing(t *testing.T) {
	tools := []config.Tool{{Name: "db", Command: []string{"/nonexistent/definitely-not-here-xyz"}}}
	got := checkToolBackends(tools, okProbes)
	if got.ok {
		t.Fatalf("supervised tool with a missing absolute-path command should fail the check")
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
	// The scion#925 default: a writable dir mounted into every agent. It must
	// fail the check, name itself, and offer a fix.
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
	// A read-only entry is still a channel, so it still fails — but the operator
	// needs to see that it is not writable before deciding what to do.
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
	// A stopped instance already fails the broker check. A second red line here
	// would be noise, so an unreachable hub passes — but says it was skipped.
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
	// A 403 (a PAT missing project:read) or a project the hub does not list is
	// NOT a down instance. Reporting it as "not checked" would hide a real
	// problem behind a pass, so anything the hub actually answered must fail.
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
	// The hub knows the project by its in-jail mount basename. ensureControllerPAT
	// derives the same key for `hub token create`; if these drift, the strip and
	// the check both look up a project that does not exist.
	if got := hubProjectKey("/lever"); got != "lever" {
		t.Fatalf("hubProjectKey(/lever) = %q, want lever", got)
	}
	if hubProjectKey("/lever") != filepath.Base(apply.JailPath("/anything", "/anything", "/lever")) {
		t.Error("hubProjectKey must match the key ensureControllerPAT mints the PAT with")
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
	r := checkGoToolchain(config.ScionConfig{}, doctorProbes{})
	if !r.ok {
		t.Fatalf("no source and no version pinned => no build => pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "not required") {
		t.Fatalf("detail should say a build isn't required: %q", r.detail)
	}
}

func TestCheckGoToolchainProbeOK(t *testing.T) {
	p := doctorProbes{goVersion: func() (string, error) { return "go version go1.26.4 darwin/arm64\n", nil }}

	r := checkGoToolchain(config.ScionConfig{Version: "666333f9"}, p)
	if !r.ok {
		t.Fatalf("a working go on PATH must pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "go1.26.4") {
		t.Fatalf("detail should report the go version: %q", r.detail)
	}
}

func TestCheckGoToolchainProbeError(t *testing.T) {
	p := doctorProbes{goVersion: func() (string, error) { return "", errors.New("exit status 126") }}

	r := checkGoToolchain(config.ScionConfig{Source: "/Users/stephen/ai/scion"}, p)
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

	// Owner edit → fail; the hint must signpost BOTH exits (--adopt to accept,
	// --force to restore) — a user hitting this nag discovers adoption here.
	// The custom content carries a CURRENT lever-version stamp: an adopted
	// file's stamp is the owner's attestation of the framework baseline it
	// was reviewed against, and a missing/old stamp fails the check (#16,
	// covered below).
	op := filepath.Join(tree, ".claude", "skills", "lever-operator", "SKILL.md")
	edited := "---\nname: custom\nlever-version: " + Version + "\n---\nmy own guidance\n"
	if err := os.WriteFile(op, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if res = checkOperatorSkills(app, stateDir); res.ok || !strings.Contains(res.fix, "--force") || !strings.Contains(res.fix, "--adopt") {
		t.Fatalf("owner-edit must fail with --adopt and --force hints: %+v", res)
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

	// Adopted baseline STAMPED WITH AN OLD VERSION → fail (#16): the file is
	// pinned to a framework baseline that has since moved on, possibly past
	// security-relevant guidance, and doctor is the only surface that can say
	// so. Also covers the missing-stamp case via the "unknown" label.
	stale := "---\nname: custom\nlever-version: 0.3.1\n---\nmy own guidance\n"
	if err := os.WriteFile(op, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
	}
	res = checkOperatorSkills(app, stateDir)
	if res.ok || !strings.Contains(res.detail, "0.3.1") || !strings.Contains(res.detail, Version) {
		t.Fatalf("stale adopted baseline must fail naming both versions: %+v", res)
	}
	if !strings.Contains(res.fix, "--adopt") || !strings.Contains(res.fix, "--force") {
		t.Fatalf("stale-baseline fix must offer re-adopt and reclaim: %+v", res)
	}

	// Restore the current-stamp adoption for the drift scenario below.
	if err := os.WriteFile(op, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSkills(app, stateDir); err != nil {
		t.Fatal(err)
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

// writeDirectivesConfig writes a minimal lever.yaml with operator.allowed_signers
// set to signersRel (relative to the instance root), or omitted entirely when
// signersRel is "".
func writeDirectivesConfig(t *testing.T, signersRel string) *config.App {
	t.Helper()
	if signersRel == "" {
		return writeDoctorConfig(t, "")
	}
	return writeDoctorConfig(t, "operator:\n  allowed_signers: "+signersRel+"\n")
}

// writeAllowedSigners generates a real ed25519 SSH keypair and writes a
// one-line allowed_signers file at path, principal "operator@demo" (matches
// writeDirectivesConfig's app name). Mirrors opsig_test.go's genKey.
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

// TestCheckDirectivesNotConfigured covers the default, opt-in-only state: no
// operator.allowed_signers means no directive channel, and that's a pass, not
// a warning — most instances never touch operator directives.
func TestCheckDirectivesNotConfigured(t *testing.T) {
	app := writeDirectivesConfig(t, "")
	st := brokerctl.StateDir(t.TempDir())
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
	st := brokerctl.StateDir(t.TempDir())
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
	// Comments and blank lines only — zero substantive key lines.
	if err := os.WriteFile(path, []byte("# no keys yet\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := brokerctl.StateDir(t.TempDir())
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
	st := brokerctl.StateDir(t.TempDir()) // no broker.pid => broker not running
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
	st := brokerctl.StateDir(t.TempDir())
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
	st := brokerctl.StateDir(t.TempDir())
	writeBrokerPID(t, st, os.Getpid()) // alive, but directive.sock never created
	r := checkDirectives(app, st)
	if r.ok {
		t.Fatal("broker alive but directive socket absent must fail")
	}
	if !strings.Contains(r.detail, "socket") {
		t.Fatalf("failure should be about the missing directive socket: %+v", r)
	}
}

// writeRemotePAT writes a remote.pat file at the given mode, creating the
// state dir first.
func writeRemotePAT(t *testing.T, st brokerctl.State, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.RemotePAT(), []byte("scion_pat_x"), mode); err != nil {
		t.Fatal(err)
	}
}

// TestCheckRemoteDisabled covers the default, opt-in-only state: remote
// access unconfigured is a pass — most instances never turn it on.
func TestCheckRemoteDisabled(t *testing.T) {
	app := writeDoctorConfig(t, "")
	st := brokerctl.StateDir(t.TempDir())
	r := checkRemote(context.Background(), app, st, okProbes, nil)
	if !r.ok {
		t.Fatalf("remote disabled must pass: %+v", r)
	}
	if !strings.Contains(r.detail, "disabled") {
		t.Fatalf("detail should say disabled: %q", r.detail)
	}
}

func TestCheckRemoteNoPIDFile(t *testing.T) {
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := brokerctl.StateDir(t.TempDir()) // no remote.pid — never started
	r := checkRemote(context.Background(), app, st, okProbes, nil)
	if r.ok {
		t.Fatal("enabled but never started (no remote.pid) must fail")
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("fix should point at lever apply: %q", r.fix)
	}
}

func TestCheckRemotePATMissing(t *testing.T) {
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := brokerctl.StateDir(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	// remote.pat intentionally left absent.
	r := checkRemote(context.Background(), app, st, okProbes, nil)
	if r.ok {
		t.Fatal("missing remote.pat must fail")
	}
	if !strings.Contains(r.fix, "lever apply") {
		t.Fatalf("fix should point at lever apply: %q", r.fix)
	}
}

// A remote.pat that exists but is group/other-accessible must fail too —
// same posture as checkCredentialFile.
func TestCheckRemotePATBadPermissions(t *testing.T) {
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := brokerctl.StateDir(t.TempDir())
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
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := brokerctl.StateDir(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	// Everything healthz depends on is green, so the 500 is the only failure
	// left to report.
	p := healthyRemoteProbes()
	p.remoteHealthz = func(int, string) (int, error) { return 500, nil }

	r := checkRemote(context.Background(), app, st, p, nil)
	if r.ok {
		t.Fatal("a non-200 from the healthz probe must fail")
	}
	if !strings.Contains(r.detail, "500") {
		t.Fatalf("detail should mention the bad status: %q", r.detail)
	}
}

// enabled+all-green: pid alive, port listening, PAT present at 0600, the
// end-to-end healthz probe returns 200, and the login provider is serving
// discovery with no authorization endpoint.
func TestCheckRemoteHealthy(t *testing.T) {
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := brokerctl.StateDir(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	r := checkRemote(context.Background(), app, st, healthyRemoteProbes(), nil)
	if !r.ok {
		t.Fatalf("pid alive + listening + PAT present + healthz 200 must pass: %+v", r)
	}
}

// TestCheckRemoteDiagnosesTheLoginPathBeforeHealthz pins the ORDER of the two
// probes, because the order decides what an operator reads at 2am.
//
// /healthz is not an API path, so the proxy opens a hub web session before it
// forwards the request (remoteproxy.NewHandler's session gate) — a broken
// login chain makes healthz answer 502 as well. With healthz probed first,
// doctor reported "GET /healthz returned 502 — inspect remote.log" and never
// reached checkRemoteLoginPath, so the operator lost the one message that says
// what to DO (a login port granted since the instance came up needs `lever
// down` + `lever up`). The cause must win over the symptom.
func TestCheckRemoteDiagnosesTheLoginPathBeforeHealthz(t *testing.T) {
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := brokerctl.StateDir(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	// The host-side provider is healthy; the GUEST half is not — the hub
	// cannot reach the provider through the forwarder, and answers 500.
	p := healthyRemoteProbes()
	p.remoteJailLogin = func(context.Context, leverexec.Runner, string) (int, string, error) {
		return 500, "", nil
	}
	// What the live proxy answers while the login chain is broken.
	healthzProbed := false
	p.remoteHealthz = func(int, string) (int, error) {
		healthzProbed = true
		return 502, nil
	}

	r := checkRemote(context.Background(), app, st, p, leverexec.NewFakeRunner())
	if r.ok {
		t.Fatal("a hub that cannot reach the login provider must fail the check")
	}
	if !strings.Contains(r.detail, "could not reach lever's login provider") {
		t.Fatalf("detail = %q, want the login-path diagnosis rather than the healthz symptom", r.detail)
	}
	if !strings.Contains(r.fix, "lever down") {
		t.Fatalf("fix = %q, want the actionable egress remediation", r.fix)
	}
	if healthzProbed {
		t.Fatal("healthz was probed before the login diagnosis: its 502 is a CONSEQUENCE of the broken login, and reporting it shadows the cause")
	}
}

// TestCheckRemoteFlagsALiveAuthorizeEndpoint is doctor's copy of the
// /authorize decision: a provider that answers anything but 404 there can mint
// an authorization code over HTTP, on a port every jailed agent reaches
// through the guest forwarder. Nothing legitimate calls it — the proxy drives
// the login server-side and mints in-process.
func TestCheckRemoteFlagsALiveAuthorizeEndpoint(t *testing.T) {
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := brokerctl.StateDir(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)
	for _, tc := range []struct {
		name  string
		probe loginProbeResult
		want  string
	}{
		{"authorize answers", loginProbeResult{discovery: 200, authorize: 302, authzURL: "https://lever.invalid/authorize"}, "/authorize"},
		{"discovery advertises a loopback authorize endpoint", loginProbeResult{discovery: 200, authorize: 404, authzURL: "http://127.0.0.1:8446/authorize"}, "on loopback"},
		{"discovery advertises localhost by name", loginProbeResult{discovery: 200, authorize: 404, authzURL: "http://localhost:9999/authorize"}, "on loopback"},
		{"discovery is not served", loginProbeResult{discovery: 500, authorize: 404}, "discovery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := healthyRemoteProbes()
			p.remoteLogin = func(int) (loginProbeResult, error) { return tc.probe, nil }
			r := checkRemote(context.Background(), app, st, p, nil)
			if r.ok {
				t.Fatalf("%s must fail the check", tc.name)
			}
			if !strings.Contains(r.detail, tc.want) {
				t.Fatalf("detail = %q, want it to name %q", r.detail, tc.want)
			}
		})
	}
}

// The proxy's own AllowedUsers gate (remoteproxy.Handler) trusts whatever
// Tailscale-User-Login header a request carries. doctor's liveness probe
// runs host-side — already as trusted as the PAT file it just read — so it
// must send the first configured allowed_users entry, or a pinned instance
// would 403 its own doctor check even when everything is actually healthy.
func TestCheckRemoteHealthzProbeUsesFirstAllowedUser(t *testing.T) {
	app := writeDoctorConfig(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n  allowed_users: [\"steve@example.com\", \"other@example.com\"]\n")
	st := brokerctl.StateDir(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	var gotLogin string
	p := healthyRemoteProbes()
	p.remoteHealthz = func(_ int, tsLogin string) (int, error) {
		gotLogin = tsLogin
		return 200, nil
	}

	if r := checkRemote(context.Background(), app, st, p, nil); !r.ok {
		t.Fatalf("expected pass, got %+v", r)
	}
	if gotLogin != "steve@example.com" {
		t.Fatalf("probe should carry the first allowed_users entry, got %q", gotLogin)
	}
}

func TestCheckClaudeVersion(t *testing.T) {
	probes := func(v string, err error) doctorProbes {
		return doctorProbes{claudeVersion: func(string) (string, error) { return v, err }}
	}
	if got := checkClaudeVersion("img", probes("2.1.207", nil)); !got.ok || !strings.Contains(got.detail, "2.1.207") {
		t.Fatalf("expected pass reporting the version, got %+v", got)
	}

	// Missing label (older image) → informational pass, not a hard fail.
	if got := checkClaudeVersion("img", probes("", nil)); !got.ok {
		t.Fatalf("missing label should be informational, not a failure, got %+v", got)
	}

	// Inspect error → fail with actionable fix.
	if got := checkClaudeVersion("img", probes("", fmt.Errorf("no such image"))); got.ok {
		t.Fatalf("inspect error should fail the check")
	}
}

// rolesYes/rolesNo stand in for the scion capability probe.
func rolesYes(context.Context) (bool, error) { return true, nil }
func rolesNo(context.Context) (bool, error)  { return false, nil }

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

// Pre-scion#1089 the same records are harmless, so this must not cry wolf —
// but it is the one place an operator can learn a bump will promote them.
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

// checkNodeToolchain is the guard that keeps a missing node from surfacing as
// scion's bare "Web UI Not Available" page in the browser, hours and one
// context-switch away from the cause.
func TestCheckNodeToolchainNotRequired(t *testing.T) {
	for _, c := range []struct {
		name string
		app  *config.App
	}{
		{"remote off", &config.App{Scion: config.ScionConfig{Version: "e82a2a08"}}},
		// No scion source to build the SPA from; the operator's own binary may
		// already embed it.
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
		Scion:  config.ScionConfig{Source: "/Users/stephen/ai/scion"},
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

// TestCheckRemoteLoginPathProvesTheGuestHalf: the host-side provider probe can
// be perfectly green while the browser gets a 502, because it never touches
// the guest forwarder or the hub's own oidc_login block. Asking the HUB to
// start a login is what exercises both — it has to fetch discovery through the
// forwarder before it can answer.
func TestCheckRemoteLoginPathProvesTheGuestHalf(t *testing.T) {
	jr := leverexec.NewFakeRunner()
	st := brokerctl.StateDir(t.TempDir())

	for _, tc := range []struct {
		name     string
		status   int
		redirect string
		err      error
		ok       bool
		want     string
	}{
		{"healthy", 302, remoteproxy.DeadAuthorizationEndpoint + "?client_id=lever-remote", nil, true, "reaches the provider"},
		{"hub has no oidc_login", 400, "", nil, false, "does not have lever's OIDC login configured"},
		{"forwarder down", 500, "", nil, false, "could not reach lever's login provider"},
		{"another IdP configured", 302, "https://accounts.google.example/o/oauth2/auth", nil, false, "not lever's provider"},
		{"jail unreachable", 0, "", errors.New("machine not found"), false, "from inside the jail"},
		{"unexpected status", 418, "", nil, false, "want 302"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := doctorProbes{remoteJailLogin: func(context.Context, leverexec.Runner, string) (int, string, error) {
				return tc.status, tc.redirect, tc.err
			}}
			detail, _, ok := checkRemoteLoginPath(context.Background(), jr, st, p)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (detail %q)", ok, tc.ok, detail)
			}
			if !strings.Contains(detail, tc.want) {
				t.Fatalf("detail = %q, want it to mention %q", detail, tc.want)
			}
		})
	}
}

// The probe must ask the hub the way an unauthenticated browser would, from
// inside the jail, with an absolute curl path (the run user's PATH has
// writable directories ahead of /usr/bin).
func TestRemoteJailLoginScriptShape(t *testing.T) {
	for _, want := range []string{"/usr/bin/curl", "-o /dev/null", "%{http_code} %{redirect_url}"} {
		if !strings.Contains(remoteJailLoginScript, want) {
			t.Fatalf("script missing %q:\n%s", want, remoteJailLoginScript)
		}
	}
	if strings.Contains(remoteJailLoginScript, "Authorization") {
		t.Fatalf("the login route is public; sending a credential would test the wrong thing:\n%s", remoteJailLoginScript)
	}
}

func TestStateRel(t *testing.T) {
	st := brokerctl.StateDir(filepath.Join(t.TempDir(), "inst"))
	if got := stateRel(st, st.RemoteLog()); got != ".lever-state/remote.log" {
		t.Fatalf("stateRel = %q", got)
	}
	if got := stateRel(st, st.Log()); got != ".lever-state/broker.log" {
		t.Fatalf("stateRel = %q", got)
	}
}

// checkListeningProcess is the ladder both the broker and remote checks climb;
// each rung names the pid file and the log the caller passed in.
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

// claudeVersionProbe maps docker's "<no value>" (label absent) to "", and
// reports docker's own stderr on failure.
func TestClaudeVersionProbe(t *testing.T) {
	r := leverexec.NewFakeRunner()
	r.Script("docker image inspect --format {{index .Config.Labels \"claude_code_version\"}} labelled", leverexec.Result{Stdout: "2.1.207\n"})
	r.Script("docker image inspect --format {{index .Config.Labels \"claude_code_version\"}} bare", leverexec.Result{Stdout: "<no value>\n"})
	if v, err := claudeVersionProbe(r, "labelled"); err != nil || v != "2.1.207" {
		t.Fatalf("labelled: %q, %v", v, err)
	}
	if v, err := claudeVersionProbe(r, "bare"); err != nil || v != "" {
		t.Fatalf("bare: %q, %v", v, err)
	}
	if _, err := claudeVersionProbe(r, "missing"); err == nil {
		t.Fatal("an inspect failure must be an error")
	}
}

// productionProbes wires every field: a nil probe would panic the first time
// a check on a real instance reached it.
func TestProductionProbesWiresEveryField(t *testing.T) {
	p := productionProbes(leverexec.NewFakeRunner())
	if p.dial == nil || p.goVersion == nil || p.nodeToolchain == nil || p.claudeVersion == nil ||
		p.remoteHealthz == nil || p.remoteLogin == nil || p.remoteJailLogin == nil {
		t.Fatalf("productionProbes left a probe nil: %+v", p)
	}
}
