package brokerctl

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/lever-to/lever/internal/backend/registry"
	"github.com/lever-to/lever/internal/broker"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/config"
	leverexec "github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/scion"
)

// Serve runs the host-side broker for app until ctx is cancelled: ensure keys +
// revocation state, build the broker, pre-bind both loopback listeners (learning
// OS-assigned ports), issue the server cert, supervise the first-party tools, and
// serve. The supervisor is torn down on shutdown.
func Serve(ctx context.Context, app *config.App, state State) error {
	kp, caInst, err := state.EnsureKeys()
	if err != nil {
		return err
	}
	rev, err := state.LoadRevocation()
	if err != nil {
		return err
	}

	// Grove dispatch runs host-side with operator identity (jail runner). apply
	// passes the resolved run-user/uid via env (LEVER_JAIL_USER/UID); the mount
	// dest is a backend constant. Without the env (manual `broker serve` with no
	// prior apply) cfg.Runtime stays nil; the grove handlers detect this via
	// runtimeReady and return 502 — they do not panic. apply is the real path.
	machine := "lever-" + app.Name
	// app.Backend was validated selectable at config.Load, so this cannot pick a
	// planned backend; routing through the registry keeps the mount dest coming
	// from the SELECTED backend rather than a hardwired one.
	be, err := registry.Select(app.Backend, leverexec.RealRunner{}, machine)
	if err != nil {
		return err
	}

	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		return err
	}
	cfg.RevocationState = rev
	cfg.PersistRevocation = state.SaveRevocation
	cfg.ServerName = be.HostToolAlias()

	jailMount := be.MountDest()
	if u, id := os.Getenv("LEVER_JAIL_USER"), os.Getenv("LEVER_JAIL_UID"); u != "" && id != "" {
		jr, jerr := registry.JailRunner(app.Backend, leverexec.RealRunner{}, machine, u, id)
		if jerr != nil {
			return jerr
		}
		cfg.Runtime = scion.New(jr, scion.Options{HubEndpoint: "http://127.0.0.1:8080"})
	}
	cfg.Groves = GroveSpecs(app, jailMount)
	cfg.ManagerProject = jailMount
	cfg.GroveToGrove = app.GroveToGroveMessaging()
	if caPEM, err := os.ReadFile(state.CACert()); err == nil {
		cfg.BrokerCAPEM = string(caPEM)
	} else {
		fmt.Fprintf(os.Stderr, "lever: warning: broker CA read: %v\n", err)
	}
	host := os.Getenv("LEVER_HOST_ALIAS_IP")
	if host == "" {
		host = cfg.ServerName
	}
	cfg.BrokerURL = groveBrokerURL(host, app.EffectiveJailPort())

	// Persist the broker's audit decisions (provision/enrol/request/revoke …) to
	// the state-dir log. Without this the broker defaults to a discard logger, so
	// every allow/deny — the first thing you need when a grove can't enrol — is
	// lost. Opened before broker.New so cfg.Log is set; reused by the supervisor.
	logf, err := os.OpenFile(state.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cfg.Log = slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	b := broker.New(cfg)

	jailLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.EffectiveJailPort()))
	if err != nil {
		return fmt.Errorf("brokerctl: bind jail listener: %w", err)
	}
	adminLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.EffectiveAdminPort()))
	if err != nil {
		_ = jailLn.Close()
		return fmt.Errorf("brokerctl: bind admin listener: %w", err)
	}
	adminURL := "http://" + adminLn.Addr().String()

	// Issue the broker server cert. Always include the selected backend's host
	// alias (cfg.ServerName, e.g. host.orb.internal) as a DNS SAN; additionally
	// include the jail's resolved host-alias IP (passed by `lever apply` via
	// $LEVER_HOST_ALIAS_IP) so agents under closed-internet egress can dial the
	// broker by IP — DNS/53 is dropped in that posture, so they cannot resolve
	// the hostname and instead connect to the already-allowlisted alias IP,
	// which TLS validates against this IP SAN. Absent (e.g. a direct `lever broker
	// serve`), fall back to the hostname-only cert.
	certPEM, keyPEM, err := caInst.IssueServerCertSANs(cfg.ServerName, []string{cfg.ServerName}, []string{os.Getenv("LEVER_HOST_ALIAS_IP")})
	if err != nil {
		_ = jailLn.Close()
		_ = adminLn.Close()
		return err
	}

	sup := NewSupervisor(app.Broker.Tools, adminURL, logf)
	if err := sup.Start(ctx); err != nil {
		_ = jailLn.Close()
		_ = adminLn.Close()
		return err
	}
	defer sup.Stop()

	return b.ServeListeners(ctx, jailLn, adminLn, certPEM, keyPEM)
}
