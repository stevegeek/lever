package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// svcSpec mirrors the subset of scion's api.ServiceSpec that the renew sidecar
// uses, for parsing the emitted scion-services.yaml back in tests.
type svcSpec struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	Restart string   `yaml:"restart"`
}

func TestWriteRenewServicesAPIKey(t *testing.T) {
	home := t.TempDir()
	bsDir := filepath.Join(home, "ws", ".lever")
	if err := os.MkdirAll(bsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bsDir, "bootstrap.json")
	const brokerURL = "https://host.orb.internal:8443"
	if err := os.WriteFile(bootstrap, []byte(`{"broker_url":"`+brokerURL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	idDir := filepath.Join(home, ".lever-id")
	settings := filepath.Join(home, ".claude", "settings.json")

	if err := writeRenewServices(home, idDir, bootstrap, settings, "api-key"); err != nil {
		t.Fatalf("writeRenewServices: %v", err)
	}

	out := filepath.Join(home, ".scion", "scion-services.yaml")
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read services file: %v", err)
	}
	var specs []svcSpec
	if err := yaml.Unmarshal(b, &specs); err != nil {
		t.Fatalf("parse services yaml: %v", err)
	}
	// Two sidecars: lever-gateway (the loopback mTLS proxy) then lever-renew.
	if len(specs) != 2 {
		t.Fatalf("want 2 services, got %d: %s", len(specs), b)
	}

	// Gateway MUST be emitted first (up before renew) and carry baked absolute
	// flags — sidecars get no CWD, so it must never fall back to a bootstrap path.
	gw := specs[0]
	if gw.Name != "lever-gateway" {
		t.Errorf("specs[0].name = %q, want lever-gateway (emitted first)", gw.Name)
	}
	if gw.Restart != "on-failure" {
		t.Errorf("gateway restart = %q, want on-failure", gw.Restart)
	}
	gwCmd := strings.Join(gw.Command, " ")
	for _, want := range []string{
		"lever-agent gateway",
		"--id-dir " + idDir,
		"--broker-url " + brokerURL, // baked; no sidecar bootstrap file-read
		"--listen 127.0.0.1:8462",
	} {
		if !strings.Contains(gwCmd, want) {
			t.Errorf("gateway command %q missing %q", gwCmd, want)
		}
	}

	s := specs[1]
	if s.Name != "lever-renew" {
		t.Errorf("specs[1].name = %q, want lever-renew", s.Name)
	}
	if s.Restart != "on-failure" {
		t.Errorf("restart = %q, want on-failure", s.Restart)
	}
	cmd := strings.Join(s.Command, " ")
	for _, want := range []string{
		"lever-agent renew --loop",
		"--id-dir " + idDir,
		"--broker-url " + brokerURL, // resolved at boot; no sidecar file-read
		"--llm-auth api-key",
		"--settings " + settings,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q", cmd, want)
		}
	}
}

// TestWriteRenewServicesNoBootstrapIsNoop: a non-brokered agent (no bootstrap
// file) gets no sidecar — there is nothing to renew against.
func TestWriteRenewServicesNoBootstrapIsNoop(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, "ws", ".lever", "bootstrap.json")
	if err := writeRenewServices(home, filepath.Join(home, ".lever-id"), missing, "", "subscription"); err != nil {
		t.Fatalf("writeRenewServices: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "scion-services.yaml")); !os.IsNotExist(err) {
		t.Fatalf("services file should not exist for a non-brokered agent; stat err = %v", err)
	}
}

