// Command lever-agent is the in-jail capability helper: it enrols the agent's
// mTLS identity (key generated in-container, never leaves), serves the capability
// MCP tool the LLM drives, renews before expiry, and (via CLI verbs) lets the
// acceptance harness mint/delegate/exercise deterministically.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/agent"
	"gopkg.in/yaml.v3"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "lever-agent:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) < 2 {
		return fmt.Errorf("usage: lever-agent <boot|serve-capability|renew|provision|request|delegate|call> ...")
	}
	switch argv[1] {
	case "boot":
		return cmdBoot(argv[2:])
	case "serve-capability":
		return cmdServeCapability(argv[2:])
	case "renew":
		return cmdRenew(argv[2:])
	case "provision":
		return cmdProvision(argv[2:])
	case "request", "delegate", "call":
		return cmdCLI(argv[1], argv[2:])
	default:
		return fmt.Errorf("unknown subcommand %q", argv[1])
	}
}

// cmdBoot wires the real claude-mcp-add exec + the claude settings.json env-block
// writer into agent.Boot. Flags: -bootstrap (default $LEVER_BOOTSTRAP or
// ./.lever/bootstrap.json), -id-dir (default $HOME/.lever-id), -settings (claude
// settings.json path; its env block receives the dynamic LLM vars),
// -tools (comma-separated broker tool names).
func cmdBoot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ContinueOnError)
	defaultBootstrap := os.Getenv("LEVER_BOOTSTRAP")
	if defaultBootstrap == "" {
		defaultBootstrap = "./.lever/bootstrap.json"
	}
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	bootstrapPath := fs.String("bootstrap", defaultBootstrap, "path to bootstrap.json")
	idDir := fs.String("id-dir", defaultIDDir, "directory for the agent identity (cert+key+ca)")
	settingsPath := fs.String("settings", "", "path to the claude settings.json whose env block receives ANTHROPIC_AUTH_TOKEN/BASE_URL (api-key mode)")
	toolsCSV := fs.String("tools", "", "comma-separated broker tool names to register via claude mcp add")
	llmAuth := fs.String("llm-auth", "subscription", "LLM auth mode: 'api-key' obtains a capability(llm) token and writes ANTHROPIC_AUTH_TOKEN/BASE_URL into the claude settings.json env block; 'subscription' (default) leaves those keys absent and uses the user's own key")
	enrolOnly := fs.Bool("enrol-only", false, "enrol + write the identity only; skip the claude mcp registration and env overlay (no `claude` binary required — used by the VM-level acceptance gate, which drives lever-agent's CLI verbs directly)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Detect whether -tools was explicitly set (even to "") via fs.Visit.
	// This distinguishes "flag omitted" (auto-discover) from "-tools ''" (explicit empty list).
	toolsSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "tools" {
			toolsSet = true
		}
	})
	var brokerTools []string
	if toolsSet {
		// Explicit -tools value: parse the CSV (may be empty, yielding nil slice).
		for _, t := range strings.Split(*toolsCSV, ",") {
			if t = strings.TrimSpace(t); t != "" {
				brokerTools = append(brokerTools, t)
			}
		}
	}
	ctx := context.Background()
	cfg := agent.BootConfig{
		BootstrapPath:   *bootstrapPath,
		IDDir:           *idDir,
		BrokerTools:     brokerTools,
		MCPAdd:          claudeMCPAdd,
		WriteEnvOverlay: writeClaudeSettingsEnv(*settingsPath),
		LLMAuth:         *llmAuth,
	}
	// Auto-discover tools from the broker only when -tools was not explicitly set.
	// When -tools is set (even to ""), the explicit list wins and discovery is skipped.
	if !toolsSet {
		cfg.ListTools = agent.ListTools
	}
	if *enrolOnly {
		// Enrol + write identity only: nil hooks make agent.Boot skip the env
		// overlay and the `claude mcp add` registration (no claude in the bare VM).
		cfg.MCPAdd = nil
		cfg.WriteEnvOverlay = nil
		cfg.BrokerTools = nil
		cfg.ListTools = nil
		cfg.LLMAuth = ""
	} else if *llmAuth == "api-key" {
		// Wire the real requestLLMToken only in non-enrol-only mode and when api-key
		// is requested; enrol-only mode skips overlay writing entirely.
		cfg.RequestLLMToken = requestLLMToken
	}
	if err := agent.Boot(ctx, cfg); err != nil {
		return err
	}
	// G2: emit the renew sidecar so scion auto-refreshes the cert and (in api-key
	// mode) the LLM token. Skip in enrol-only mode — the bare VM acceptance gate
	// has no long-running container to run sidecars in.
	if !*enrolOnly {
		if err := writeRenewServices(os.Getenv("HOME"), *idDir, *bootstrapPath, *settingsPath, *llmAuth); err != nil {
			return fmt.Errorf("emit renew sidecar: %w", err)
		}
	}
	return nil
}

