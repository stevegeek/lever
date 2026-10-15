package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/conduit"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// portAccessPermission is scion's agent.port_access. scion decides a
// tunnel request (scion tunnel, which `lever ssh` runs) as port access, on
// top of agent.attach for the SSH stream itself.
const portAccessPermission = "agent.port_access"

// sshTokenScopes is the tunnel token's exact scope set: attach for the SSH
// stream, port access for the tunnel path, and the reads scion tunnel resolves
// the project and agent with. It is minted for the instance project only.
var sshTokenScopes = []string{"agent:attach", "agent:port_access", "agent:read", "project:read"}

// sshTokenGranted are the scopes a mint must come back with: no alias expands
// to them.
var sshTokenGranted = []string{"agent:attach", "agent:port_access"}

const (
	sshTokenName = "lever-ssh"
	// sshTokenLifetime bounds a session: the hub ends a tunnel when its
	// credential expires, and each connection mints its own token.
	sshTokenLifetime = 12 * time.Hour
	// sshRemoteUser is the user name sent to the agent; its SSH server runs
	// every session as the container's user whatever the name.
	sshRemoteUser = "scion"
	// sshRevokeTimeout bounds the revoke after a session ends.
	sshRevokeTimeout = 30 * time.Second
)

func newSSHCmd(bf BackendFactory) *cobra.Command {
	var configPath string
	var proxy bool
	c := &cobra.Command{
		Use:   "ssh [--config PATH] NAME [-- COMMAND...]",
		Short: "Open an SSH session to the manager or a named worker over the hub's relay (opt-in: ssh.enabled)",
		Long: `Open an SSH session to the manager or a named worker through scion's hub
relay (conduit), with the system ssh client. Off unless lever.yaml sets
ssh.enabled and the jail's scion has the hub's conduit controls; see the SSH
guide. Everything after NAME is the remote command.

With --proxy the session is carried on stdin and stdout instead, as an ssh
ProxyCommand: that is what ` + "`lever ssh-config`" + ` writes for VS Code. Each connection
mints its own tunnel token for the operator, valid for 12 hours, and revokes it
when the connection ends.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote := args[1:]
			if len(remote) > 0 && remote[0] == "--" {
				remote = remote[1:]
			}
			if proxy && len(remote) > 0 {
				return usageError{errors.New("ssh: --proxy takes no remote command")}
			}
			p, err := resolveConfigPath(configPath)
			if err != nil {
				return err
			}
			if p, err = filepath.Abs(p); err != nil {
				return err
			}
			app, err := config.Load(p)
			if err != nil {
				return err
			}
			st := stateFor(p)
			if err := sshAllowed(app, st); err != nil {
				return err
			}
			if !proxy {
				return runSSHClient(cmd, app, st, p, args[0], remote)
			}
			return runSSHProxy(cmd.Context(), bf, app, st, args[0])
		},
	}
	c.Flags().StringVar(&configPath, "config", "", "the instance's lever.yaml (default: lever.yaml in the current directory)")
	c.Flags().BoolVar(&proxy, "proxy", false, "carry the session on stdin and stdout, as an ssh ProxyCommand")
	c.Flags().SetInterspersed(false)
	return c
}

// sshAllowed refuses before anything reaches the jail: ssh off in the
// config, or a state directory an agent can write (it holds the host keys
// the client trusts).
func sshAllowed(app *config.App, st state.State) error {
	if !app.SSHEnabled() {
		return fmt.Errorf("%s is off: set ssh.enabled: true in %s and run `lever apply`", sshOffPrefix, config.CanonicalName)
	}
	inside, err := brokerctl.StateInsideTreeErr(app, st)
	if err != nil || inside {
		return fmt.Errorf("%s: the state directory %s is inside the tree %s (or lever cannot tell), where an agent could rewrite the host keys it pins; point `tree:` at a subdirectory that does not contain %s/",
			sshOffPrefix, st.Dir, app.Tree, state.DirName)
	}
	return nil
}

// sshAgentNames resolves NAME ("" or the app name = manager) to a slug,
// refusing anything that is not one of the instance's agents.
func sshAgentName(app *config.App, name string) (string, error) {
	slug, _, err := attachTarget(app, "", name)
	if err != nil {
		return "", fmt.Errorf("ssh: %s", strings.TrimPrefix(err.Error(), "attach: "))
	}
	return slug, nil
}

// sshHostAlias is the name the client and the known-hosts file know an agent
// by. Both parts match config's name rule, so it is a plain DNS-like label.
func sshHostAlias(app *config.App, slug string) string {
	return "lever." + app.Name + "." + slug
}

// sshClientOptions are the options every `lever ssh` connection runs with,
// interactive or from `lever ssh-config`: the host key pinned under the
// alias in lever's own file, and nothing the agent's end could use against
// the operator's machine (agent and X11 forwarding, local commands, a shared
// control socket, the operator's environment, keys or a password prompt: the
// agent's SSH server asks for no authentication, and a server that does is
// not lever's). Options ssh adds up across blocks (RemoteForward,
// LocalForward, SendEnv, IdentityFile) can still come from a later Host *
// block; the interactive client reads no config file for that reason.
func sshClientOptions(alias, knownHosts, proxyCommand string) [][2]string {
	return [][2]string{
		{"ProxyCommand", proxyCommand},
		{"HostKeyAlias", alias},
		{"UserKnownHostsFile", knownHosts},
		{"GlobalKnownHostsFile", "/dev/null"},
		{"StrictHostKeyChecking", "yes"},
		{"UpdateHostKeys", "no"},
		{"CheckHostIP", "no"},
		{"ForwardAgent", "no"},
		{"ForwardX11", "no"},
		{"ForwardX11Trusted", "no"},
		{"PermitLocalCommand", "no"},
		{"ControlMaster", "no"},
		{"ControlPath", "none"},
		{"SendEnv", "-*"},
		{"IdentitiesOnly", "yes"},
		{"IdentityAgent", "none"},
		{"PubkeyAuthentication", "no"},
		{"PasswordAuthentication", "no"},
		{"KbdInteractiveAuthentication", "no"},
		{"GSSAPIAuthentication", "no"},
		{"HostbasedAuthentication", "no"},
	}
}

// sshProxyCommand is the ProxyCommand line. ssh runs it with the user's
// shell after expanding % tokens, so each path is shell-quoted and its %
// doubled.
func sshProxyCommand(exe, configPath, slug string) (string, error) {
	for _, p := range []string{exe, configPath} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return "", fmt.Errorf("ssh: %q is not a clean absolute path", p)
		}
	}
	q := func(s string) string { return strings.ReplaceAll(shellQuote(s), "%", "%%") }
	return strings.Join([]string{q(exe), "ssh", "--proxy", "--config", q(configPath), "--", slug}, " "), nil
}

// leverExecutable is the absolute path ProxyCommand runs.
var leverExecutable = defaultLeverExecutable

func defaultLeverExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// sshClientArgv is the host ssh argv of an interactive `lever ssh`. -F none
// keeps the operator's ssh config out: a Host * block there could forward
// the operator's agent or ports to the agent's end.
func sshClientArgv(app *config.App, st state.State, configPath, slug string, remote []string) ([]string, error) {
	exe, err := leverExecutable()
	if err != nil {
		return nil, fmt.Errorf("ssh: locating lever: %w", err)
	}
	pc, err := sshProxyCommand(exe, configPath, slug)
	if err != nil {
		return nil, err
	}
	kh, err := sshConfigPath(st.SSHKnownHosts())
	if err != nil {
		return nil, err
	}
	alias := sshHostAlias(app, slug)
	argv := []string{"ssh", "-F", "none"}
	for _, o := range sshClientOptions(alias, kh, pc) {
		argv = append(argv, "-o", o[0]+"="+o[1])
	}
	argv = append(argv, "-o", "BatchMode=yes", "-o", "ClearAllForwardings=yes", "-l", sshRemoteUser, alias)
	return append(argv, remote...), nil
}

func runSSHClient(cmd *cobra.Command, app *config.App, st state.State, configPath, name string, remote []string) error {
	slug, err := sshAgentName(app, name)
	if err != nil {
		return err
	}
	if err := st.EnsureSSHDir(); err != nil {
		return err
	}
	argv, err := sshClientArgv(app, st, configPath, slug, remote)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("ssh: no ssh client on PATH: %w", err)
	}
	c := exec.CommandContext(cmd.Context(), bin, argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The terminal's interrupt belongs to ssh, which gets it too. Caught,
	// not ignored: ssh would inherit an ignored signal.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return cli.WithExitCode(fmt.Errorf("ssh exited with status %d", ee.ExitCode()), ee.ExitCode())
	}
	return err
}

// sshSession is what the proxy found and minted for one connection.
type sshSession struct {
	slug, projectID string
	token           string
	tokenID         string
	revoke          func()
}

func runSSHProxy(ctx context.Context, bf BackendFactory, app *config.App, st state.State, name string) error {
	slug, err := sshAgentName(app, name)
	if err != nil {
		return err
	}
	b, err := bf(app.Backend, machineName(app.Name))
	if err != nil {
		return err
	}
	if err := b.ResolveRunUser(ctx); err != nil {
		return fmt.Errorf("ssh: %w (%v) — run `lever up` first", errJailNotUp, err)
	}
	jr := b.JailRunner()
	if err := conduitReady(ctx, app, st, jailConduit{jr}); err != nil {
		return err
	}
	s, err := openSSHSession(ctx, b, app, st, slug)
	if err != nil {
		return err
	}
	defer s.revoke()
	line, err := jail.HubTokenLine(s.token)
	if err != nil {
		return err
	}
	argv := b.AttachArgv(jail.WithHubTokenFromStdin(conduit.TunnelSSHArgv(jailScionBin, scion.DefaultHubEndpoint, s.projectID, s.slug)))
	return runTunnelChild(argv, io.MultiReader(strings.NewReader(line), os.Stdin), os.Stdout, os.Stderr)
}

// conduitReady refuses unless conduit is on for `lever ssh` the way apply
// left it: a scion with the controls, conduit recorded on, and a live hub
// running with lever's policy. It repeats apply's checks because the jail can
// change between applies.
func conduitReady(ctx context.Context, app *config.App, st state.State, sc conduitScion) error {
	controls, err := sc.ConduitControls(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", sshOffPrefix, err)
	case !controls:
		return errNoConduitControls
	}
	rec, found, err := st.LoadHubExperimentsRecord()
	if err != nil {
		return err
	}
	if !found || !slices.Contains(rec.On, conduitExperiment) {
		return fmt.Errorf("%s: lever has not turned %s on at the hub; run `lever apply` (its warnings, and `lever doctor`, say what keeps it off)", sshOffPrefix, conduitExperiment)
	}
	g := sshGate{policy: apply.ConduitPolicy(app), sc: sc}
	if err := g.policyLive(ctx); err != nil {
		return fmt.Errorf("%s: %v; run `lever apply`", sshOffPrefix, err)
	}
	return nil
}

// openSSHSession mints the connection's token as the controller, records its
// id for apply to revoke should this process die first, and pins the agent's
// host key.
func openSSHSession(ctx context.Context, b backend.Backend, app *config.App, st state.State, slug string) (*sshSession, error) {
	id, found, err := st.LoadHubIdentity()
	if err != nil {
		return nil, err
	}
	if !found || id.ProjectID == "" {
		return nil, fmt.Errorf("%s: lever has no hub identity record for this instance; run `lever apply`", sshOffPrefix)
	}
	key, err := hubauth.LoadKey(st.HubAuthKey())
	if err != nil {
		return nil, fmt.Errorf("ssh: hub assertion key: %w", err)
	}
	instanceID, err := st.ReadInstanceID()
	if err != nil {
		return nil, fmt.Errorf("ssh: %w", err)
	}
	jr := b.JailRunner()
	clock := readGuestClock(ctx, jr, func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) })
	signer := hubauth.NewSigner(key, instanceID, clock)
	assert := func(sign func() (string, error)) *hubapi.Client {
		return &hubapi.Client{T: &hubapi.AssertCurl{Runner: jr, BaseURL: scion.DefaultHubEndpoint, Sign: sign}}
	}
	h := &hubIdentity{st: st, jr: jr, admin: assert(signer.Admin), ctl: assert(signer.Controller), guestNow: clock.Now}
	if _, err := h.assertController(ctx, id.Mode); err != nil {
		return nil, fmt.Errorf("ssh: %w", err)
	}
	if err := st.EnsureSSHDir(); err != nil {
		return nil, err
	}
	secret, tok, err := h.ctl.CreateToken(ctx, sshTokenName, id.ProjectID, sshTokenScopes, clock.Now().Add(sshTokenLifetime))
	if err != nil {
		return nil, fmt.Errorf("ssh: minting the tunnel token: %w", err)
	}
	s := &sshSession{slug: slug, projectID: id.ProjectID, token: secret, tokenID: tok.ID}
	s.revoke = func() {
		rctx, cancel := context.WithTimeout(context.Background(), sshRevokeTimeout)
		defer cancel()
		if _, err := h.assertController(rctx, id.Mode); err == nil {
			if err := h.ctl.RevokeToken(rctx, tok.ID); err == nil || errors.Is(err, hubapi.ErrNotFound) {
				_ = st.ForgetSSHToken(tok.ID)
				return
			}
		}
		fmt.Fprintf(os.Stderr, "lever ssh: the tunnel token %s stays valid until %s or the next `lever apply` with ssh off\n", tok.ID, expiryText(tok.ExpiresAt))
	}
	if err := st.RecordSSHToken(tok.ID); err != nil {
		s.revoke()
		return nil, err
	}
	for _, want := range sshTokenGranted {
		if !slices.Contains(tok.Scopes, want) {
			s.revoke()
			return nil, fmt.Errorf("ssh: the hub minted the tunnel token without %s; the %s role needs %s, which `lever apply` grants while ssh is on", want, controllerRoleName, portAccessPermission)
		}
	}
	hc := &hubapi.Client{T: hubJailTransport(jr, st)}
	agentID, err := hc.AgentID(ctx, path.Base(b.MountDest()), scion.DefaultHubEndpoint, slug)
	if err != nil {
		s.revoke()
		return nil, fmt.Errorf("ssh: %w", err)
	}
	raw, err := hc.AgentSSHHostKey(ctx, agentID)
	if err != nil {
		s.revoke()
		return nil, fmt.Errorf("ssh: reading %s's host key: %w", slug, err)
	}
	if raw == "" {
		s.revoke()
		return nil, fmt.Errorf("ssh: %s has reported no SSH host key: it was started before conduit was on, or its sciontool serves no SSH; restart it (`lever stop` and `lever up` for the manager)", slug)
	}
	hostKey, err := parseSSHHostKey(raw)
	if err != nil {
		s.revoke()
		return nil, fmt.Errorf("ssh: %s's host key: %w", slug, err)
	}
	if err := pinSSHHostKey(st.SSHKnownHosts(), sshHostAlias(app, slug), hostKey); err != nil {
		s.revoke()
		return nil, err
	}
	return s, nil
}

// runTunnelChild runs the in-jail tunnel on the given streams. ssh ends its
// ProxyCommand with SIGHUP or SIGTERM; those are passed on, so the token is
// revoked after the child exits rather than left behind by a killed lever.
func runTunnelChild(argv []string, in io.Reader, out, errw io.Writer) error {
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("ssh: %w", err)
	}
	// WaitDelay: the stdin copy blocks on ssh's open pipe after the tunnel
	// exits, and ssh waits on the tunnel's output, so Wait must not wait for it.
	c := &exec.Cmd{Path: bin, Args: argv, Stdin: in, Stdout: out, Stderr: errw, WaitDelay: 2 * time.Second}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := c.Start(); err != nil {
		return fmt.Errorf("ssh: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	for {
		select {
		case sig := <-sigs:
			_ = c.Process.Signal(sig)
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() > 0 {
				return cli.WithExitCode(fmt.Errorf("ssh: the tunnel exited with status %d", ee.ExitCode()), ee.ExitCode())
			}
			if err != nil {
				return fmt.Errorf("ssh: the tunnel: %w", err)
			}
			return nil
		}
	}
}

// sshKeyTypes are the host key types a known-hosts pin may carry.
var sshKeyTypes = []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa"}

// parseSSHHostKey checks an agent-reported host key and returns it as
// "<type> <base64>". The agent chose the value, so anything beyond a known
// type and a well-formed key blob of that type is refused: it is written
// into a known-hosts file.
func parseSSHHostKey(raw string) (string, error) {
	f := strings.Fields(raw)
	if len(f) < 2 || len(f) > 3 || len(raw) > 16<<10 {
		return "", errors.New("not an authorized-keys line")
	}
	if !slices.Contains(sshKeyTypes, f[0]) {
		return "", fmt.Errorf("key type %q is not one lever pins", f[0])
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(f[1])
	if err != nil {
		return "", errors.New("the key is not base64")
	}
	if len(blob) < 4 {
		return "", errors.New("the key blob is truncated")
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) || string(blob[4:4+n]) != f[0] {
		return "", errors.New("the key blob does not match its type")
	}
	return f[0] + " " + f[1], nil
}

// pinSSHHostKey sets alias's line in the known-hosts file to key, keeping
// every other line. The file and its directory are the operator's own (0600
// in a 0700 directory).
func pinSSHHostKey(file, alias, key string) error {
	old, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ssh: reading %s: %w", file, err)
	}
	var out bytes.Buffer
	for _, line := range strings.Split(string(old), "\n") {
		if line == "" {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 && f[0] == alias {
			continue
		}
		out.WriteString(line + "\n")
	}
	out.WriteString(alias + " " + key + "\n")
	if err := fsutil.WriteFileAtomic(file, out.Bytes(), 0o600); err != nil {
		return fmt.Errorf("ssh: writing %s: %w", file, err)
	}
	return nil
}
