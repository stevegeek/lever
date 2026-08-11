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

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
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

func TestCheckToolBackendsNoneDeclared(t *testing.T) {
	r := checkToolBackends(nil, failDial)
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
	r := checkToolBackends(tools, okDial)
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
	r := checkToolBackends(tools, dial)
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
	got := checkToolBackends(tools, func(string) error { return nil })
	if got.ok {
		t.Fatalf("supervised tool with missing binary should fail the check")
	}
}

func TestCheckToolBackendsExternalDown(t *testing.T) {
	tools := []config.Tool{{Name: "x", External: true, Backend: "127.0.0.1:59999"}}
	got := checkToolBackends(tools, func(string) error { return fmt.Errorf("refused") })
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
	failDial := func(string) error { return fmt.Errorf("refused") }
	tools := []config.Tool{{Name: "db", Command: []string{"true"}, Backend: "127.0.0.1:59999"}}
	if got := checkToolBackends(tools, failDial); !got.ok {
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
	got := checkToolBackends(tools, func(string) error { return nil })
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
	if hubProjectKey("/lever") != filepath.Base(jailProjectPath("/anything", "/lever")) {
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
// signersRel is "". Mirrors apply_test.go's writeTmpConfig / config_test.go's
// writeTmp.
func writeDirectivesConfig(t *testing.T, signersRel string) *config.App {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, config.CanonicalName)
	body := "name: demo\nbackend: orbstack\ntree: ws\nbroker:\n  llm_auth: subscription\n"
	if signersRel != "" {
		body += "operator:\n  allowed_signers: " + signersRel + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return app
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

func TestCheckClaudeVersion(t *testing.T) {
	saved := claudeVersionProbe
	t.Cleanup(func() { claudeVersionProbe = saved })

	claudeVersionProbe = func(string) (string, error) { return "2.1.207", nil }
	if got := checkClaudeVersion("img", claudeVersionProbe); !got.ok || !strings.Contains(got.detail, "2.1.207") {
		t.Fatalf("expected pass reporting the version, got %+v", got)
	}

	// Missing label (older image) → informational pass, not a hard fail.
	claudeVersionProbe = func(string) (string, error) { return "", nil }
	if got := checkClaudeVersion("img", claudeVersionProbe); !got.ok {
		t.Fatalf("missing label should be informational, not a failure, got %+v", got)
	}

	// Inspect error → fail with actionable fix.
	claudeVersionProbe = func(string) (string, error) { return "", fmt.Errorf("no such image") }
	if got := checkClaudeVersion("img", claudeVersionProbe); got.ok {
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
