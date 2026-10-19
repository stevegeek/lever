// Command lever-agent is the in-jail capability helper: it enrols the agent's
// mTLS identity (the key is generated in the container and never leaves),
// serves the capability MCP tool, renews before expiry, and offers CLI verbs
// to mint, delegate and call capabilities.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/agent"
	"github.com/stevegeek/lever/internal/logging"
)

func main() { os.Exit(realMain(os.Args, os.Stderr)) }

// logVerbs run unattended (boot in scion's pre-start hook, the others as
// sidecars or as Claude Code's MCP server), so only the container log reads
// their stderr and it takes lever's log line shape. The other verbs answer
// in prose.
var logVerbs = map[string]bool{"boot": true, "serve-capability": true, "renew": true, "gateway": true}

func realMain(argv []string, stderr io.Writer) int {
	if len(argv) > 1 && logVerbs[argv[1]] {
		l := logging.Install(stderr, "lever-agent").With("cmd", argv[1])
		slog.SetDefault(l)
		if err := run(argv); err != nil {
			l.Error(err.Error())
			return 1
		}
		return 0
	}
	if err := run(argv); err != nil {
		fmt.Fprintf(stderr, "lever-agent: %v\n", err)
		return 1
	}
	return 0
}

// warnf is boot's Log: what boot reports is a problem it carried on through.
func warnf(format string, args ...any) { slog.Warn(fmt.Sprintf(format, args...)) }

// errNoIdentity means the id-dir holds no enrolled identity.
var errNoIdentity = errors.New("no identity")

func run(argv []string) error {
	if len(argv) < 2 {
		return errors.New("usage: lever-agent <boot|serve-capability|renew|gateway|request|delegate|call|after-compact|claude-ready|version>")
	}
	if argv[1] == "--version" || argv[1] == "-version" {
		fmt.Println(agent.BuildLine("lever-agent", agent.Version))
		return nil
	}
	// SIGINT/SIGTERM cancel ctx. NotifyContext replaces the default terminate
	// disposition, so every long-lived verb MUST return on ctx.Done or it
	// outlives the signal until SIGKILL.
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
		return cmdGateway(ctx, argv[2:])
	case "request", "delegate", "call":
		return cmdCLI(ctx, argv[1], argv[2:])
	case "after-compact":
		return cmdAfterCompact(argv[2:], os.Stdout)
	case "claude-ready":
		return cmdClaudeReady(argv[2:], os.Getenv("TMUX_PANE"))
	case "version":
		return cmdVersion(ctx, argv[2:], os.Stdout)
	default:
		return fmt.Errorf("unknown subcommand %q", argv[1])
	}
}

// cmdVersion prints this binary's build, then the host lever's (GET /version)
// and whether the two match.
func cmdVersion(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	idDir, brokerURL, bootstrapPath := commonFlags(fs, "directory for the agent identity", "path to bootstrap.json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("version: unexpected argument %q", fs.Arg(0))
	}
	fmt.Fprintln(stdout, agent.BuildLine("lever-agent", agent.Version))
	id, ok := agent.LoadIdentity(*idDir)
	if !ok {
		return fmt.Errorf("version: %w in %s", errNoIdentity, *idDir)
	}
	bURL, err := resolveBrokerURL(*brokerURL, *bootstrapPath)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("version: build mTLS client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	host, err := agent.HostVersion(ctx, bURL, client)
	report, err := agent.VersionReport(agent.Version, host, err, os.Getenv("LEVER_BOOTSTRAP") != "")
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	fmt.Fprintln(stdout, report)
	return nil
}

// defaultBootstrapPath is $LEVER_BOOTSTRAP, else ./.lever/bootstrap.json.
func defaultBootstrapPath() string {
	if p := os.Getenv("LEVER_BOOTSTRAP"); p != "" {
		return p
	}
	return "./.lever/bootstrap.json"
}

func defaultIDDir() string {
	return filepath.Join(os.Getenv("HOME"), ".lever-id")
}

// toolsFlag is the -tools flag: a comma-separated list that also records
// whether the flag was given, distinguishing omitted (auto-discover from the
// broker) from an explicit empty list.
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

// cmdBoot wires the real `claude mcp add` exec into agent.Boot and then
// emits the sidecar specs.
func cmdBoot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("boot", flag.ContinueOnError)
	bootstrapPath := fs.String("bootstrap", defaultBootstrapPath(), "path to bootstrap.json")
	idDir := fs.String("id-dir", defaultIDDir(), "directory for the agent identity (cert+key+ca)")
	settingsPath := fs.String("settings", "", "path to the claude settings.json whose env block receives ANTHROPIC_AUTH_TOKEN/BASE_URL (api-key mode)")
	managedPath := fs.String("managed-settings", "", "path to Claude Code's managed-settings.json, which receives the bootstrap's claude block (auto_compact_window, after_compact_note); empty skips it")
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
		// An explicit -tools (even empty) wins over auto-discovery.
		DiscoverTools:       !tools.set,
		SettingsPath:        *settingsPath,
		ManagedSettingsPath: *managedPath,
		Log:                 warnf,
		LLMAuth:             *llmAuth,
		MCPAdd:              newClaudeMCP().Add,
	}
	if *enrolOnly {
		// Enrol only: no env overlay, llm token or `claude mcp add` (there is no
		// claude in the bare VM).
		cfg.BrokerTools = nil
		cfg.DiscoverTools = false
		cfg.SettingsPath = ""
		cfg.ManagedSettingsPath = ""
		cfg.Log = nil
		cfg.LLMAuth = ""
		cfg.MCPAdd = nil
	}
	if err := agent.Boot(ctx, cfg); err != nil {
		return err
	}
	// The renew sidecar refreshes the cert and, in api-key mode, the LLM token.
	// Enrol-only mode has no long-running container to run sidecars in.
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

