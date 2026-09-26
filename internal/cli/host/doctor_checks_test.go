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
	"github.com/stevegeek/lever/internal/provision/webassets"
	"github.com/stevegeek/lever/internal/remoteproxy"
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
		remoteHealthz: func(healthzProbe) (int, error) { return 200, nil },
		remoteLogin: func(int) (loginProbeResult, error) {
			return loginProbeResult{discovery: 200, authorize: 404, authzURL: "https://lever.invalid/authorize"}, nil
		},
		remoteJailLogin: func(context.Context, proc.Runner, string) (int, string, error) {
			return 302, remoteproxy.DeadAuthorizationEndpoint, nil
		},
	}
}

// writePIDFile records pid at path inside st.Dir, creating the state dir.
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
	// The exact bad-teardown bug: registered for /lever, but the marker is gone.
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
	// Two entries for /lever even with the marker present — a duplicate that
	// scion init trips over.
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
	stateDir := state.ForConfig(root)

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
	edited := "---\nname: custom\nlever-version: " + cli.Version + "\n---\nmy own guidance\n"
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
	if res.ok || !strings.Contains(res.detail, "0.3.1") || !strings.Contains(res.detail, cli.Version) {
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
		return loadInstance(t, "")
	}
	return loadInstance(t, "operator:\n  allowed_signers: "+signersRel+"\n")
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
	// Comments and blank lines only — zero substantive key lines.
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

// writeRemotePAT writes a remote.pat file at the given mode, creating the
// state dir first.
func writeRemotePAT(t *testing.T, st state.State, mode os.FileMode) {
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

	// Everything healthz depends on is green, so the 500 is the only failure
	// left to report.
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

// enabled+all-green: pid alive, port listening, PAT present at 0600, the
// end-to-end healthz probe returns 200, and the login provider is serving
// discovery with no authorization endpoint.
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
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := state.ForConfig(t.TempDir())
	writeRemotePID(t, st, os.Getpid())
	writeRemotePAT(t, st, 0o600)

	// The host-side provider is healthy; the GUEST half is not — the hub
	// cannot reach the provider through the forwarder, and answers 500.
	p := healthyRemoteProbes()
	p.remoteJailLogin = func(context.Context, proc.Runner, string) (int, string, error) {
		return 500, "", nil
	}
	// What the live proxy answers while the login chain is broken.
	healthzProbed := false
	p.remoteHealthz = func(healthzProbe) (int, error) {
		healthzProbed = true
		return 502, nil
	}

	r := checkRemote(context.Background(), app, st, p, proc.NewFakeRunner())
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
	app := loadInstance(t, "remote:\n  enabled: true\n  base_url: \"https://demo.tailnet.ts.net\"\n")
	st := state.ForConfig(t.TempDir())
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

// With another front, the probe must speak ITS header and dial where the
// proxy actually listens — a non-loopback bind has no 127.0.0.1 listener.
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

// Each weakening setting gets a warning row — shown, with a fix, and never
// counted as a failure — and a default remote block passes plainly.
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

	// Missing label (older image) → informational pass, not a hard fail.
	if got := checkClaudeVersion("img", "", probes("", nil)); !got.ok {
		t.Fatalf("missing label should be informational, not a failure, got %+v", got)
	}

	// Inspect error → fail with actionable fix.
	if got := checkClaudeVersion("img", "", probes("", fmt.Errorf("no such image"))); got.ok {
		t.Fatalf("inspect error should fail the check")
	}
}