// requestLLMToken obtains a capability(llm) token from the broker /request
// endpoint over mTLS. The broker returns {"token":"<base64url>"} — the token
// field is already base64url-encoded (broker does RawURLEncoding.EncodeToString
// server-side), so we return it verbatim. Do NOT re-encode. The proxy's
// bearerToken does RawURLEncoding.DecodeString on the value after "Bearer ",
// so ANTHROPIC_AUTH_TOKEN must equal cr.Token exactly.
func requestLLMToken(ctx context.Context, brokerURL string, client *http.Client, cn string) (string, error) {
	body, _ := json.Marshal(map[string]string{"tool": "llm", "op": "generate", "bound_to": cn})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(brokerURL, "/")+"/request", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("broker /request: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var cr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("broker /request: decode: %w", err)
	}
	if cr.Token == "" {
		return "", fmt.Errorf("broker /request: empty token in response")
	}
	return cr.Token, nil // already base64url-encoded; return verbatim
}

// mcpAddArgs builds the `claude mcp add` argv. It forces --scope user (global,
// not the default local scope which is keyed by the current working directory):
// the pre-start hook runs boot from the agent HOME, but the claude session runs
// in /workspace, so a CWD-scoped registration would be invisible to the session.
// User scope makes every brokered tool + the stdio capability server reachable
// regardless of where claude is launched. --scope precedes the server name.
func mcpAddArgs(name string, argv []string) []string {
	return append([]string{"mcp", "add", "--scope", "user", name}, argv...)
}

// mcpRemoveArgs builds the `claude mcp remove` argv for the same (user) scope
// claudeMCPAdd writes to, so a re-registration first clears the prior entry.
func mcpRemoveArgs(name string) []string {
	return []string{"mcp", "remove", "--scope", "user", name}
}

// runCommand is the exec seam (overridden in tests) so claudeMCPAdd's
// remove-then-add ordering and error handling are testable without a real
// `claude` binary.
var runCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// claudeMCPAdd registers an MCP server, idempotently. `claude mcp add` exits
// non-zero if the server already exists, and the scion pre-start hook runs boot
// under `set -eu` on every container start — so on a resume (same persistent
// /home/scion), an unconditional add would fail the hook and block bring-up.
// Removing first (ignoring "no such server", which also exits non-zero) makes it
// a clean upsert regardless of prior state.
func claudeMCPAdd(name string, argv ...string) error {
	_, _ = runCommand("claude", mcpRemoveArgs(name)...) // ignore: absent is fine
	out, err := runCommand("claude", mcpAddArgs(name, argv)...)
	if err != nil {
		return fmt.Errorf("claude mcp add %s: %w: %s", name, err, out)
	}
	return nil
}

// writeClaudeSettingsEnv returns a writer that MERGES the given env vars into the
// `env` block of the claude settings.json at path, preserving any existing
// settings and env keys (read-modify-write). This is how the dynamic
// ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL reach the in-container `claude`:
// Claude Code natively reads its settings.json `env` block at startup (verified
// live 2026-06-28), whereas the scion harness env-overlay path is inert for our
// builtin harness. Empty path is a no-op (e.g. enrol-only). Written 0600.
func writeClaudeSettingsEnv(path string) func(map[string]string) error {
	return func(m map[string]string) error {
		if path == "" {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("settings dir %s: %w", filepath.Dir(path), err)
		}
		// Merge into existing settings rather than clobber (claude may already have
		// written model/permissions/etc; mcp config lives in a separate ~/.claude.json).
		settings := map[string]any{}
		if b, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(b, &settings); err != nil {
				return fmt.Errorf("settings %s: parse existing: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("settings %s: read: %w", path, err)
		}
		env, _ := settings["env"].(map[string]any)
		if env == nil {
			env = map[string]any{}
		}
		for k, v := range m {
			env[k] = v
		}
		settings["env"] = env
		b, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, b, 0o600)
	}
}

