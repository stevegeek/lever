package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
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

			// Scion project health needs a read into the jail (via the backend).
			// A read error almost always means the jail machine isn't up — report
			// that plainly rather than a marker verdict we can't actually make.
			scion := checkResult{name: "scion project registration"}
			if st, serr := b.ReadScionProjectState(cmd.Context()); serr != nil {
				scion.ok = false
				scion.detail = "could not read scion state from the jail (is the machine up?): " + serr.Error()
				scion.fix = "bring the jail up with `lever apply`, then re-run doctor"
			} else {
				scion = checkScionProject(st, b.MountDest())
			}

			// Reads the hub through the jail, like apply's strip — a host-side
			// call cannot reach the hub on the Lima backend. A down jail or hub
			// is reported as "not checked", not as a finding; see
			// checkProjectSharedDirs.
			hc := &hubapi.Client{T: hubJailTransport(b.JailRunner(), state)}
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
			// The role check needs the scion IN THE JAIL, which is the binary
			// that will actually resolve the stored role — not the host's.
			rolesSupported := brokerctl.HostScionClient(b.JailRunner(), state, app.Scion.AgentRole).RolesSupported

			checks := []checkResult{
				checkBrokerAlive(state, app.EffectiveJailPort(), tcpDial),
				checkAgentCert(state, time.Now()),
				checkToolBackends(app.Broker.Tools, tcpDial),
				checkClaudeVersion(app.ManagerImage(), claudeVersionProbe),
				checkCredentialFile(app.Manager.CredentialFile),
				checkMcpJsonInTree(app.Tree),
				checkGoToolchain(app.Scion),
				checkOperatorSkills(app, state.Dir),
				checkDirectives(app, state),
				checkProjectSharedDirs(cmd.Context(), hubProjectKey(b.MountDest()), listSharedDirs),
				checkAgentRoles(cmd.Context(), hubProjectKey(b.MountDest()), rolesSupported, listAgentRoles),
				scion,
			}
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