// With an image_tar the label comes from the archive: the host docker probe
// is never consulted (a deploy host may have no docker), the detail names
// the tar, and a tar the ref is missing from fails with the tar named.
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
	jr := proc.NewFakeRunner()
	st := state.ForConfig(t.TempDir())

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
			p := doctorProbes{remoteJailLogin: func(context.Context, proc.Runner, string) (int, string, error) {
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
	r := proc.NewFakeRunner()
	r.Script("docker image inspect --format {{index .Config.Labels \"claude_code_version\"}} labelled", proc.Result{Stdout: "2.1.207\n"})
	r.Script("docker image inspect --format {{index .Config.Labels \"claude_code_version\"}} bare", proc.Result{Stdout: "<no value>\n"})
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
	p := productionProbes(proc.NewFakeRunner())
	if p.dial == nil || p.goVersion == nil || p.nodeToolchain == nil || p.claudeVersion == nil ||
		p.remoteHealthz == nil || p.remoteLogin == nil || p.remoteJailLogin == nil {
		t.Fatalf("productionProbes left a probe nil: %+v", p)
	}
}

// checkManagerLive (lever#31): the one doctor row that says whether the agent
// itself is up, not just the plumbing around it.
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
		// Activity and its age ride along on a live manager; a dead activity
		// (the hub's stall sweeper, lever#34) fails the row even though the
		// container is up — that is the DNS-dead / bad-credential shape.
		{"running, completed", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "completed", LastActivityEvent: now.Add(-3 * time.Minute)}), true, "activity completed, 3m0s ago", ""},
		{"running, working long", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "working", LastActivityEvent: now.Add(-40 * time.Minute)}), true, "activity working, 40m0s ago", ""},
		{"running, no event time", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "waiting_for_input"}), true, "activity waiting_for_input)", ""},
		{"running, stalled", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "stalled", LastActivityEvent: now.Add(-6 * time.Minute)}), false, "stalled", "guest DNS"},
		{"running, crashed", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Up 4 days", Activity: "crashed"}), false, "crashed", "lever attach"},
		{"absent", listing(scion.Agent{Slug: "scratch", Phase: "running", ContainerStatus: "Up 1 second"}), false, "no record", "lever up"},
		{"crashed", listing(scion.Agent{Slug: "assistant", Phase: "error", ContainerStatus: "Exited (1) 3 seconds ago"}), false, `phase "error"`, "--force"},
		{"suspended", listing(scion.Agent{Slug: "assistant", Phase: "suspended", ContainerStatus: "stopped"}), false, `phase "suspended"`, "resume"},
		{"running record, dead container", listing(scion.Agent{Slug: "assistant", Phase: "running", ContainerStatus: "Exited (1) 2 seconds ago"}), false, "Exited (1)", "lever up"},
		// A blank column is "cannot tell", read the same way `lever up` reads it.
		{"running record, blank column", listing(scion.Agent{Slug: "assistant", Phase: "running"}), true, "no container status", ""},
	}
	for _, c := range cases {
		r := checkManagerLive(context.Background(), "/lever", "assistant", c.list, now)
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
	// A listing failure is "not checked", never a pass with a claim attached
	// and never a finding of its own — a down jail is another row's job.
	r := checkManagerLive(context.Background(), "/lever", "assistant", func(context.Context, string) ([]scion.Agent, error) {
		return nil, errors.New("connection refused\nusage: scion list ...")
	}, now)
	if !r.ok || !strings.Contains(r.detail, "not checked") || strings.Contains(r.detail, "usage") {
		t.Fatalf("list error must read as not-checked with only the first line: %+v", r)
	}
}

