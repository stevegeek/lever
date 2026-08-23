package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
	"github.com/stevegeek/lever/internal/hubapi"
	scionpkg "github.com/stevegeek/lever/internal/scion"
)

func newDoctorCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag *string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the instance: backend profile, broker, tool backends, agent image version, credential, scion state",
		// A failed health check is a diagnosis, not a usage error — exit non-zero
		// (scriptable) without dumping the command's usage text.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Backend containment profile (informational header) — resolved from
			// the flags/config exactly as before.
			_, b, err := resolveJailBackend(factory, *machine, *backendFlag)
			if err != nil {
				return err
			}
			cmd.Println(b.Profile().Summary())

			// Health checks need the parsed config (broker port, external tools)
			// and the state dir (broker.pid). When there's no config here, doctor
			// is being used away from an instance root (profile-only, via
			// --machine/--backend) — print the profile and stop, don't error. An
			// invalid config that IS present is a real fault and surfaces.
			path, err := resolveConfigPath("")
			if err != nil {
				cmd.Println("(no lever.yaml here — run doctor from an instance root for broker + external-tool checks)")
				return nil
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			state := stateFor(path)

			probes := productionProbes(leverexec.RealRunner{})
			checks := runDoctorChecks(cmd.Context(), app, state, b, probes)
			failed := 0
			for _, c := range checks {
				if c.ok {
					cmd.Printf("✓ %s — %s\n", c.name, c.detail)
					continue
				}
				failed++
				cmd.Printf("✗ %s — %s\n", c.name, c.detail)
				if c.fix != "" {
					cmd.Printf("    fix: %s\n", c.fix)
				}
			}
			if failed > 0 {
				return fmt.Errorf("doctor: %d check(s) failed", failed)
			}
			return nil
		},
	}
	machine, backendFlag = addJailTargetFlags(cmd)
	return cmd
}

// runDoctorChecks evaluates every health check in report order. The order is
// the order an operator reads: process liveness first, then the pieces each
// later check depends on. Tests pin it.
func runDoctorChecks(ctx context.Context, app *config.App, state brokerctl.State, b backend.Backend, probes doctorProbes) []checkResult {
	jr := b.JailRunner()
	project := hubProjectKey(b.MountDest())

	// Reads the hub through the jail, like apply's strip — a host-side call
	// cannot reach the hub on the Lima backend. A down jail or hub is reported
	// as "not checked", not as a finding; see checkProjectSharedDirs.
	hc := &hubapi.Client{T: hubJailTransport(jr, state)}
	listSharedDirs := func(ctx context.Context, project string) ([]hubapi.SharedDir, error) {
		id, err := hc.ProjectID(ctx, project, scionpkg.DefaultHubEndpoint)
		if err != nil {
			return nil, err
		}
		return hc.SharedDirs(ctx, id)
	}
	listAgentRoles := func(ctx context.Context, project string) ([]hubapi.Agent, error) {
		return hc.Agents(ctx, project, scionpkg.DefaultHubEndpoint)
	}
	// The role check needs the scion IN THE JAIL, which is the binary that
	// will actually resolve the stored role — not the host's.
	rolesSupported := brokerctl.HostScionClient(jr, state, app.Scion.AgentRole).RolesSupported

	checks := []func() checkResult{
		func() checkResult { return checkBrokerAlive(state, app.EffectiveJailPort(), probes) },
		func() checkResult { return checkAgentCert(state, time.Now()) },
		func() checkResult { return checkToolBackends(app.Broker.Tools, probes) },
		func() checkResult { return checkClaudeVersion(app.ManagerImage(), probes) },
		func() checkResult { return checkCredentialFile(app.Manager.CredentialFile) },
		func() checkResult { return checkMcpJsonInTree(app.Tree) },
		func() checkResult { return checkGoToolchain(app.Scion, probes) },
		func() checkResult { return checkNodeToolchain(app, probes) },
		func() checkResult { return checkOperatorSkills(app, state.Dir) },
		func() checkResult { return checkDirectives(app, state) },
		func() checkResult { return checkRemote(ctx, app, state, probes, jr) },
		func() checkResult { return checkProjectSharedDirs(ctx, project, listSharedDirs) },
		func() checkResult { return checkAgentRoles(ctx, project, rolesSupported, listAgentRoles) },
		func() checkResult { return checkScionProjectInJail(ctx, b) },
	}
	out := make([]checkResult, 0, len(checks))
	for _, run := range checks {
		out = append(out, run())
	}
	return out
}

// checkScionProjectInJail reads scion's project state through the backend and
// judges it with checkScionProject. A read error almost always means the jail
// machine isn't up — report that plainly rather than a marker verdict doctor
// cannot actually make.
func checkScionProjectInJail(ctx context.Context, b backend.Backend) checkResult {
	st, err := b.ReadScionProjectState(ctx)
	if err != nil {
		return checkResult{
			name:   "scion project registration",
			ok:     false,
			detail: "could not read scion state from the jail (is the machine up?): " + err.Error(),
			fix:    "bring the jail up with `lever apply`, then re-run doctor",
		}
	}
	return checkScionProject(st, b.MountDest())
}
