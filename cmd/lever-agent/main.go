// Command lever-agent is the in-jail capability helper: it enrols the agent's
// mTLS identity (key generated in-container, never leaves), serves the capability
// MCP tool the LLM drives, renews before expiry, and (via CLI verbs) lets the
// acceptance harness mint/delegate/exercise deterministically.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/agent"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("lever-agent: ")
	if err := run(os.Args); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

// errNoIdentity means the id-dir holds no enrolled identity. Every verb that
// needs one wraps it ("<verb>: no identity in <dir>").
var errNoIdentity = errors.New("no identity")

func run(argv []string) error {
	if len(argv) < 2 {
		return errors.New("usage: lever-agent <boot|serve-capability|renew|gateway|provision|request|delegate|call>")
	}
	// One signal-bound context for every verb: SIGINT/SIGTERM cancels the
	// network call in flight (or the renew loop) instead of leaving the
	// process hanging in a dial or a read.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch argv[1] {
	case "boot":
		return cmdBoot(ctx, argv[2:])
	case "serve-capability":
		return cmdServeCapability(ctx, argv[2:])
	case "renew":
		return cmdRenew(ctx, argv[2:])
	case "gateway":
		return cmdGateway(argv[2:])
	case "provision":
		return cmdProvision(ctx, argv[2:])
	case "request", "delegate", "call":
		return cmdCLI(ctx, argv[1], argv[2:])
	default:
		return fmt.Errorf("unknown subcommand %q", argv[1])
	}
}

// defaultBootstrapPath is the --bootstrap default shared by every subcommand:
// $LEVER_BOOTSTRAP, else ./.lever/bootstrap.json.
func defaultBootstrapPath() string {
	if p := os.Getenv("LEVER_BOOTSTRAP"); p != "" {
		return p
	}
	return "./.lever/bootstrap.json"
}

// defaultIDDir is the --id-dir default shared by every subcommand: $HOME/.lever-id.
func defaultIDDir() string {
	return filepath.Join(os.Getenv("HOME"), ".lever-id")
}

// toolsFlag is the -tools flag.Value: a comma-separated tool list that also
// records whether the flag was given at all, distinguishing "flag omitted"
// (auto-discover from the broker) from "-tools ”" (explicit empty list).
type toolsFlag struct {
	set   bool
	tools []string
}

func (f *toolsFlag) String() string { return strings.Join(f.tools, ",") }

func (f *toolsFlag) Set(v string) error {
	f.set = true
	f.tools = nil
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t != "" {
			f.tools = append(f.tools, t)
		}
	}
	return nil
}

// cmdBoot wires the real claude-mcp-add exec into agent.Boot and then emits the
// sidecar specs. Flags: -bootstrap (default $LEVER_BOOTSTRAP or
// ./.lever/bootstrap.json), -id-dir (default $HOME/.lever-id), -settings (claude
// settings.json path; its env block receives the dynamic LLM vars),
// -tools (comma-separated broker tool names).
func cmdBoot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("boot", flag.ContinueOnError)
	bootstrapPath := fs.String("bootstrap", defaultBootstrapPath(), "path to bootstrap.json")
	idDir := fs.String("id-dir", defaultIDDir(), "directory for the agent identity (cert+key+ca)")
	settingsPath := fs.String("settings", "", "path to the claude settings.json whose env block receives ANTHROPIC_AUTH_TOKEN/BASE_URL (api-key mode)")
	var tools toolsFlag
	fs.Var(&tools, "tools", "comma-separated broker tool names to register via claude mcp add")
	llmAuth := fs.String("llm-auth", agent.LLMAuthSubscription, "LLM auth mode: 'api-key' obtains a capability(llm) token and writes ANTHROPIC_AUTH_TOKEN/BASE_URL into the claude settings.json env block; 'subscription' (default) leaves those keys absent and uses the user's own key")
	enrolOnly := fs.Bool("enrol-only", false, "enrol + write the identity only; skip the claude mcp registration and env overlay (no `claude` binary required — used by the VM-level acceptance gate, which drives lever-agent's CLI verbs directly)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := agent.BootConfig{
		BootstrapPath: *bootstrapPath,
		IDDir:         *idDir,
		BrokerTools:   tools.tools,
		// Auto-discover tools from the broker only when -tools was not given.
		// When -tools is set (even to ""), the explicit list wins.
		DiscoverTools: !tools.set,
		SettingsPath:  *settingsPath,
		LLMAuth:       *llmAuth,
		MCPAdd:        newClaudeMCP().Add,
	}
	if *enrolOnly {
		// Enrol + write identity only: skip the env overlay, the llm token and
		// the `claude mcp add` registration (no claude in the bare VM).
		cfg.BrokerTools = nil
		cfg.DiscoverTools = false
		cfg.SettingsPath = ""
		cfg.LLMAuth = ""
		cfg.MCPAdd = nil
	}
	if err := agent.Boot(ctx, cfg); err != nil {
		return err
	}
	// Emit the renew sidecar so scion auto-refreshes the cert and (in api-key
	// mode) the LLM token. Skip in enrol-only mode — the bare VM acceptance gate
	// has no long-running container to run sidecars in.
	if *enrolOnly {
		return nil
	}
	err := agent.WriteSidecarSpecs(agent.SidecarConfig{
		HomeDir:       os.Getenv("HOME"),
		IDDir:         *idDir,
		BootstrapPath: *bootstrapPath,
		SettingsPath:  *settingsPath,
		LLMAuth:       *llmAuth,
	})
	if err != nil {
		return fmt.Errorf("emit renew sidecar: %w", err)
	}
	return nil
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