// renewServiceSpec mirrors the subset of scion's api.ServiceSpec we emit (scion
// unmarshals scion-services.yaml into []api.ServiceSpec). Local copy to avoid a
// dependency on the vendored scion module; the yaml keys must match.
type renewServiceSpec struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	Restart string   `yaml:"restart"`
}

// writeRenewServices emits $HOME/.scion/scion-services.yaml describing the
// lever-renew sidecar, so scion launches in-container auto-refresh of the agent
// cert and (in api-key mode) the LLM capability token right after the pre-start
// hooks. scion reads this file as []api.ServiceSpec and launches each entry as
// the agent user with the container env inherited (so the projected
// LEVER_LLM_AUTH still flows), but it does NOT set the sidecar's working
// directory — so every path here is made absolute and the broker URL is resolved
// from an explicit --bootstrap rather than renew's CWD-relative default.
//
// The broker URL is resolved here from the bootstrap and baked into the sidecar
// as --broker-url, so the sidecar never reads the bootstrap file at all: it
// avoids re-touching the one-time enrolment ticket the bootstrap also carries,
// and removes any dependency on the sidecar's uid/CWD matching the bootstrap's
// perms (the sidecar runs as the agent user; the bootstrap is 0600). No-op when
// the bootstrap is absent or carries no broker URL: a non-brokered agent has
// nothing to renew against. The renew loop self-heals transient errors (logged,
// loop continues); restart:on-failure covers a hard crash.
//
// Tamper window: this file sits at $HOME/.scion/ under the agent-writable
// /home/scion bind-mount, so a compromised agent could rewrite it. That grants
// no escalation — scion launches the sidecar at the agent's OWN uid (nothing it
// couldn't already exec), and boot runs inside the pre-start hook, which scion
// runs BEFORE it reads scion-services.yaml, so this write always wins over any
// pre-seeded value before the spec is consumed.
func writeRenewServices(homeDir, idDir, bootstrapPath, settingsPath, llmAuth string) error {
	bs, err := agent.LoadBootstrap(bootstrapPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no bootstrap deposited — non-brokered agent, emit no sidecar
		}
		return fmt.Errorf("load bootstrap: %w", err)
	}
	if bs.BrokerURL == "" {
		return nil // brokerless bootstrap — nothing to renew against
	}
	command := []string{
		"lever-agent", "renew", "--loop",
		"--id-dir", idDir,
		"--broker-url", bs.BrokerURL,
		"--llm-auth", llmAuth,
		"--settings", settingsPath,
	}
	specs := []renewServiceSpec{{Name: "lever-renew", Command: command, Restart: "on-failure"}}
	b, err := yaml.Marshal(specs)
	if err != nil {
		return err
	}
	dir := filepath.Join(homeDir, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("scion dir %s: %w", dir, err)
	}
	return os.WriteFile(filepath.Join(dir, "scion-services.yaml"), b, 0o644)
}

