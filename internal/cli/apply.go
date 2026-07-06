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

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/scion"
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
			deps, _, _, err := buildApplyDeps(cmd.Context(), app, path, bf, cmd)
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
// cmd is the invoking cobra command, used only to wire Deps.Log (a loud,
// user-facing progress line — see apply.Deps.Log); may be nil (e.g. tests
// that never exercise a Log-emitting path), in which case Log falls back to
// stderr.
func buildApplyDeps(ctx context.Context, app *config.App, configPath string, bf BackendFactory, cmd *cobra.Command) (apply.Deps, backend.Backend, *scion.Client, error) {
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

	// startBroker spawns `lever broker serve <config>` as a daemonized child
	// (its own session, via brokerServeCmd) so it outlives the apply
	// invocation. Named (rather than inlined in the Deps literal below) so
	// RearmBootstrap can reuse it verbatim instead of duplicating the
	// broker-start logic.
	startBroker := func(ctx context.Context) error {
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
	}

	// brokerHealthy polls GET /epoch until 200 or a ~10s timeout. Named (see
	// startBroker's comment) so RearmBootstrap can reuse it.
	brokerHealthy := func(ctx context.Context) error {
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
	}

	// mintManagerBootstrap POSTs /bootstrap to obtain the one-time manager
	// enrolment ticket, reads the CA PEM from the state dir, and returns the
	// full BootstrapMaterial. Named (see startBroker's comment) so
	// RearmBootstrap can reuse it instead of duplicating the mint logic.
	mintManagerBootstrap := func(ctx context.Context) (apply.BootstrapMaterial, error) {
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

		StartBroker:          startBroker,
		BrokerHealthy:        brokerHealthy,
		MintManagerBootstrap: mintManagerBootstrap,

		// RearmBootstrap backs Deps.RearmBootstrap (see its doc in
		// internal/apply/run.go): start-manager's create path calls this when
		// no fresh bootstrap material was minted this apply (mint-manager-
		// bootstrap tolerated a spent latch), because a freshly-created scion
		// agent record has no agent home to reuse and so ALWAYS re-enrols —
		// against a spent latch that would 403.
		//
		// Reuses the exact same broker-start/health/mint closures as the
		// broker-up and mint-manager-bootstrap steps (no duplicated broker-
		// start logic): stop the (possibly still-running) broker so its next
		// start re-arms the single-use latch — the CA and signing keys live on
		// disk in the state dir and are untouched by a process restart, so
		// existing agent certs and capability tokens keep working — then start
		// it fresh, wait for it to become healthy, mint, and stage the result
		// into app.Tree/.lever/bootstrap.json via the same StageBootstrapMaterial
		// helper the mint-manager-bootstrap step itself uses (one staging code
		// path). Staging happens HERE (not in apply/run.go) because start-
		// manager's Step.Target is the manager's slug, not the tree dir — this
		// closure is the only place that has app.Tree in scope.
		RearmBootstrap: func(ctx context.Context) (apply.BootstrapMaterial, error) {
			if err := state.StopBroker(); err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("stopping the broker to re-arm its bootstrap latch: %w", err)
			}
			if err := startBroker(ctx); err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("restarting the broker to re-arm its bootstrap latch: %w", err)
			}
			if err := brokerHealthy(ctx); err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("waiting for the re-armed broker to become healthy: %w", err)
			}
			m, err := mintManagerBootstrap(ctx)
			if err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("minting bootstrap material from the re-armed broker: %w", err)
			}
			if err := apply.StageBootstrapMaterial(app.Tree, m); err != nil {
				return apply.BootstrapMaterial{}, fmt.Errorf("staging re-armed bootstrap material: %w", err)
			}
			return m, nil
		},

		// Log surfaces start-manager's loud resume-failed recovery notice (see
		// apply.Deps.Log) on the invoking command's stderr, mirroring how other
		// user-facing warnings already surface (cmd.PrintErrf; see cli/stop.go,
		// cli/down.go). A nil cmd (defence in depth for any caller that doesn't
		// have one, e.g. a future direct test) falls back to os.Stderr so the
		// line is never silently lost.
		Log: func(format string, args ...any) {
			if cmd != nil {
				cmd.PrintErrf(format+"\n", args...)
				return
			}
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}, b, sc, nil
}