// claudeMCP registers MCP servers through the `claude` CLI. run executes one
// command and returns its combined output; tests substitute a recorder so the
// remove-then-add ordering and error handling are testable without a real
// `claude` binary.
type claudeMCP struct {
	run func(name string, args ...string) ([]byte, error)
}

// newClaudeMCP is the production registrar, exec'ing `claude` from PATH.
func newClaudeMCP() claudeMCP {
	return claudeMCP{run: func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}}
}

// Add registers an MCP server, idempotently. `claude mcp add` exits
// non-zero if the server already exists, and the scion pre-start hook runs boot
// under `set -eu` on every container start — so on a resume (same persistent
// /home/scion), an unconditional add would fail the hook and block bring-up.
// Removing first (ignoring "no such server", which also exits non-zero) makes it
// a clean upsert regardless of prior state.
func (c claudeMCP) Add(name string, argv ...string) error {
	_, _ = c.run("claude", mcpRemoveArgs(name)...) // ignore: absent is fine
	out, err := c.run("claude", mcpAddArgs(name, argv)...)
	if err != nil {
		return fmt.Errorf("claude mcp add %s: %w: %s", name, err, out)
	}
	return nil
}

// cmdServeCapability serves the capability MCP server over stdio (line-delimited
// JSON-RPC 2.0, see agent.ServeStdio). It pairs with boot's
//
//	MCPAdd("lever-capability", "lever-agent", "serve-capability")
//
// which registers this binary as an MCP command-mode server: Claude Code (and
// the acceptance harness) spawns "lever-agent serve-capability" and talks
// JSON-RPC over its stdin/stdout.
func cmdServeCapability(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve-capability", flag.ContinueOnError)
	idDir, brokerURL, bootstrapPath := commonFlags(fs, "directory for the agent identity", "path to bootstrap.json (for broker URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, ok := agent.LoadIdentity(*idDir)
	if !ok {
		return fmt.Errorf("serve-capability: %w in %s — run 'lever-agent boot' first", errNoIdentity, *idDir)
	}
	bURL, err := resolveBrokerURL(*brokerURL, *bootstrapPath)
	if err != nil {
		return fmt.Errorf("serve-capability: %w", err)
	}
	// serve-capability is a LONG-LIVED sidecar: use a client that re-reads the
	// rotating leaf per handshake, not id.Client()'s static boot cert (which
	// would expire after 24h and break every capability mint — the recurring
	// brokered-tools outage). See agent.NewReloadingClient.
	client, err := agent.NewReloadingClient(*idDir, id.CAPEM)
	if err != nil {
		return fmt.Errorf("serve-capability: build mTLS client: %w", err)
	}
	agentCN, err := id.CN()
	if err != nil {
		return fmt.Errorf("serve-capability: %w", err)
	}
	srv := agent.NewMCPServer(agent.MCPConfig{
		BrokerURL: bURL,
		AgentCN:   agentCN,
		Client:    client,
	})
	return agent.ServeStdio(ctx, os.Stdin, os.Stdout, srv)
}

// renewOpts collects the renew flags for renewOnce.
type renewOpts struct {
	idDir, brokerURL, bootstrapPath string
	// llmAuth, when "api-key", triggers a fresh ANTHROPIC_AUTH_TOKEN request
	// after the cert is renewed, and rewrites the claude settings.json env block.
	llmAuth      string
	settingsPath string
}

// renewOnce loads the identity, resolves the broker URL the lazy way every other
// subcommand does, and runs one agent.RenewOnce cycle.
func renewOnce(ctx context.Context, opts renewOpts) error {
	id, ok := agent.LoadIdentity(opts.idDir)
	if !ok {
		return fmt.Errorf("renew: %w in %s", errNoIdentity, opts.idDir)
	}
	bURL, err := resolveBrokerURL(opts.brokerURL, opts.bootstrapPath)
	if err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	return agent.RenewOnce(ctx, agent.RenewConfig{
		Identity:     id,
		IDDir:        opts.idDir,
		BrokerURL:    bURL,
		LLMAuth:      opts.llmAuth,
		SettingsPath: opts.settingsPath,
	})
}

// cmdRenew renews the agent certificate. With -loop it runs as a long-lived
// sidecar, renewing every -interval (default 12h) until signalled. Transient
// errors in loop mode are logged and the loop continues; only SIGINT/SIGTERM
// terminates it. Without -loop it performs a single renewal.
// When -llm-auth api-key, also refreshes ANTHROPIC_AUTH_TOKEN after each cert
// renewal and rewrites the claude settings.json env block at -settings.
func cmdRenew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("renew", flag.ContinueOnError)
	idDir, brokerURL, bootstrapPath := commonFlags(fs, "directory for the agent identity", "path to bootstrap.json")
	loop := fs.Bool("loop", false, "run as a renewal daemon (renew then sleep -interval, repeat until signal)")
	interval := fs.Duration("interval", 12*time.Hour, "renewal interval in loop mode (default 12h; cert TTL is 24h)")
	llmAuth := fs.String("llm-auth", agent.LLMAuthSubscription, "LLM auth mode: 'api-key' refreshes ANTHROPIC_AUTH_TOKEN after each cert renewal")
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
		return renewOnce(ctx, opts)
	}

	// Loop mode: renew once immediately, then on each ticker tick, until the
	// signal-bound ctx is cancelled.
	if err := renewOnce(ctx, opts); err != nil {
		log.Print("renew: ", err)
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := renewOnce(ctx, opts); err != nil {
				log.Print("renew: ", err)
			}
		}
	}
}