// cmdAfterCompact is lever's after-compaction hook (written by
// agent.WriteManagedSettings): Claude Code runs it at the session start after
// a compaction, and it prints the operator's note as SessionStart hook
// context. It reads nothing but its flag (the hook input on stdin is ignored)
// and reaches nothing outside the container.
func cmdAfterCompact(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("after-compact", flag.ContinueOnError)
	noteB64 := fs.String("note-b64", "", "the operator's note, base64 (standard encoding)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := agent.AfterCompactOutput(*noteB64)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}

// cmdClaudeReady is lever's claude-ready hook (written by
// agent.WriteManagedSettings): Claude Code runs it at every session start,
// and it writes the marker the manager gate waits for
// (agent.WriteReadyMarker). A session outside a tmux pane is not scion's
// harness and writes nothing. It prints nothing on stdout (Claude Code would
// add it to the session's context) and takes no flags: the command line is
// in a file the agent can rewrite, so nothing it passes may change what the
// marker says.
func cmdClaudeReady(args []string, pane string) error {
	if len(args) != 0 {
		return errors.New("claude-ready: takes no arguments")
	}
	err := agent.WriteReadyMarker(agent.ReadyMarkerPath, "/proc", pane)
	if errors.Is(err, agent.ErrNotHarness) {
		return nil
	}
	return err
}

// mcpAddArgs forces --scope user: the pre-start hook runs boot from the agent
// HOME but the session runs in /workspace, and the default local scope is
// keyed by the working directory, so a local registration would be invisible
// to the session. --scope precedes the server name.
func mcpAddArgs(name string, argv []string) []string {
	return append([]string{"mcp", "add", "--scope", "user", name}, argv...)
}

// mcpRemoveArgs targets the same user scope as mcpAddArgs.
func mcpRemoveArgs(name string) []string {
	return []string{"mcp", "remove", "--scope", "user", name}
}

// claudeMCP registers MCP servers through the `claude` CLI. run returns one
// command's combined output; tests substitute a recorder.
type claudeMCP struct {
	run func(name string, args ...string) ([]byte, error)
}

func newClaudeMCP() claudeMCP {
	return claudeMCP{run: func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}}
}

// Add registers an MCP server idempotently. `claude mcp add` exits non-zero
// if the server exists, and the pre-start hook runs boot under `set -eu` on
// every start, so a resume would fail the hook. Removing first (ignoring the
// non-zero "no such server") makes it an upsert.
func (c claudeMCP) Add(name string, argv ...string) error {
	_, _ = c.run("claude", mcpRemoveArgs(name)...) // ignore: absent is fine
	out, err := c.run("claude", mcpAddArgs(name, argv)...)
	if err != nil {
		return fmt.Errorf("claude mcp add %s: %w: %s", name, err, out)
	}
	return nil
}

// cmdServeCapability serves the capability MCP server over stdio
// (agent.ServeStdio); boot registers it as a command-mode MCP server.
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
	// A long-lived sidecar: re-read the rotating leaf per handshake.
	// id.Client()'s static boot cert expires after 24h and would break every
	// capability mint.
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

type renewOpts struct {
	idDir, brokerURL, bootstrapPath string
	// llmAuth "api-key" also requests a fresh ANTHROPIC_AUTH_TOKEN after the
	// cert is renewed and rewrites the claude settings.json env block.
	llmAuth      string
	settingsPath string
}

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

// cmdRenew renews the agent certificate. With -loop it runs as a sidecar,
// renewing every -interval until signalled; errors in loop mode are logged
// and the loop continues.
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

	if err := renewOnce(ctx, opts); err != nil {
		slog.Warn("renewal failed; trying again at the next interval", "err", err, "interval", *interval)
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := renewOnce(ctx, opts); err != nil {
				slog.Warn("renewal failed; trying again at the next interval", "err", err, "interval", *interval)
			}
		}
	}
}