// cmdServeCapability serves the capability MCP server over stdio using
// line-delimited JSON-RPC 2.0. This pairs with boot.go's:
//
//	MCPAdd("lever-capability", "lever-agent", "serve-capability")
//
// which registers this binary as an MCP command-mode server. Claude Code
// (and the acceptance harness) spawns "lever-agent serve-capability" and
// communicates via stdin/stdout JSON-RPC.
//
// TRANSPORT DECISION: agent.NewMCPServer().Handler() is an http.Handler.
// We bridge it here using a stdio JSON-RPC loop: each line from stdin is a
// JSON-RPC object; we pass it through the handler via httptest.ResponseRecorder
// and write the result JSON line to stdout. This avoids opening a real TCP port
// inside the jail (no port allocation, no cross-container TLS needed for the MCP
// channel). The tradeoff is that the bridge is not streaming — it batches each
// JSON-RPC object as one line and replies synchronously. For the capability tool
// (request/delegate) this is fine; revisit if the MCP session needs
// notifications.
//
// PAIRING WITH BOOT.GO: boot.go calls MCPAdd("lever-capability", "lever-agent",
// "serve-capability"). Claude Code interprets this as a command-mode MCP server
// and spawns "lever-agent serve-capability" reading/writing stdio JSON-RPC. Task 8
// validates this transport live in the acceptance run.
func cmdServeCapability(args []string) error {
	fs := flag.NewFlagSet("serve-capability", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	idDir := fs.String("id-dir", defaultIDDir, "directory for the agent identity")
	brokerURL := fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	bootstrapPath := fs.String("bootstrap", "", "path to bootstrap.json (for broker URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, ok := agent.LoadIdentity(*idDir)
	if !ok {
		return fmt.Errorf("serve-capability: no identity in %s — run 'lever-agent boot' first", *idDir)
	}
	bURL, err := resolveBrokerURL(*brokerURL, *bootstrapPath)
	if err != nil {
		return fmt.Errorf("serve-capability: %w", err)
	}
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("serve-capability: build mTLS client: %w", err)
	}
	agentCN, err := leafCN(id.CertPEM)
	if err != nil {
		return fmt.Errorf("serve-capability: %w", err)
	}
	srv := agent.NewMCPServer(agent.MCPConfig{
		BrokerURL: bURL,
		AgentCN:   agentCN,
		Client:    client,
	})
	handler := srv.Handler()

	// Stdio JSON-RPC bridge: read one JSON object per line from stdin, pass it
	// through the http.Handler via httptest.ResponseRecorder, write the response
	// JSON line to stdout.
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec := httptest.NewRecorder()
		req, err := http.NewRequest("POST", "/", bytes.NewReader(line))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec, req)
		out := bytes.TrimSpace(rec.Body.Bytes())
		if len(out) > 0 {
			fmt.Fprintf(os.Stdout, "%s\n", out)
		}
	}
	return scanner.Err()
}

// renewOpts collects the parameters for renewOnce.
type renewOpts struct {
	idDir, brokerURL, bootstrapPath string
	// llmAuth, when "api-key", triggers a fresh ANTHROPIC_AUTH_TOKEN request
	// after the cert is renewed, and rewrites the claude settings.json env block.
	llmAuth      string
	settingsPath string
}

// renewOnce performs a single certificate renewal: load identity, resolve broker
// URL, call agent.Renew, write the renewed identity back to idDir. If llmAuth is
// "api-key" and settingsPath is set, also refreshes the ANTHROPIC_AUTH_TOKEN via
// the broker /request endpoint and rewrites the claude settings.json env block.
func renewOnce(opts renewOpts) error {
	id, ok := agent.LoadIdentity(opts.idDir)
	if !ok {
		return fmt.Errorf("renew: no identity in %s", opts.idDir)
	}
	bURL, err := resolveBrokerURL(opts.brokerURL, opts.bootstrapPath)
	if err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	ctx := context.Background()
	renewed, err := agent.Renew(ctx, bURL, id)
	if err != nil {
		return err
	}
	if err := renewed.Write(opts.idDir); err != nil {
		return err
	}
	// api-key mode: refresh the LLM capability token and rewrite the claude
	// settings.json env block so the next claude launch picks up the fresh token.
	if opts.llmAuth == "api-key" && opts.settingsPath != "" {
		cn, err := leafCN(renewed.CertPEM)
		if err != nil {
			return fmt.Errorf("renew: parse CN for llm token: %w", err)
		}
		overlay := map[string]string{
			"CLAUDE_CODE_CLIENT_CERT": filepath.Join(opts.idDir, "agent.crt"),
			"CLAUDE_CODE_CLIENT_KEY":  filepath.Join(opts.idDir, "agent.key"),
			"NODE_EXTRA_CA_CERTS":     filepath.Join(opts.idDir, "ca.crt"),
		}
		if err := agent.RefreshLLMToken(ctx, bURL, renewed, cn, requestLLMToken, overlay); err != nil {
			return err
		}
		if err := writeClaudeSettingsEnv(opts.settingsPath)(overlay); err != nil {
			return err
		}
	}
	return nil
}