// cmdGateway runs the long-lived loopback reverse-proxy that presents the
// always-current agent leaf to the broker on Claude's behalf (Claude caches a
// cert for its process lifetime, so it can't follow the 24h-TTL leaf's rotation).
// Claude talks plaintext to --listen; the proxy re-reads <id-dir>/agent.{crt,key}
// per handshake. Flags: --id-dir, --broker-url / --bootstrap (broker URL + CA),
// --listen (loopback only).
func cmdGateway(args []string) error {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	idDir, brokerURL, bootstrapPath := commonFlags(fs, "directory for the agent identity (cert+key+ca)", "path to bootstrap.json (for broker URL + CA)")
	listen := fs.String("listen", agent.LocalGatewayAddr, "loopback address to serve plaintext MCP/LLM traffic on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bURL, err := resolveBrokerURL(*brokerURL, *bootstrapPath)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	// CA to trust the broker's serving cert: prefer the identity's pinned ca.crt
	// (written at enrol), fall back to the bootstrap's BrokerCA before enrolment.
	var caPEM []byte
	if id, ok := agent.LoadIdentity(*idDir); ok {
		caPEM = id.CAPEM
	} else if bs, berr := agent.LoadBootstrap(bootstrapPathOrDefault(*bootstrapPath)); berr == nil {
		caPEM = []byte(bs.BrokerCA)
	}
	if len(caPEM) == 0 {
		return fmt.Errorf("gateway: no CA found in %s or bootstrap", *idDir)
	}
	return agent.Gateway(agent.GatewayConfig{Listen: *listen, BrokerURL: bURL, CAPEM: caPEM, IDDir: *idDir})
}

// bootstrapPathOrDefault resolves an empty --bootstrap the same way
// resolveBrokerURL does, so the gateway's CA fallback reads the same file.
func bootstrapPathOrDefault(path string) string {
	if path != "" {
		return path
	}
	return defaultBootstrapPath()
}

// cmdProvision mints a one-use enrolment ticket for a worker via the broker's
// /provision endpoint (manager-CN-gated). The resulting Bootstrap JSON is written
// to -out (0600) so the acceptance harness can drop it in the jail for boot.
func cmdProvision(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	idDir, brokerURL, bootstrapPath := commonFlags(fs, "directory for the manager identity (cert+key+ca)", "path to bootstrap.json (for broker URL if -broker-url not set)")
	worker := fs.String("worker", "", "worker name to provision a ticket for")
	out := fs.String("out", "", "path to write the worker bootstrap JSON (0600)")
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
		return fmt.Errorf("provision: %w in %s — run 'lever-agent boot' first", errNoIdentity, *idDir)
	}

	// Resolve broker URL: explicit flag wins, else from bootstrap file. The CA
	// is always the identity's pinned CA regardless of how the URL resolves.
	bURL, err := resolveBrokerURL(*brokerURL, *bootstrapPath)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}

	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("provision: build mTLS client: %w", err)
	}

	ticket, err := agent.Provision(ctx, bURL, client, *worker)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}

	bs := agent.Bootstrap{Ticket: ticket, BrokerCA: string(id.CAPEM), BrokerURL: bURL, AgentCN: *worker}
	data, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("provision: marshal bootstrap: %w", err)
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		return fmt.Errorf("provision: write bootstrap: %w", err)
	}
	return nil
}

