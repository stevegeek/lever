package brokerctl

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/lever-to/lever/internal/broker"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/config"
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
	cfg, err := BuildBroker(app, kp, caInst, ca.NewTicketStore())
	if err != nil {
		return err
	}
	cfg.RevocationState = rev
	cfg.PersistRevocation = state.SaveRevocation
	b := broker.New(cfg)

	jailLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.Broker.JailPort))
	if err != nil {
		return fmt.Errorf("brokerctl: bind jail listener: %w", err)
	}
	adminLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.Broker.AdminPort))
	if err != nil {
		_ = jailLn.Close()
		return fmt.Errorf("brokerctl: bind admin listener: %w", err)
	}
	adminURL := "http://" + adminLn.Addr().String()

	certPEM, keyPEM, err := caInst.IssueServerCert(serverName)
	if err != nil {
		_ = jailLn.Close()
		_ = adminLn.Close()
		return err
	}

	logf, err := os.OpenFile(state.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = jailLn.Close()
		_ = adminLn.Close()
		return err
	}
	defer logf.Close()

	sup := NewSupervisor(app.Broker.Tools, adminURL, logf)
	if err := sup.Start(ctx); err != nil {
		_ = jailLn.Close()
		_ = adminLn.Close()
		return err
	}
	defer sup.Stop()

	return b.ServeListeners(ctx, jailLn, adminLn, certPEM, keyPEM)
}
