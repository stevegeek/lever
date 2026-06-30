package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lever-to/lever/internal/apply"
	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/backend/orbstack"
	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
	leverexec "github.com/lever-to/lever/internal/exec"
	"github.com/lever-to/lever/internal/jail"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

func newApplyCmd(bf BackendFactory) *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "apply [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Bring an agent-manager application up from a config",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			if dryRun {
				for _, s := range apply.Plan(app, apply.PlanOpts{}) {
					cmd.Printf("  %-16s %s\n", s.Kind, s.Target)
				}
				return nil
			}
			deps, _, _, err := buildApplyDeps(cmd.Context(), app, path, bf)
			if err != nil {
				return err
			}
			if err := apply.Run(cmd.Context(), app, deps); err != nil {
				return err
			}
			cmd.Printf("application %q is up.\n", app.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the bring-up plan and exit")
	return c
}

// buildApplyDeps wires the live dependencies for apply.Run.
// It eagerly calls EnsureUp so the OrbStack backend resolves the in-machine
// run-user and UID before the JailRunner and scion.Client are constructed.
// JailUp is therefore a no-op in the returned Deps — the jail is already
// confirmed up and the user/uid are known.
// configPath is the resolved config file path; it is passed to `lever broker
// serve` and used to locate the broker state dir.
func buildApplyDeps(ctx context.Context, app *config.App, configPath string, bf BackendFactory) (apply.Deps, *orbstack.OrbStack, *scion.Client, error) {
	machine := "lever-" + app.Name
	b := bf(machine)
	ob, ok := b.(*orbstack.OrbStack)
	if !ok {
		return apply.Deps{}, nil, nil, fmt.Errorf("apply currently supports the orbstack backend only")
	}
	allowed := append([]int{app.EffectiveJailPort()}, app.Manager.AllowPorts...)
	closed, warn := app.ClosedInternetEgress()
	if warn != "" {
		// Surface the mixed-mode egress relaxation (R2); not fatal.
		fmt.Fprintf(os.Stderr, "lever: warning: %s\n", warn)
	}
	cfg := backend.Config{
		MachineName:    machine,
		ProjectTree:    app.Tree,
		AllowedPorts:   allowed,
		ScionSource:    app.Scion.Source,
		ScionVersion:   app.Scion.Version,
		ClosedInternet: closed,
	}
	// Bring the jail up now so we can resolve the run-user/uid for the JailRunner.
	if err := ob.EnsureUp(ctx, cfg); err != nil {
		return apply.Deps{}, nil, nil, err
	}
	user, uid := ob.RunUser(), ob.RunUID()
	jr := jail.New(leverexec.RealRunner{}, machine, user, uid)
	sc := scion.New(jr, scion.Options{HubEndpoint: "http://127.0.0.1:8080"})

	state := brokerctl.StateDir(filepath.Dir(configPath))
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", app.EffectiveAdminPort())

	// The jail's resolved host-alias IP (host.orb.internal as seen from the jail).
	// Agents dial the broker by this IP — under closed-internet egress DNS/53 is
	// dropped, so the hostname can't be resolved; the IP is already allowlisted and
	// the broker cert carries it as a SAN. Falls back to the hostname if unresolved.
	aliasV4 := ob.HostAliasV4()
	brokerHost := ob.HostToolAlias() // host.orb.internal (DNS) by default…
	if aliasV4 != "" {
		brokerHost = aliasV4 // …but prefer the resolved IP (no DNS needed)
	}

	return apply.Deps{
		// JailUp is a no-op: buildApplyDeps already brought the jail up
		// (idempotent; resolves user/uid). The apply executor's jail-up step
		// is thus a confirmed no-op here.
		JailUp:    func(context.Context, *config.App) error { return nil },
		LoadImage: func(ctx context.Context, ref string) error { return jail.LoadImage(ctx, machine, user, uid, ref) },
		Scion:     sc,
		JailMount: ob.MountDest(),

		// StartBroker spawns `lever broker serve <config>` detached from the
		// current process group so it outlives the apply invocation.
		StartBroker: func(ctx context.Context) error {
			// Idempotent (M2): if a broker is already serving (re-apply), don't spawn
			// a duplicate — it would fail to bind the ports, die, and clobber
			// broker.pid with a dead PID. A fast single-shot probe (no listener =>
			// instant connection-refused, so no penalty on a fresh apply).
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if req, err := http.NewRequestWithContext(probeCtx, "GET", adminURL+"/epoch", nil); err == nil {
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						return nil // already serving; keep the existing process + PID
					}
				}
			}
			cmd := exec.Command(os.Args[0], "broker", "serve", configPath)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			// Pass the resolved host-alias IP so the broker mints its server cert
			// with that IP as a SAN (agents dial it by IP under closed egress).
			cmd.Env = append(os.Environ(), "LEVER_HOST_ALIAS_IP="+aliasV4)
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("lever broker serve: %w", err)
			}
			// Record PID so the broker can be stopped later.
			pidFile := state.PID()
			if err := os.MkdirAll(filepath.Dir(pidFile), 0o700); err == nil {
				_ = os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o600)
			}
			return nil
		},

		// BrokerHealthy polls GET /epoch until 200 or a ~10s timeout.
		BrokerHealthy: func(ctx context.Context) error {
			deadline := time.Now().Add(10 * time.Second)
			epochURL := adminURL + "/epoch"
			for {
				req, err := http.NewRequestWithContext(ctx, "GET", epochURL, nil)
				if err != nil {
					return err
				}
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						return nil
					}
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("broker did not become healthy within 10s (last err: %v)", err)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(200 * time.Millisecond):
				}
			}
		},

		// MintManagerBootstrap POSTs /bootstrap to obtain the one-time manager
		// enrolment ticket, reads the CA PEM from the state dir, and returns
		// the full BootstrapMaterial.
		MintManagerBootstrap: func(ctx context.Context) (apply.BootstrapMaterial, error) {
			req, err := http.NewRequestWithContext(ctx, "POST", adminURL+"/bootstrap", bytes.NewReader(nil))
			if err != nil {
				return apply.BootstrapMaterial{}, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("broker /bootstrap: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusForbidden {
				// Single-use latch already consumed — signal the mint step to tolerate
				// it (idempotent re-apply against the same broker process).
				return apply.BootstrapMaterial{}, apply.ErrBootstrapLatched
			}
			if resp.StatusCode != http.StatusOK {
				return apply.BootstrapMaterial{}, fmt.Errorf("broker /bootstrap returned %d", resp.StatusCode)
			}
			var result struct {
				Ticket string `json:"ticket"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("broker /bootstrap decode: %w", err)
			}
			caPEM, err := os.ReadFile(state.CACert())
			if err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("reading broker CA cert: %w", err)
			}
			return apply.BootstrapMaterial{
				Ticket:    result.Ticket,
				BrokerCA:  string(caPEM),
				BrokerURL: fmt.Sprintf("https://%s:%d", brokerHost, app.EffectiveJailPort()),
				AgentCN:   app.ManagerCN(),
			}, nil
		},
	}, ob, sc, nil
}
