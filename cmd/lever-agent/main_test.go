package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/httpjson"
)

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
		if errors.Is(err, errNoIdentity) {
			t.Fatalf("%s: argument checks must run before the identity load, got %q", tc.name, err)
		}
	}
}

// isFlagParseError reports whether err is the flag package's own rejection of
// an argv: an undefined flag, or -h. Those have no sentinel in package flag
// except flag.ErrHelp, so the undefined-flag case is matched on its wording.
func isFlagParseError(err error) bool {
	return err != nil && (errors.Is(err, flag.ErrHelp) ||
		strings.Contains(err.Error(), "flag provided but not defined"))
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

// TestRenewOnceNoIdentityErrors verifies that renewOnce returns an error (not a
// panic or hang) when no identity exists in the directory.
func TestRenewOnceNoIdentityErrors(t *testing.T) {
	tmp := t.TempDir()
	err := renewOnce(context.Background(), renewOpts{idDir: tmp})
	if err == nil {
		t.Fatal("renewOnce with empty dir must return an error")
	}
	if !errors.Is(err, errNoIdentity) {
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
	if !errors.Is(err, errNoIdentity) {
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
	if isFlagParseError(err) {
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
	if isFlagParseError(err) {
		t.Fatalf("provision flags must parse: %v", err)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns whatever
// fn printed. Small outputs only (the pipe buffer is not drained concurrently).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// callVerbWorkerID writes a loadable worker identity (a CA-signed leaf) to a
// temp dir and returns it. The `call` verb only needs id.Client() to build its
// mTLS client; the server it dials is a plain-HTTP stub.
func callVerbWorkerID(t *testing.T) string {
	t.Helper()
	caInst, err := ca.Generate()
	if err != nil {
		t.Fatal(err)
	}
	csrPEM, keyPEM, err := agent.GenerateCSR("worker")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	idDir := t.TempDir()
	id := agent.Identity{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caInst.CertPEM()}
	if err := id.Write(idDir); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	return idDir
}

// TestCallVerbSemantics pins the wire + I/O contract of the `call` verb, whose
// only prior coverage was the live acceptance harness (not run by `go test`):
// it POSTs a JSON-RPC tools/call to /mcp/<tool>/ with Content-Type
// application/json, the token in arguments._capability, prints the raw response
// body to stdout EVEN on a non-200, and surfaces a non-200 as an error carrying
// the status.
func TestCallVerbSemantics(t *testing.T) {
	idDir := callVerbWorkerID(t)

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"success", http.StatusOK, `{"result":"ok"}`},
		{"denied", http.StatusForbidden, `{"error":"denied"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotCT, gotBody string
			// Plain-HTTP stub: id.Client's mTLS transport dials http:// fine
			// (its TLS config is inert for a non-TLS scheme).
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotCT = r.Header.Get("Content-Type")
				var b bytes.Buffer
				b.ReadFrom(r.Body)
				gotBody = b.String()
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer stub.Close()

			var err error
			out := captureStdout(t, func() {
				err = cmdCLI("call", []string{
					"-id-dir", idDir,
					"-broker-url", stub.URL,
					"-tool", "db",
					"-op", "read",
					"-token", "tok_xyz",
					"table=users",
				})
			})

			if gotPath != "/mcp/db/" {
				t.Errorf("POST path = %q, want /mcp/db/", gotPath)
			}
			if gotCT != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotCT)
			}
			if !strings.Contains(gotBody, `"_capability":"tok_xyz"`) {
				t.Errorf("request body missing _capability token: %s", gotBody)
			}
			if !strings.Contains(gotBody, `"table":"users"`) {
				t.Errorf("request body missing constraint arg: %s", gotBody)
			}
			// Body is printed to stdout regardless of status.
			if !strings.Contains(out, tc.body) {
				t.Errorf("stdout = %q, want it to contain response body %q", out, tc.body)
			}
			if tc.status == http.StatusOK {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else if httpjson.Status(err) != tc.status {
				t.Errorf("error = %v, want one carrying status %d", err, tc.status)
			}
		})
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

// TestClaudeMCPAddIsIdempotent verifies claudeMCP.Add removes before adding and
// ignores a failing remove (absent server), so a re-boot (scion resume) can't
// fail the pre-start hook on "already exists".
func TestClaudeMCPAddIsIdempotent(t *testing.T) {
	var calls [][]string
	c := claudeMCP{run: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 1 && args[1] == "remove" {
			return []byte("No MCP server named \"db\""), errors.New("exit status 1") // absent → non-zero
		}
		return nil, nil
	}}
	if err := c.Add("db", "--transport", "http", "https://broker/mcp/db/"); err != nil {
		t.Fatalf("a failing remove must be ignored; claudeMCP.Add returned %v", err)
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
	c := claudeMCP{run: func(name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "add" {
			return []byte("boom"), errors.New("exit status 1")
		}
		return nil, nil
	}}
	if err := c.Add("db", "--transport", "http", "u"); err == nil {
		t.Fatal("a failing add must return an error")
	}
}

// TestToolsFlagDistinguishesOmittedFromEmpty pins the -tools contract: omitted
// means auto-discover (set=false); "-tools ”" is an explicit empty list
// (set=true, no tools); a CSV is trimmed and blanks dropped.
func TestToolsFlagDistinguishesOmittedFromEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantSet bool
		want    []string
	}{
		{"omitted", nil, false, nil},
		{"explicit empty", []string{"-tools", ""}, true, nil},
		{"csv", []string{"-tools", " db, ,mail "}, true, []string{"db", "mail"}},
	} {
		fs := flag.NewFlagSet("boot", flag.ContinueOnError)
		var tools toolsFlag
		fs.Var(&tools, "tools", "")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tools.set != tc.wantSet || !slices.Equal(tools.tools, tc.want) {
			t.Errorf("%s: set=%v tools=%v, want set=%v tools=%v", tc.name, tools.set, tools.tools, tc.wantSet, tc.want)
		}
	}
}

// TestCommonFlagsDefinesTrio pins the id-dir/broker-url/bootstrap trio that
// commonFlags registers for the five lazy-resolving subcommands: flag names, the
// per-command-configurable help strings, and the defaults (id-dir →
// $HOME/.lever-id, the other two empty). D3 collapses five hand-rolled copies
// onto this one helper, so this is the guard for that single source of truth.
func TestCommonFlagsDefinesTrio(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	idDir, brokerURL, bootstrap := commonFlags(fs, "id help", "bootstrap help")

	wantIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	if *idDir != wantIDDir {
		t.Errorf("id-dir default: got %q, want %q", *idDir, wantIDDir)
	}
	if *brokerURL != "" {
		t.Errorf("broker-url default: got %q, want empty", *brokerURL)
	}
	if *bootstrap != "" {
		t.Errorf("bootstrap default: got %q, want empty", *bootstrap)
	}

	for _, tc := range []struct {
		name, usage string
	}{
		{"id-dir", "id help"},
		{"broker-url", "broker URL (overrides bootstrap)"},
		{"bootstrap", "bootstrap help"},
	} {
		f := fs.Lookup(tc.name)
		if f == nil {
			t.Errorf("flag %q not defined", tc.name)
			continue
		}
		if f.Usage != tc.usage {
			t.Errorf("flag %q usage: got %q, want %q", tc.name, f.Usage, tc.usage)
		}
	}
}

// TestResolveBrokerURLFallbackChain pins the three-step resolution order of
// resolveBrokerURL: an explicit --broker-url wins outright without touching any
// file; otherwise the broker URL is read from a bootstrap file located by
// --bootstrap, then $LEVER_BOOTSTRAP, then ./.lever/bootstrap.json. D3 folds the
// path-resolution half into bootstrapPathOrDefault, so this guards the merge —
// neither resolveBrokerURL nor bootstrapPathOrDefault had a test before.
func TestResolveBrokerURLFallbackChain(t *testing.T) {
	writeBootstrap := func(t *testing.T, dir, url string) string {
		t.Helper()
		p := filepath.Join(dir, "bootstrap.json")
		data, err := json.Marshal(agent.Bootstrap{BrokerURL: url})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("explicit broker-url wins without reading any file", func(t *testing.T) {
		// $LEVER_BOOTSTRAP points at a nonexistent file; if it were consulted the
		// call would error. It must not be — the explicit flag short-circuits.
		t.Setenv("LEVER_BOOTSTRAP", filepath.Join(t.TempDir(), "does-not-exist.json"))
		got, err := resolveBrokerURL("https://explicit.example", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://explicit.example" {
			t.Errorf("got %q, want the explicit flag value", got)
		}
	})

	t.Run("bootstrap flag path is read when broker-url is empty", func(t *testing.T) {
		// env points elsewhere; the explicit --bootstrap path must take precedence.
		t.Setenv("LEVER_BOOTSTRAP", filepath.Join(t.TempDir(), "wrong.json"))
		p := writeBootstrap(t, t.TempDir(), "https://from-flag.example")
		got, err := resolveBrokerURL("", p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://from-flag.example" {
			t.Errorf("got %q, want the URL from the --bootstrap file", got)
		}
	})

	t.Run("LEVER_BOOTSTRAP is used when both flags are empty", func(t *testing.T) {
		p := writeBootstrap(t, t.TempDir(), "https://from-env.example")
		t.Setenv("LEVER_BOOTSTRAP", p)
		got, err := resolveBrokerURL("", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://from-env.example" {
			t.Errorf("got %q, want the URL from $LEVER_BOOTSTRAP", got)
		}
	})

	t.Run("defaults to ./.lever/bootstrap.json when nothing is set", func(t *testing.T) {
		t.Setenv("LEVER_BOOTSTRAP", "")
		dir := t.TempDir()
		leverDir := filepath.Join(dir, ".lever")
		if err := os.MkdirAll(leverDir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(agent.Bootstrap{BrokerURL: "https://from-default.example"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(leverDir, "bootstrap.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		got, err := resolveBrokerURL("", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://from-default.example" {
			t.Errorf("got %q, want the URL from ./.lever/bootstrap.json", got)
		}
	})
}
