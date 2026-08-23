package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/scion"
)

func newRemoteCmd(bf BackendFactory) *cobra.Command {
	c := &cobra.Command{Use: "remote", Short: "Run / inspect the remote-access proxy (tailnet-facing)"}
	c.AddCommand(newRemoteServeCmd(bf), newRemoteStatusCmd())
	return c
}

func newRemoteServeCmd(bf BackendFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "serve [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Run the remote-access proxy (foreground)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			if !app.RemoteEnabled() {
				return fmt.Errorf("remote access is disabled — set remote.enabled: true in the config")
			}
			// Orbstack-only for now — but NOT because of how the proxy
			// reaches the hub. It dials through the jail
			// (remoteproxy.JailDial), which is backend-agnostic and needs no
			// guest→host forwarding at all. The gate stays because the Lima
			// path has never been live-validated; lifting it is a live-test
			// question, not a code one. Same wording in config.validateRemote,
			// which fires first for every path that loads config.
			if app.Backend != "orbstack" {
				return fmt.Errorf("remote access requires the orbstack backend in v1 (the Lima path is not live-validated yet)")
			}
			state := stateFor(path)

			// The hub's address INSIDE the guest, which is where the dialer
			// lands — so this is also the correct Host header. It is
			// deliberately not a host-reachable address: see
			// scion.DefaultHubEndpoint.
			target, err := url.Parse(scion.DefaultHubEndpoint)
			if err != nil {
				return err
			}
			auditFn, auditCloser, err := remoteproxy.OpenAudit(state.RemoteAudit())
			if err != nil {
				return err
			}
			defer auditCloser.Close()

			// One dialler for both the proxied traffic and the login
			// handshake: the handshake IS hub traffic, and must travel the
			// same route into this instance's own jail.
			dial := remoteproxy.JailDial(jailPrefixFn(bf, app.Backend, machineName(app.Name), cmd.ErrOrStderr()))

			// The local OIDC provider, and the driver that logs in with it.
			// Both live in THIS process because an authorization code must be
			// mintable only by an in-process call — see the provider's own
			// documentation for what that property is holding up. The guest
			// reaches this listener through the forwarder `lever apply`
			// installed (internal/backend/guest.EnsureHubLogin).
			provider := remoteproxy.NewProvider(remoteproxy.ProviderConfig{
				Port: app.EffectiveRemoteLoginPort(),
				// The hub dials the GUEST port; the forwarder carries it here.
				// Two numbers on purpose — see backend.GuestLoginIssuerPort.
				IssuerPort: backend.GuestLoginIssuerPort,
				Audit:      auditFn,
			})
			login := remoteproxy.NewLoginDriver(remoteproxy.LoginConfig{
				Hub:         target,
				DialContext: dial,
				Provider:    provider,
				Audit:       auditFn,
			})

			handler := remoteproxy.NewHandler(remoteproxy.Config{
				Target:      target,
				DialContext: dial,
				PAT:         func() string { pat, _ := state.LoadRemotePAT(); return pat },
				ServeHost:   remoteServeHost(app.Remote.BaseURL),
				// So the Host gate admits `lever doctor`'s loopback /healthz
				// probe without widening the allowlist beyond this one port.
				ListenPort:   app.EffectiveRemotePort(),
				AllowedUsers: app.Remote.AllowedUsers,
				Session:      login,
				Audit:        auditFn,
			})

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			port := app.EffectiveRemotePort()
			cmd.Printf("remote proxy %q serving on 127.0.0.1:%d (login provider on 127.0.0.1:%d, issuer %s)\n",
				app.Name, port, provider.Port(), provider.IssuerURL())
			return remoteproxy.Serve(ctx, remoteproxy.ServeConfig{
				Port:     port,
				Handler:  handler,
				PIDPath:  state.RemotePID(),
				Provider: provider,
				// Record the config THIS process actually loaded, not the one
				// whoever started it believes is running. `lever apply` reuses
				// a live proxy only when this record matches the config it is
				// applying, and apply is not the only thing that starts
				// proxies — this command is reachable by hand, and the pid
				// file apply reads is written by every serve. Stamping here is
				// what stops a hand-started proxy from inheriting the record
				// of an apply-started one. See ServeConfig.Stamp.
				Stamp: func() error {
					return state.WriteRemoteStamp(versionString(), brokerctl.RemoteConfigHash(app))
				},
			})
		},
	}
}

// jailResolveTimeout bounds one attempt to read the jail's run user. The
// probe is three short commands into a running machine; a machine that is
// wedged must fail the dial rather than hold a request open.
const jailResolveTimeout = 15 * time.Second