// cmdRenew renews the agent certificate. With -loop it runs as a long-lived
// sidecar, renewing every -interval (default 12h) until signalled. Transient
// errors in loop mode are logged to stderr and the loop continues; only
// SIGINT/SIGTERM terminates it. Without -loop it performs a single renewal.
// When -llm-auth api-key, also refreshes ANTHROPIC_AUTH_TOKEN after each cert
// renewal and rewrites the claude settings.json env block at -settings.
func cmdRenew(args []string) error {
	fs := flag.NewFlagSet("renew", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	idDir := fs.String("id-dir", defaultIDDir, "directory for the agent identity")
	brokerURL := fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	bootstrapPath := fs.String("bootstrap", "", "path to bootstrap.json")
	loop := fs.Bool("loop", false, "run as a renewal daemon (renew then sleep -interval, repeat until signal)")
	interval := fs.Duration("interval", 12*time.Hour, "renewal interval in loop mode (default 12h; cert TTL is 24h)")
	llmAuth := fs.String("llm-auth", "subscription", "LLM auth mode: 'api-key' refreshes ANTHROPIC_AUTH_TOKEN after each cert renewal")
	settingsPath := fs.String("settings", "", "path to the claude settings.json whose env block is rewritten on -llm-auth api-key refresh")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := renewOpts{
		idDir:         *idDir,
		brokerURL:     *brokerURL,
		bootstrapPath: *bootstrapPath,
		llmAuth:       *llmAuth,
		settingsPath:  *settingsPath,
	}

	if !*loop {
		return renewOnce(opts)
	}

	// Loop mode: renew once immediately, then on each ticker tick.
	// Signal-cancel the context to exit cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := renewOnce(opts); err != nil {
		fmt.Fprintln(os.Stderr, "lever-agent renew:", err)
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := renewOnce(opts); err != nil {
				fmt.Fprintln(os.Stderr, "lever-agent renew:", err)
			}
		}
	}
}

// cmdProvision mints a one-use enrolment ticket for a worker via the broker's
// /provision endpoint (manager-CN-gated). The resulting Bootstrap JSON is written
// to -out (0600) so the acceptance harness can drop it in the jail for boot.
func cmdProvision(args []string) error {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	idDir := fs.String("id-dir", defaultIDDir, "directory for the manager identity (cert+key+ca)")
	worker := fs.String("worker", "", "worker name to provision a ticket for")
	out := fs.String("out", "", "path to write the worker bootstrap JSON (0600)")
	bootstrapPath := fs.String("bootstrap", "", "path to bootstrap.json (for broker URL if -broker-url not set)")
	brokerURL := fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *worker == "" {
		return fmt.Errorf("provision: -worker is required")
	}
	if *out == "" {
		return fmt.Errorf("provision: -out is required")
	}

	id, ok := agent.LoadIdentity(*idDir)
	if !ok {
		return fmt.Errorf("provision: no identity in %s — run 'lever-agent boot' first", *idDir)
	}

	// Resolve broker URL and CA PEM: explicit flag wins, else from bootstrap file.
	// The CA is always the identity's pinned CA regardless of how the URL resolves.
	caPEM := string(id.CAPEM)
	bURL := *brokerURL
	if bURL == "" {
		resolved, err := resolveBrokerURL("", *bootstrapPath)
		if err != nil {
			return fmt.Errorf("provision: %w", err)
		}
		bURL = resolved
	}

	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("provision: build mTLS client: %w", err)
	}

	ticket, err := agent.Provision(context.Background(), bURL, client, *worker)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}

	bs := agent.BootstrapFor(*worker, ticket, caPEM, bURL)
	data, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("provision: marshal bootstrap: %w", err)
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		return fmt.Errorf("provision: write bootstrap: %w", err)
	}
	return nil
}

