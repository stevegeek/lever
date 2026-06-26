// Command lever-agent is the in-jail capability helper: it enrols the agent's
// mTLS identity (key generated in-container, never leaves), serves the capability
// MCP tool the LLM drives, renews before expiry, and (via CLI verbs) lets the
// acceptance harness mint/attenuate/delegate/exercise deterministically.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lever-to/lever/internal/agent"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "lever-agent:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) < 2 {
		return fmt.Errorf("usage: lever-agent <boot|serve-capability|renew|provision|request|attenuate|delegate|call> ...")
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
	case "request", "attenuate", "delegate", "call":
		return cmdCLI(argv[1], argv[2:])
	default:
		return fmt.Errorf("unknown subcommand %q", argv[1])
	}
}

// cmdBoot wires the real claude-mcp-add exec + the scion env-overlay path into
// agent.Boot. Flags: -bootstrap (default $LEVER_BOOTSTRAP or ./.lever/bootstrap.json),
// -id-dir (default $HOME/.lever-id), -overlay (scion env-overlay output path),
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
	overlayPath := fs.String("overlay", "", "path to write scion env-overlay JSON")
	toolsCSV := fs.String("tools", "", "comma-separated broker tool names to register via claude mcp add")
	enrolOnly := fs.Bool("enrol-only", false, "enrol + write the identity only; skip the claude mcp registration and env overlay (no `claude` binary required — used by the VM-level acceptance gate, which drives lever-agent's CLI verbs directly)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var brokerTools []string
	if *toolsCSV != "" {
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
		WriteEnvOverlay: writeOverlay(*overlayPath),
	}
	// Auto-discover tools from the broker when no explicit -tools value was given.
	// When an explicit list is provided, leave ListTools nil so the list wins.
	if *toolsCSV == "" {
		cfg.ListTools = agent.ListTools
	}
	if *enrolOnly {
		// Enrol + write identity only: nil hooks make agent.Boot skip the env
		// overlay and the `claude mcp add` registration (no claude in the bare VM).
		cfg.MCPAdd = nil
		cfg.WriteEnvOverlay = nil
		cfg.BrokerTools = nil
		cfg.ListTools = nil
	}
	return agent.Boot(ctx, cfg)
}

func claudeMCPAdd(name string, argv ...string) error {
	out, err := exec.Command("claude", append([]string{"mcp", "add", name}, argv...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude mcp add %s: %w: %s", name, err, out)
	}
	return nil
}

func writeOverlay(path string) func(map[string]string) error {
	return func(m map[string]string) error {
		if path == "" {
			return nil
		}
		b, _ := json.Marshal(m)
		return os.WriteFile(path, b, 0o600)
	}
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
// (request/attenuate/delegate) this is fine; revisit if the MCP session needs
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

// renewOnce performs a single certificate renewal: load identity, resolve broker
// URL, call agent.Renew, and write the renewed identity back to idDir.
func renewOnce(idDir, brokerURL, bootstrapPath string) error {
	id, ok := agent.LoadIdentity(idDir)
	if !ok {
		return fmt.Errorf("renew: no identity in %s", idDir)
	}
	bURL, err := resolveBrokerURL(brokerURL, bootstrapPath)
	if err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	renewed, err := agent.Renew(context.Background(), bURL, id)
	if err != nil {
		return err
	}
	return renewed.Write(idDir)
}

// cmdRenew renews the agent certificate. With -loop it runs as a long-lived
// sidecar, renewing every -interval (default 12h) until signalled. Transient
// errors in loop mode are logged to stderr and the loop continues; only
// SIGINT/SIGTERM terminates it. Without -loop it performs a single renewal.
func cmdRenew(args []string) error {
	fs := flag.NewFlagSet("renew", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	idDir := fs.String("id-dir", defaultIDDir, "directory for the agent identity")
	brokerURL := fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	bootstrapPath := fs.String("bootstrap", "", "path to bootstrap.json")
	loop := fs.Bool("loop", false, "run as a renewal daemon (renew then sleep -interval, repeat until signal)")
	interval := fs.Duration("interval", 12*time.Hour, "renewal interval in loop mode (default 12h; cert TTL is 24h)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !*loop {
		return renewOnce(*idDir, *brokerURL, *bootstrapPath)
	}

	// Loop mode: renew once immediately, then on each ticker tick.
	// Signal-cancel the context to exit cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := renewOnce(*idDir, *brokerURL, *bootstrapPath); err != nil {
		fmt.Fprintln(os.Stderr, "lever-agent renew:", err)
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := renewOnce(*idDir, *brokerURL, *bootstrapPath); err != nil {
				fmt.Fprintln(os.Stderr, "lever-agent renew:", err)
			}
		}
	}
}

// cmdProvision mints a one-use enrolment ticket for a grove via the broker's
// /provision endpoint (manager-CN-gated). The resulting Bootstrap JSON is written
// to -out (0600) so the acceptance harness can drop it in the jail for boot.
func cmdProvision(args []string) error {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	defaultIDDir := filepath.Join(os.Getenv("HOME"), ".lever-id")
	idDir := fs.String("id-dir", defaultIDDir, "directory for the manager identity (cert+key+ca)")
	grove := fs.String("grove", "", "grove name to provision a ticket for")
	out := fs.String("out", "", "path to write the grove bootstrap JSON (0600)")
	bootstrapPath := fs.String("bootstrap", "", "path to bootstrap.json (for broker URL if -broker-url not set)")
	brokerURL := fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *grove == "" {
		return fmt.Errorf("provision: -grove is required")
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

	ticket, err := agent.Provision(context.Background(), bURL, client, *grove)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}

	bs := agent.BootstrapFor(*grove, ticket, caPEM, bURL)
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
	case "attenuate":
		fs.StringVar(&tokenStr, "token", "", "base64url token to attenuate")
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
	case "attenuate":
		narrowed, err := agent.Attenuate(tokenStr, constraints)
		if err != nil {
			return err
		}
		fmt.Println(narrowed)
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