// jailPrefixFn resolves the argv prefix that reaches inside THIS instance's
// jail, for remoteproxy.JailDial. On OrbStack that prefix embeds the machine's
// run user, so resolving it means talking to the machine.
//
// Resolution is deferred to the first dial, not done at startup: the proxy is
// a long-lived process started alongside the rest of the instance, and
// demanding a running jail before it will serve would make startup order
// load-bearing and turn a stopped jail into a dead proxy. A failed resolve
// returns nil — JailDial renders that as an actionable dial error — and the
// next request tries again.
//
// Success is cached because resolving costs three commands into the machine
// (`orb list`, `whoami`, `id -u`), which is real latency to pay per
// connection, and the run user cannot change under a running machine. A
// rebuilt jail with a different run user therefore needs a proxy restart — and
// `lever apply` does NOT give you one: its reuse check compares the lever
// version, the `remote:` block, the instance name and the backend
// (brokerctl.RemoteConfigHash), none of which a jail rebuild changes, so a
// matching stamp keeps the old process and its stale prefix. Use `lever stop`
// + `lever up` after rebuilding the jail. (Verified against
// remoteController.Start; the comment used to claim the opposite.)
//
// Concurrent dials share one attempt rather than queueing behind each other:
// a browser opens several connections at once, and a wedged machine would
// otherwise hold the second caller for two timeouts, the third for three, and
// so on — the opposite of failing the dial promptly.
//
// warn receives resolve failures, deduplicated for the same reason: the
// proxy's stderr is remote.log, and ten parallel connections to a down jail
// must not write ten identical lines per page load.
func jailPrefixFn(bf BackendFactory, backendName, machine string, warn io.Writer) func() []string {
	var (
		mu       sync.Mutex
		cached   []string
		inflight chan struct{} // non-nil while one attempt is running
		lastWarn string
	)
	report := func(err error) { // call with mu held
		if warn == nil || err.Error() == lastWarn {
			return
		}
		lastWarn = err.Error()
		fmt.Fprintf(warn, "lever: remote proxy cannot reach jail %s: %v\n", machine, err)
	}
	resolve := func() ([]string, error) {
		b, err := bf(backendName, machine)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), jailResolveTimeout)
		defer cancel()
		if err := b.ResolveRunUser(ctx); err != nil {
			return nil, err
		}
		return registry.JailArgv(backendName, machine, b.RunUser())
	}
	return func() []string {
		mu.Lock()
		if cached != nil {
			defer mu.Unlock()
			return cached
		}
		if wait := inflight; wait != nil {
			// Another dial is already asking the machine. Take its answer —
			// including its failure — instead of probing again.
			mu.Unlock()
			<-wait
			mu.Lock()
			defer mu.Unlock()
			return cached
		}
		done := make(chan struct{})
		inflight = done
		mu.Unlock()

		argv, err := resolve()

		mu.Lock()
		if err != nil {
			report(err)
		} else {
			cached = argv
		}
		inflight = nil
		close(done)
		out := cached
		mu.Unlock()
		return out
	}
}

// remoteServeHost derives the proxy's ServeHost from the configured
// base_url: url.Parse(...).Host, which includes the port when base_url
// carries one — the Handler matches a request's Origin host:port exactly,
// ignoring scheme (see remoteproxy.Config.ServeHost). base_url is REQUIRED
// whenever remote is enabled — config.Load's validateRemote rejects an
// enabled block with an empty or malformed base_url, so this function only
// ever sees a well-formed https URL when called from `remote serve` on a
// config that actually loaded. The empty/unparsable-input branches below
// are defensive (an empty ServeHost fails every request closed — see
// proxy.go — rather than risk a silent empty-string Origin match), kept so
// this stays a total function rather than assuming its caller's invariant.
func remoteServeHost(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func newRemoteStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Show the remote-access proxy's status",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			state := stateFor(path)
			port := app.EffectiveRemotePort()

			pid, found, alive := remotePIDStatus(state.RemotePID())
			switch {
			case !found:
				cmd.Println("proxy: not running (no remote.pid)")
			case !alive:
				cmd.Printf("proxy: not running (remote.pid names pid %d, but that process is gone)\n", pid)
			default:
				addr := fmt.Sprintf("127.0.0.1:%d", port)
				if err := tcpDial(addr); err != nil {
					cmd.Printf("proxy: pid %d recorded but nothing is listening on %s\n", pid, addr)
				} else {
					cmd.Printf("proxy: running, pid %d, listening on %s\n", pid, addr)
				}
			}

			cmd.Printf("tailscale command: tailscale serve --bg --https=443 http://127.0.0.1:%d\n", port)
			// The provider's port is worth printing because it is the second
			// host listener this instance owns: a second remote-enabled
			// instance needs its own, and config validation can only catch a
			// collision within one instance. `lever doctor` is what reports
			// whether it is actually healthy.
			cmd.Printf("login provider port: %d on host loopback; the jail reaches it from 127.0.0.1:%d\n",
				app.EffectiveRemoteLoginPort(), backend.GuestLoginIssuerPort)

			if app.Remote.BaseURL != "" {
				cmd.Printf("serve URL: %s\n", app.Remote.BaseURL)
			} else {
				// Reachable only with remote disabled (or unconfigured): an
				// enabled block with no base_url is rejected at config load
				// (validateRemote), so this can no longer describe a proxy
				// that's up and 403ing everything — remote access simply
				// isn't turned on yet.
				cmd.Println("base_url not set — remote access needs both `remote.enabled: true` and `remote.base_url` (the tailnet serve hostname) set in lever.yaml; set both, then `lever apply`")
			}

			if _, err := os.Stat(state.RemotePAT()); err == nil {
				cmd.Println("remote PAT: present")
			} else {
				cmd.Println("remote PAT: absent — run `lever apply` to mint it")
			}
			return nil
		},
	}
}

// remotePIDStatus reads the recorded proxy pid at path and reports whether
// that process is currently alive. Same read-only signal-0 technique as
// brokerctl.State.BrokerPIDStatus, duplicated here rather than shared
// because that method is hardwired to the broker's own pid path, not an
// arbitrary one.
//
//   - found=false: no pid file (proxy never started, or cleanly stopped).
//   - found=true, alive=false: a stale pid file — the process is gone (or
//     the file is garbage).
//   - found=true, alive=true: the recorded process is running.
func remotePIDStatus(path string) (pid int, found, alive bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, true, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return pid, true, false
	}
	return pid, true, proc.Signal(syscall.Signal(0)) == nil
}