// cliArgs is what one capability verb (request, delegate, call) parsed from
// its flags, plus the remaining key=value constraints.
type cliArgs struct {
	verb                            string
	idDir, brokerURL, bootstrapPath string
	tool, op, to, token             string
	constraints                     map[string]string
}

// cliSession is a parsed verb with its identity and broker client resolved.
type cliSession struct {
	cliArgs
	id        agent.Identity
	brokerURL string
	client    *http.Client
}

// cliVerbs dispatches each capability verb to its action.
var cliVerbs = map[string]func(context.Context, cliSession) error{
	"request":  cliRequest,
	"delegate": cliDelegate,
	"call":     cliCall,
}

// cmdCLI runs one capability verb: parse, validate, resolve identity + broker,
// then dispatch through cliVerbs.
func cmdCLI(ctx context.Context, verb string, args []string) error {
	a, err := parseCLIArgs(verb, args)
	if err != nil {
		return err
	}
	if err := validateCLIArgs(a); err != nil {
		return err
	}
	id, ok := agent.LoadIdentity(a.idDir)
	if !ok {
		return fmt.Errorf("%s: %w in %s", verb, errNoIdentity, a.idDir)
	}
	bURL, err := resolveBrokerURL(a.brokerURL, a.bootstrapPath)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("%s: build mTLS client: %w", verb, err)
	}
	return cliVerbs[verb](ctx, cliSession{cliArgs: a, id: id, brokerURL: bURL, client: client})
}

// parseCLIArgs registers the verb's flags and parses args into a cliArgs.
// Extra positional key=value args become constraints.
func parseCLIArgs(verb string, args []string) (cliArgs, error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	idDir, brokerURL, bootstrapPath := commonFlags(fs, "directory for the agent identity", "path to bootstrap.json")
	a := cliArgs{verb: verb, constraints: map[string]string{}}
	switch verb {
	case "request", "delegate":
		fs.StringVar(&a.tool, "tool", "", "tool name")
		fs.StringVar(&a.op, "op", "", "operation")
		if verb == "delegate" {
			fs.StringVar(&a.to, "to", "", "recipient agent CN")
		}
	case "call":
		fs.StringVar(&a.tool, "tool", "", "tool name")
		fs.StringVar(&a.op, "op", "", "operation name (maps to params.name in the JSON-RPC envelope)")
		fs.StringVar(&a.token, "token", "", "base64url capability token")
	}
	if err := fs.Parse(args); err != nil {
		return cliArgs{}, err
	}
	a.idDir, a.brokerURL, a.bootstrapPath = *idDir, *brokerURL, *bootstrapPath
	for _, arg := range fs.Args() {
		if k, v, ok := strings.Cut(arg, "="); ok {
			a.constraints[k] = v
		}
	}
	return a, nil
}