// cmdGateway runs the loopback reverse proxy that presents the always-current
// agent leaf to the broker on Claude's behalf: Claude caches a cert for its
// process lifetime, so it cannot follow the 24h leaf's rotation. Claude
// talks plaintext to --listen (loopback only); the proxy re-reads
// <id-dir>/agent.{crt,key} per handshake.
func cmdGateway(ctx context.Context, args []string) error {
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
	// Trust the identity's pinned ca.crt, falling back to the bootstrap's
	// BrokerCA before enrolment.
	var caPEM []byte
	if id, ok := agent.LoadIdentity(*idDir); ok {
		caPEM = id.CAPEM
	} else if bs, berr := agent.LoadBootstrap(bootstrapPathOrDefault(*bootstrapPath)); berr == nil {
		caPEM = []byte(bs.BrokerCA)
	}
	if len(caPEM) == 0 {
		return fmt.Errorf("gateway: no CA found in %s or bootstrap", *idDir)
	}
	return agent.Gateway(ctx, agent.GatewayConfig{Listen: *listen, BrokerURL: bURL, CAPEM: caPEM, IDDir: *idDir})
}

// bootstrapPathOrDefault resolves an empty --bootstrap as resolveBrokerURL
// does, so the gateway's CA fallback reads the same file.
func bootstrapPathOrDefault(path string) string {
	if path != "" {
		return path
	}
	return defaultBootstrapPath()
}

type cliArgs struct {
	verb                            string
	idDir, brokerURL, bootstrapPath string
	tool, op, to, token             string
	constraints                     map[string]string
}

type cliSession struct {
	cliArgs
	id        agent.Identity
	brokerURL string
	client    *http.Client
}

var cliVerbs = map[string]func(context.Context, cliSession) error{
	"request":  cliRequest,
	"delegate": cliDelegate,
	"call":     cliCall,
}

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

// parseCLIArgs parses a verb's flags; extra positional key=value args become
// constraints.
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
// bad argument. These verbs are on $PATH in every jail and mint like the
// capability MCP tool: an empty delegate target would make the broker bind
// the token to the caller and print it as a success. The checks are not
// shared with internal/agent/mcpserver.go because here flags and constraints
// are separate namespaces, so a positional `to=...` is unambiguously a
// constraint.
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

// cliDelegate mints a token bound to another agent and prints it. As in the
// MCP tool, naming yourself routes through the obtain policy, not the
// delegate one, and audits like a self-obtain.
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

// cliCall POSTs a tools/call to the gateway and prints the body before a
// non-200 error surfaces: callers rely on both the output and the non-zero
// exit.
func cliCall(ctx context.Context, s cliSession) error {
	out, err := agent.Call(ctx, s.brokerURL, s.client, s.tool, s.op, s.token, s.constraints)
	fmt.Print(out)
	return err
}

// resolveBrokerURL returns brokerURL if set, else reads it from the bootstrap
// file (bootstrapPathOrDefault).
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

// commonFlags registers the id-dir/broker-url/bootstrap flags of the
// lazy-resolving subcommands. cmdBoot does not use it: it has no
// --broker-url and resolves its --bootstrap default eagerly.
func commonFlags(fs *flag.FlagSet, idDirHelp, bootstrapHelp string) (idDir, brokerURL, bootstrap *string) {
	idDir = fs.String("id-dir", defaultIDDir(), idDirHelp)
	brokerURL = fs.String("broker-url", "", "broker URL (overrides bootstrap)")
	bootstrap = fs.String("bootstrap", "", bootstrapHelp)
	return idDir, brokerURL, bootstrap
}