// TestCheckManagerImage: the manager runs the image its record was created
// with; a resume never moves it, so a changed manager.image reads as healthy
// on the version row alone (lever#33). This row compares the record with the
// config and points at `lever up --fresh`.
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
		{"same", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "scionlocal/lever-claude:arm64"}), true, "scionlocal/lever-claude:arm64", ""},
		{"same, podman-qualified", listing(scion.Agent{Slug: "assistant", Phase: "running", Image: "localhost/scionlocal/lever-claude:arm64"}), true, "", ""},
		{"drifted", listing(scion.Agent{Slug: "assistant", Phase: "suspended", Image: "scionlocal/lever-claude:v1"}), false, "scionlocal/lever-claude:v1", "lever up --fresh"},
		{"no record", listing(scion.Agent{Slug: "scratch", Phase: "running", Image: "x"}), true, "not checked", ""},
		{"record without image", listing(scion.Agent{Slug: "assistant", Phase: "running"}), true, "not checked", ""},
		{"list fails", func(context.Context, string) ([]scion.Agent, error) { return nil, fmt.Errorf("hub down") }, true, "not checked", ""},
	}
	for _, c := range cases {
		r := checkManagerImage(context.Background(), "/lever", "assistant", "scionlocal/lever-claude:arm64", c.list)
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

// The version row's advice must name the verb that actually recreates the
// manager: `lever stop && lever up` resumes the record on its old image.
func TestCheckClaudeVersionAdvisesFresh(t *testing.T) {
	p := doctorProbes{claudeVersion: func(string) (string, error) { return "2.1.240", nil }}
	got := checkClaudeVersion("img", "", p)
	if !strings.Contains(got.detail, "lever up --fresh") || strings.Contains(got.detail, "stop && lever up") {
		t.Fatalf("detail = %q", got.detail)
	}
}

// TestCheckOperatorSkillsMissingTreeFailsWithoutCreating: doctor is
// read-only — a tree that does not exist yet is a failed row pointing at
// `lever init` (which creates it), never created here.
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
	// mountsOf answers by container ref; an absent ref is jail.ErrNoContainer
	// unless it is listed under broken, which is an inspect failure.
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
		// No id in the listing (pin 89ed0fe8): the container is found by scion's name.
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

// lever#34: the guest-DNS row is the one that tells a DNS-dead jail from a
// healthy idle one.
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

	// A resolver that never answers hits the probe's own timeout (exit 124).
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
	r := checkPATTokens(st, false, now)
	if !r.ok {
		t.Fatalf("a current record must pass; got %+v", r)
	}
	if !strings.Contains(r.detail, "expires in") {
		t.Errorf("detail should say when the token expires, got %q", r.detail)
	}
}

// The live case on every instance minted before records existed: no
// agent:message, a 90-day expiry nobody knows about.
func TestCheckPATTokensFailsWithoutARecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := st.SaveControllerPAT("tok"); err != nil {
		t.Fatal(err)
	}
	r := checkPATTokens(st, false, time.Now())
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
	r := checkPATTokens(st, false, time.Now())
	if r.ok || !strings.Contains(r.detail, "scopes") {
		t.Fatalf("drifted scopes must fail and say so; got %+v", r)
	}
}

func TestCheckPATTokensFailsNearExpiry(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "tok")
	now := time.Now()
	if err := st.SaveControllerPATRecord(state.PATRecord{Requested: controllerPATScopes(), ExpiresAt: now.Add(3 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r := checkPATTokens(st, false, now)
	if r.ok || !strings.Contains(r.detail, "expire") {
		t.Fatalf("a token inside the renew window must fail; got %+v", r)
	}
}

func TestCheckPATTokensFailsWhenNoTokenExists(t *testing.T) {
	r := checkPATTokens(state.ForConfig(t.TempDir()), false, time.Now())
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
	r := checkPATTokens(st, true, time.Now())
	if r.ok || !strings.Contains(r.detail, "remote") {
		t.Fatalf("a remote token without a record must fail and name the remote token; got %+v", r)
	}
	if r := checkPATTokens(st, false, time.Now()); !r.ok {
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
// unrolled one, and the doctor must name it as such rather than list it
// among the healthy "slug=full" records.
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

// The ceiling setting landed in the same scion commit as roles (scion#1089),
// so a scion without --role has no ceiling to read: not a finding, and the
// hub is not even asked. A probe that cannot answer is "not checked", never a
// finding either.
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
			r := checkAgentNetwork(context.Background(), "/lever/proj", agents, c.list, c.modes)
			if r.ok != c.ok || !strings.Contains(r.detail, c.wantDetail) || !strings.Contains(r.fix, c.wantFix) {
				t.Fatalf("got ok=%v detail=%q fix=%q; want ok=%v detail~%q fix~%q", r.ok, r.detail, r.fix, c.ok, c.wantDetail, c.wantFix)
			}
		})
	}
}