func cmdCLI(verb string, args []string) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	idDir := fs.String("id-dir", defaultIDDir, "directory for the agent identity")
	brokerURL := fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	bootstrapPath := fs.String("bootstrap", "", "path to bootstrap.json")
	// verb-specific flags
	var tool, op, to, tokenStr string
	switch verb {
	case "request", "delegate":
		fs.StringVar(&tool, "tool", "", "tool name")
		fs.StringVar(&op, "op", "", "operation")
		if verb == "delegate" {
			fs.StringVar(&to, "to", "", "recipient agent CN")
		}
	case "call":
		fs.StringVar(&tool, "tool", "", "tool name")
		fs.StringVar(&op, "op", "", "operation name (maps to params.name in the JSON-RPC envelope)")
		fs.StringVar(&tokenStr, "token", "", "base64url capability token")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	id, ok := agent.LoadIdentity(*idDir)
	if !ok {
		return fmt.Errorf("%s: no identity in %s", verb, *idDir)
	}
	bURL, err := resolveBrokerURL(*brokerURL, *bootstrapPath)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("%s: build mTLS client: %w", verb, err)
	}
	ctx := context.Background()

	// Build remaining constraints from any extra key=value args.
	constraints := map[string]string{}
	for _, a := range fs.Args() {
		k, v, ok := strings.Cut(a, "=")
		if ok {
			constraints[k] = v
		}
	}

	switch verb {
	case "request":
		cn, err := leafCN(id.CertPEM)
		if err != nil {
			return err
		}
		tok, err := agent.Request(ctx, bURL, client, tool, op, cn, constraints)
		if err != nil {
			return err
		}
		fmt.Println(tok)
	case "delegate":
		tok, err := agent.Request(ctx, bURL, client, tool, op, to, constraints)
		if err != nil {
			return err
		}
		fmt.Println(tok)
	case "call":
		// call: POST a JSON-RPC 2.0 tools/call to the broker gateway /mcp/<tool>/.
		// The capability token MUST be in params.arguments._capability (not a header);
		// the gateway reads the token exclusively from that field and actively scrubs
		// all inbound X-Lever-* headers. The tool name is encoded in the URL path;
		// params.name carries the operation name within that tool.
		body := buildToolCallBody(op, tokenStr, constraints)
		req, err := http.NewRequestWithContext(ctx, "POST", bURL+"/mcp/"+tool+"/", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("call: %w", err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			return fmt.Errorf("call: read response: %w", err)
		}
		fmt.Print(buf.String())
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("call: status %d", resp.StatusCode)
		}
	}
	return nil
}

// resolveBrokerURL returns brokerURL if set, else reads it from bootstrapPath
// (falling back to $LEVER_BOOTSTRAP then ./.lever/bootstrap.json).
func resolveBrokerURL(brokerURL, bootstrapPath string) (string, error) {
	if brokerURL != "" {
		return brokerURL, nil
	}
	bsPath := bootstrapPath
	if bsPath == "" {
		bsPath = os.Getenv("LEVER_BOOTSTRAP")
	}
	if bsPath == "" {
		bsPath = "./.lever/bootstrap.json"
	}
	bs, err := agent.LoadBootstrap(bsPath)
	if err != nil {
		return "", fmt.Errorf("resolve broker URL: %w", err)
	}
	return bs.BrokerURL, nil
}

// buildToolCallBody constructs the JSON-RPC 2.0 body for a tools/call request to
// the capability gateway. The token is placed in arguments._capability as required
// by the gateway contract (internal/broker/mcp.go:toolsCallFields). The tool's
// URL path carries the tool name; op maps to params.name (the operation within
// that tool). Extra key=value pairs from the CLI are merged into arguments.
func buildToolCallBody(op, token string, args map[string]string) []byte {
	arguments := make(map[string]any, len(args)+1)
	for k, v := range args {
		arguments[k] = v
	}
	arguments["_capability"] = token
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      op,
			"arguments": arguments,
		},
	}
	out, _ := json.Marshal(body)
	return out
}

// leafCN parses the common name from the first certificate in certPEM.
func leafCN(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("leafCN: invalid cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("leafCN: parse cert: %w", err)
	}
	return cert.Subject.CommonName, nil
}