// validateCLIArgs checks the caller's own arguments before anything touches
// the identity, the filesystem or the network, so a bad invocation names the
// bad argument instead of surfacing as whatever unrelated thing fails first.
// lever-agent is on $PATH inside every agent jail, so these verbs are the same
// mint path the capability MCP tool exposes and carried the same hazard:
// `delegate` with no recipient sent an EMPTY bind target, which the broker
// defaults to the caller ("default: self-obtain"), printing a SELF-bound token
// as an ordinary success. See requestArgs/delegateArgs in
// internal/agent/mcpserver.go — the checks are deliberately not shared,
// because the surfaces differ: here flags and constraints occupy separate
// namespaces, so a positional `to=...` is unambiguously a constraint, while
// the MCP tool has one flat argument map in which it is ambiguous.
func validateCLIArgs(a cliArgs) error {
	switch a.verb {
	case "request", "delegate":
		if strings.TrimSpace(a.tool) == "" {
			return fmt.Errorf(`%s: missing required argument "-tool"`, a.verb)
		}
		if strings.TrimSpace(a.op) == "" {
			return fmt.Errorf(`%s: missing required argument "-op"`, a.verb)
		}
		if a.verb == "delegate" && strings.TrimSpace(a.to) == "" {
			return errors.New(`delegate: missing required argument "-to" (the recipient agent CN); use "request" to mint a token for yourself`)
		}
	}
	return nil
}

// cliRequest mints a token bound to this agent and prints it.
func cliRequest(ctx context.Context, s cliSession) error {
	cn, err := s.id.CN()
	if err != nil {
		return err
	}
	tok, err := agent.Request(ctx, s.brokerURL, s.client, s.tool, s.op, cn, s.constraints)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}

// cliDelegate mints a token bound to another agent and prints it. Parity with
// the MCP tool: naming yourself hands nothing off, and it routes through the
// OBTAIN policy rather than the delegate one, so it succeeds with no delegate
// grant and audits like a self-obtain.
func cliDelegate(ctx context.Context, s cliSession) error {
	cn, err := s.id.CN()
	if err != nil {
		return err
	}
	if s.to == cn {
		return errors.New(`delegate: "-to" names this agent; use "request" to mint a token for yourself`)
	}
	tok, err := agent.Request(ctx, s.brokerURL, s.client, s.tool, s.op, s.to, s.constraints)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}

// cliCall POSTs the JSON-RPC tools/call to the gateway. agent.Call hands back
// the raw body and the error separately so the body is printed BEFORE a
// non-200 error surfaces — the acceptance harness's deny checks rely on both
// the printed output and the non-zero exit.
func cliCall(ctx context.Context, s cliSession) error {
	out, err := agent.Call(ctx, s.brokerURL, s.client, s.tool, s.op, s.token, s.constraints)
	fmt.Print(out)
	return err
}

// resolveBrokerURL returns brokerURL if set, else reads it from bootstrapPath
// (falling back to $LEVER_BOOTSTRAP then ./.lever/bootstrap.json).
func resolveBrokerURL(brokerURL, bootstrapPath string) (string, error) {
	if brokerURL != "" {
		return brokerURL, nil
	}
	bs, err := agent.LoadBootstrap(bootstrapPathOrDefault(bootstrapPath))
	if err != nil {
		return "", fmt.Errorf("resolve broker URL: %w", err)
	}
	return bs.BrokerURL, nil
}

// commonFlags registers the id-dir/broker-url/bootstrap trio shared verbatim by
// the five lazy-resolving subcommands (serve-capability, renew, gateway,
// provision, cmdCLI) and returns the bound pointers. The id-dir and bootstrap
// help strings genuinely differ per command (provision names the *manager*
// identity; gateway's bootstrap also carries the CA), so they are passed in; the
// broker-url flag is identical everywhere. cmdBoot is deliberately NOT a caller:
// it has no --broker-url and eagerly resolves its --bootstrap default.
func commonFlags(fs *flag.FlagSet, idDirHelp, bootstrapHelp string) (idDir, brokerURL, bootstrap *string) {
	idDir = fs.String("id-dir", defaultIDDir(), idDirHelp)
	brokerURL = fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	bootstrap = fs.String("bootstrap", "", bootstrapHelp)
	return idDir, brokerURL, bootstrap
}