// TestWriteRenewServicesEmptyBrokerURLIsNoop: a bootstrap that exists but carries
// no broker URL (brokerless) is a distinct path from a missing bootstrap — it too
// gets no sidecar, since there is nothing to renew against.
func TestWriteRenewServicesEmptyBrokerURLIsNoop(t *testing.T) {
	home := t.TempDir()
	bsDir := filepath.Join(home, "ws", ".lever")
	if err := os.MkdirAll(bsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bsDir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte(`{"ticket":"tk","broker_url":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeRenewServices(home, filepath.Join(home, ".lever-id"), bootstrap, "", "api-key"); err != nil {
		t.Fatalf("writeRenewServices: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "scion-services.yaml")); !os.IsNotExist(err) {
		t.Fatalf("services file should not exist for a brokerless bootstrap; stat err = %v", err)
	}
}

func TestWriteClaudeSettingsEnvMergesNotClobbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing settings: an unrelated top-level key + an existing env var.
	if err := os.WriteFile(path, []byte(`{"model":"sonnet","env":{"EXISTING":"keep"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := writeClaudeSettingsEnv(path)
	if err := w(map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_BASE_URL": "http://x/llm"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["model"] != "sonnet" {
		t.Errorf("clobbered unrelated top-level key: model=%v", got["model"])
	}
	env, ok := got["env"].(map[string]any)
	if !ok {
		t.Fatalf("env is not an object: %v", got["env"])
	}
	if env["EXISTING"] != "keep" {
		t.Errorf("clobbered pre-existing env var: EXISTING=%v", env["EXISTING"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" || env["ANTHROPIC_BASE_URL"] != "http://x/llm" {
		t.Errorf("dynamic vars not merged into env: %v", env)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestWriteClaudeSettingsEnvCreatesWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := writeClaudeSettingsEnv(path)(map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	env, _ := got["env"].(map[string]any)
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Fatalf("absent-file case did not create env block: %v", got)
	}
}

func TestWriteClaudeSettingsEnvEmptyPathNoop(t *testing.T) {
	if err := writeClaudeSettingsEnv("")(map[string]string{"X": "y"}); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
}

func TestCLICapabilityVerbsValidateArgsBeforeAnythingElse(t *testing.T) {
	// lever-agent sits on $PATH inside every agent jail, so `delegate` here is
	// the same mint path the capability MCP tool exposes — and it had the same
	// defect: an absent -to sent an empty bind target, which the broker defaults
	// to the caller, printing a SELF-bound token as a success. The checks must
	// also run BEFORE the identity load, so a bad invocation reports the bad
	// argument rather than whatever unrelated thing fails first.
	missingID := filepath.Join(t.TempDir(), "no-such-id-dir")
	for _, tc := range []struct {
		name, verb, want string
		args             []string
	}{
		{"delegate without -to", "delegate", `"-to"`, []string{"-tool", "db", "-op", "read"}},
		{"delegate blank -to", "delegate", `"-to"`, []string{"-tool", "db", "-op", "read", "-to", "  "}},
		// A positional `to=worker` is swallowed as a CONSTRAINT while -to stays
		// empty — the CLI shape of the misspelt-argument bug.
		{"delegate with positional to=", "delegate", `"-to"`, []string{"-tool", "db", "-op", "read", "to=worker"}},
		{"delegate without -tool", "delegate", `"-tool"`, []string{"-op", "read", "-to", "worker"}},
		{"request without -tool", "request", `"-tool"`, []string{"-op", "read"}},
		{"request without -op", "request", `"-op"`, []string{"-tool", "db"}},
	} {
		err := cmdCLI(tc.verb, append([]string{"-id-dir", missingID}, tc.args...))
		if err == nil {
			t.Fatalf("%s: must error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %q, want it to name %s", tc.name, err, tc.want)
		}
		if strings.Contains(err.Error(), "no identity") {
			t.Fatalf("%s: argument checks must run before the identity load, got %q", tc.name, err)
		}
	}
}

func TestUnknownSubcommandErrors(t *testing.T) {
	if err := run([]string{"lever-agent", "bogus"}); err == nil {
		t.Fatal("unknown subcommand must error")
	}
}

func TestRunRequiresSubcommand(t *testing.T) {
	if err := run([]string{"lever-agent"}); err == nil {
		t.Fatal("missing subcommand must error")
	}
}

// TestBuildToolCallBody verifies that the JSON-RPC body produced for the
// gateway satisfies the contract expected by internal/broker/mcp.go:toolsCallFields:
//   - jsonrpc == "2.0", method == "tools/call"
//   - params.name == op
//   - params.arguments._capability == token
//   - extra kv args appear in params.arguments
func TestBuildToolCallBody(t *testing.T) {
	const op = "query"
	const tok = "tok_abc123"
	extra := map[string]string{"table": "users", "limit": "10"}

	body := buildToolCallBody(op, tok, extra)

	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := msg["jsonrpc"]; got != "2.0" {
		t.Errorf("jsonrpc: got %v, want 2.0", got)
	}
	if got := msg["method"]; got != "tools/call" {
		t.Errorf("method: got %v, want tools/call", got)
	}

	params, ok := msg["params"].(map[string]any)
	if !ok {
		t.Fatal("params missing or wrong type")
	}
	if got := params["name"]; got != op {
		t.Errorf("params.name: got %v, want %q", got, op)
	}

	args, ok := params["arguments"].(map[string]any)
	if !ok {
		t.Fatal("params.arguments missing or wrong type")
	}
	if got := args["_capability"]; got != tok {
		t.Errorf("arguments._capability: got %v, want %q", got, tok)
	}
	if got := args["table"]; got != "users" {
		t.Errorf("arguments.table: got %v, want users", got)
	}
	if got := args["limit"]; got != "10" {
		t.Errorf("arguments.limit: got %v, want 10", got)
	}
}

// TestRenewFlagAcceptance verifies that the renew flagset accepts --loop and
// --interval without a parse error (reconciles manifest.json sidecar declaration).
func TestRenewFlagAcceptance(t *testing.T) {
	fs := flag.NewFlagSet("renew", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	fs.String("id-dir", defaultIDDir, "")
	fs.String("broker-url", "", "")
	fs.String("bootstrap", "", "")
	loop := fs.Bool("loop", false, "")
	interval := fs.Duration("interval", 12*time.Hour, "")

	if err := fs.Parse([]string{"--loop", "--interval", "6h"}); err != nil {
		t.Fatalf("flag parse error (manifest sidecar would crash): %v", err)
	}
	if !*loop {
		t.Error("--loop should be true after parse")
	}
	if *interval != 6*time.Hour {
		t.Errorf("--interval: got %v, want 6h", *interval)
	}
}

// TestRenewOnceNoIdentityErrors verifies that renewOnce returns an error (not a
// panic or hang) when no identity exists in the directory.
func TestRenewOnceNoIdentityErrors(t *testing.T) {
	tmp := t.TempDir()
	err := renewOnce(renewOpts{idDir: tmp})
	if err == nil {
		t.Fatal("renewOnce with empty dir must return an error")
	}
	if !strings.Contains(err.Error(), "no identity") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRenewNonLoopReturnsErrorImmediately verifies that run with renew (no
// --loop) returns immediately with an error for an empty id-dir (no hang).
func TestRenewNonLoopReturnsErrorImmediately(t *testing.T) {
	tmp := t.TempDir()
	err := run([]string{"lever-agent", "renew", "--id-dir", tmp})
	if err == nil {
		t.Fatal("renew with no identity must error")
	}
	if !strings.Contains(err.Error(), "no identity") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestRenewLoopFlagsAcceptedByRealCmd exercises the REAL dispatch path through
// run() to prove that cmdRenew accepts --loop and --interval without a
// "flag provided but not defined" parse error. Loop mode only exits on
// SIGINT/SIGTERM, so we send SIGINT to ourselves after a brief delay to unblock
// it. The test asserts that any returned error is NOT a flag-parse error (an
// "no identity" or nil return both indicate the flags were accepted).
func TestRenewLoopFlagsAcceptedByRealCmd(t *testing.T) {
	tmp := t.TempDir()

	// Send SIGINT to ourselves after 50ms to unblock the loop.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	err := run([]string{"lever-agent", "renew", "--id-dir", tmp, "--loop", "--interval", "24h"})
	// Loop mode exits cleanly (nil) on signal. Either way, the error must NOT be
	// a flag-parse error — that would mean cmdRenew doesn't define --loop/--interval.
	if err != nil && (strings.Contains(err.Error(), "flag provided but not defined") ||
		strings.Contains(err.Error(), "flag: help requested")) {
		t.Fatalf("real cmdRenew rejected --loop/--interval (manifest sidecar would break): %v", err)
	}
}

// TestProvisionVerbAcceptedByRun verifies that run() dispatches "provision" and
// that the provision flags parse correctly. It uses a temp dir as -id-dir so there
// is no identity — cmdProvision errors with "no identity", which proves dispatch
// and flag parsing succeeded without a "flag provided but not defined" error.
func TestProvisionVerbAcceptedByRun(t *testing.T) {
	err := run([]string{"lever-agent", "provision", "-worker", "worker", "-out", t.TempDir() + "/w.json", "-id-dir", t.TempDir()})
	if err == nil {
		t.Fatal("expected an error (no identity), got nil")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("provision flags must parse: %v", err)
	}
}

// TestBuildToolCallBodyEmptyArgs verifies token-only calls (no extra kv pairs).
func TestBuildToolCallBodyEmptyArgs(t *testing.T) {
	body := buildToolCallBody("op", "mytoken", nil)
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	params := msg["params"].(map[string]any)
	args := params["arguments"].(map[string]any)
	if got := args["_capability"]; got != "mytoken" {
		t.Errorf("arguments._capability: got %v, want mytoken", got)
	}
	// Only _capability should be present
	if len(args) != 1 {
		t.Errorf("expected 1 argument (only _capability), got %d: %v", len(args), args)
	}
}

// TestMCPAddArgsUsesUserScope pins the fix for the worker/manager MCP-wiring gap:
// the pre-start hook runs `claude mcp add` from the agent home, but the claude
// session runs in /workspace. `claude mcp add`'s default (local) scope is
// keyed by CWD, so servers registered from the home are invisible to the
// session. --scope user makes them global (CWD-independent) — required for both
// the http broker tools and the stdio capability server.
func TestMCPAddArgsUsesUserScope(t *testing.T) {
	got := mcpAddArgs("db", []string{"--transport", "http", "https://broker/mcp/db/"})
	want := []string{"mcp", "add", "--scope", "user", "db", "--transport", "http", "https://broker/mcp/db/"}
	if !slices.Equal(got, want) {
		t.Fatalf("http tool args = %v, want %v", got, want)
	}
	// The stdio capability server must also be user-scoped.
	cap := mcpAddArgs("lever-capability", []string{"lever-agent", "serve-capability"})
	if strings.Join(cap[:4], " ") != "mcp add --scope user" {
		t.Fatalf("capability server args must lead with `mcp add --scope user`, got %v", cap)
	}
	if cap[4] != "lever-capability" {
		t.Fatalf("scope must precede the server name, got %v", cap)
	}
}

// TestMCPRemoveArgsUserScope pins the remove targets the same user scope as add.
func TestMCPRemoveArgsUserScope(t *testing.T) {
	got := mcpRemoveArgs("db")
	want := []string{"mcp", "remove", "--scope", "user", "db"}
	if !slices.Equal(got, want) {
		t.Fatalf("mcpRemoveArgs = %v, want %v", got, want)
	}
}

// TestClaudeMCPAddIsIdempotent verifies claudeMCPAdd removes before adding and
// ignores a failing remove (absent server), so a re-boot (scion resume) can't
// fail the pre-start hook on "already exists".
func TestClaudeMCPAddIsIdempotent(t *testing.T) {
	var calls [][]string
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 1 && args[1] == "remove" {
			return []byte("No MCP server named \"db\""), errors.New("exit status 1") // absent → non-zero
		}
		return nil, nil
	}
	if err := claudeMCPAdd("db", "--transport", "http", "https://broker/mcp/db/"); err != nil {
		t.Fatalf("a failing remove must be ignored; claudeMCPAdd returned %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want remove then add (2 calls), got %d: %v", len(calls), calls)
	}
	if calls[0][2] != "remove" || calls[1][2] != "add" {
		t.Fatalf("must remove before add; got %v then %v", calls[0], calls[1])
	}
	// remove: [claude mcp remove --scope user db]; add: [claude mcp add --scope user db --transport …]
	if calls[0][5] != "db" || calls[1][5] != "db" {
		t.Fatalf("both must target the same server name; got %v / %v", calls[0], calls[1])
	}
}

// TestClaudeMCPAddSurfacesAddError: a failing ADD (not remove) must surface.
func TestClaudeMCPAddSurfacesAddError(t *testing.T) {
	orig := runCommand
	defer func() { runCommand = orig }()
	runCommand = func(name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "add" {
			return []byte("boom"), errors.New("exit status 1")
		}
		return nil, nil
	}
	if err := claudeMCPAdd("db", "--transport", "http", "u"); err == nil {
		t.Fatal("a failing add must return an error")
	}
}
