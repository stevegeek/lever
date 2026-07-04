package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lever-to/lever/internal/apply"
	"github.com/lever-to/lever/internal/backend"
	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

// brokerServeCmd builds the detached `lever broker serve` command: its OWN
// session (Setsid — survives the parent terminal/session, no controlling TTY),
// stdout+stderr appended to outLog (so a bind failure or panic is inspectable,
// not discarded), and the env the broker needs to issue its cert + reach the
// jail. The pid file is written by the serve process itself (Task 1), not here.
func brokerServeCmd(self, configPath, outLog, aliasV4, runUser, runUID string) (*exec.Cmd, *os.File, error) {
	// On a fresh apply the state dir (.lever-state) does not exist yet — it's
	// created by EnsureKeys inside the spawned child, too late for this open —
	// so create the log's parent here or the whole bring-up hard-fails at
	// broker-up before the broker is ever spawned.
	if err := os.MkdirAll(filepath.Dir(outLog), 0o700); err != nil {
		return nil, nil, fmt.Errorf("broker out log dir: %w", err)
	}
	f, err := os.OpenFile(outLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("broker out log: %w", err)
	}
	cmd := exec.Command(self, "broker", "serve", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = append(os.Environ(),
		"LEVER_HOST_ALIAS_IP="+aliasV4,
		"LEVER_JAIL_USER="+runUser,
		"LEVER_JAIL_UID="+runUID,
	)
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd, f, nil
}

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
			// State the containment posture every bring-up runs under, so the
			// selected backend's guarantees are visible, not assumed.
			if p, ok := backend.ProfileFor(app.Backend); ok {
				cmd.Printf("backend: %s\n", p.Summary())
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
// It eagerly calls EnsureUp so the backend resolves the in-machine
// run-user and UID before the JailRunner and scion.Client are constructed.
// JailUp is therefore a no-op in the returned Deps — the jail is already
// confirmed up and the user/uid are known.
// configPath is the resolved config file path; it is passed to `lever broker
// serve` and used to locate the broker state dir.
func buildApplyDeps(ctx context.Context, app *config.App, configPath string, bf BackendFactory) (apply.Deps, backend.Backend, *scion.Client, error) {
	machine := "lever-" + app.Name
	b, err := bf(app.Backend, machine)
	if err != nil {
		return apply.Deps{}, nil, nil, err
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
	if err := b.EnsureUp(ctx, cfg); err != nil {
		return apply.Deps{}, nil, nil, err
	}
	jr := b.JailRunner()
	sc := scion.New(jr, scion.Options{HubEndpoint: "http://127.0.0.1:8080"})

	state := brokerctl.StateDir(filepath.Dir(configPath))
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", app.EffectiveAdminPort())

	// The jail's resolved host-alias IP (host.orb.internal as seen from the jail).
	// Agents dial the broker by this IP — under closed-internet egress DNS/53 is
	// dropped, so the hostname can't be resolved; the IP is already allowlisted and
	// the broker cert carries it as a SAN. Falls back to the hostname if unresolved.
	aliasV4 := b.HostAliasV4()
	brokerHost := b.HostToolAlias() // host.orb.internal (DNS) by default…
	if aliasV4 != "" {
		brokerHost = aliasV4 // …but prefer the resolved IP (no DNS needed)
	}

	return apply.Deps{
		// JailUp is a no-op: buildApplyDeps already brought the jail up
		// (idempotent; resolves user/uid). The apply executor's jail-up step
		// is thus a confirmed no-op here.
		JailUp: func(context.Context, *config.App) error { return nil },
		LoadImage: func(ctx context.Context, ref string) error {
			return b.LoadImage(ctx, ref)
		},
		Scion:     sc,
		JailMount: b.MountDest(),

		// RemoveJailFile removes a regular file at a jail-absolute path THROUGH
		// the jail runner, so the removal shares the jail's own filesystem view
		// with the `scion init` that follows it in the register step (see the
		// comment at the register-manager/register-grove case in
		// internal/apply/run.go for the VirtioFS unlink/init race this closes).
		// The guard leaves directories untouched and is a no-op if the path is
		// already absent, mirroring removeStaleMarker's host-side semantics.
		RemoveJailFile: func(ctx context.Context, jailPath string) error {
			const script = `if [ ! -d "$1" ] && [ -e "$1" ]; then rm -f -- "$1"; fi`
			if _, err := jr.Run(ctx, nil, "sh", "-c", script, "_", jailPath); err != nil {
				return fmt.Errorf("removing stale marker %s in jail: %w", jailPath, err)
			}
			return nil
		},

		// RemoveScionProjectConfigs clears any stale ~/.scion/project-configs
		// registration(s) for a workspace path before the register step re-inits
		// (see internal/apply/run.go's register-manager/register-grove case) —
		// keeps apply from accumulating a duplicate registration every run.
		RemoveScionProjectConfigs: func(ctx context.Context, wp string) error {
			return b.RemoveScionProjectConfigs(ctx, wp)
		},

		// ScionProjectRegistered observes whether the register-manager/register-
		// grove apply step (internal/apply/run.go) even needs to run its
		// destructive clean+init path — see RemoveScionProjectConfigs's comment
		// above for why that path exists; this is the idempotency gate that
		// decides whether to run it at all, so a re-apply stops orphaning a
		// resumable scion agent record.
		ScionProjectRegistered: func(ctx context.Context, wp string) (bool, error) {
			return b.ScionProjectRegistered(ctx, wp)
		},

		// StartBroker spawns `lever broker serve <config>` as a daemonized child
		// (its own session, via brokerServeCmd) so it outlives the apply invocation.
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
			cmd, logf, err := brokerServeCmd(os.Args[0], configPath, state.OutLog(), aliasV4, b.RunUser(), b.RunUID())
			if err != nil {
				return err
			}
			// Keep the log fd owned by the child; close our copy after Start.
			defer logf.Close()
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("lever broker serve: %w", err)
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
	}, b, sc, nil
}
