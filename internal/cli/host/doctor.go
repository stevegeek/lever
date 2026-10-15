package host

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/jailabi"
	"github.com/stevegeek/lever/internal/proc"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
	"github.com/stevegeek/lever/internal/wire"
)

func newDoctorCmd(factory BackendFactory) *cobra.Command {
	var machine, backendFlag *string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the instance: backend profile, broker, tool backends, agent image version, credential, scion state",
		// A failed check is a diagnosis: exit non-zero without the usage text.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, b, err := resolveJailBackend(factory, *machine, *backendFlag)
			if err != nil {
				return err
			}
			cmd.Println(b.Profile().Summary())

			// Without a config here doctor runs away from an instance root
			// (--machine/--backend): print the profile and stop. An invalid config that is
			// present is a real fault.
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

			probes := productionProbes(proc.RealRunner{})
			checks := runDoctorChecks(cmd.Context(), app, state, b, probes)
			if failed := printDoctorReport(cmd, checks); failed > 0 {
				return fmt.Errorf("doctor: %d check(s) failed", failed)
			}
			return nil
		},
	}
	machine, backendFlag = addJailTargetFlags(cmd)
	return cmd
}

// printDoctorReport prints one row per check and returns how many failed. It
// is the single point where check text reaches the terminal, so guest-supplied
// content (phase, container status, slug, scion's stderr) is sanitized here.
func printDoctorReport(cmd *cobra.Command, checks []checkResult) int {
	failed := 0
	for _, c := range checks {
		name, detail := termsafe.Sanitize(c.name), termsafe.Sanitize(c.detail)
		if c.ok && c.fix != "" {
			// A warning row: shown with its fix, not counted.
			cmd.Printf("! %s — %s\n", name, detail)
			cmd.Printf("    fix: %s\n", termsafe.Sanitize(c.fix))
			continue
		}
		if c.ok {
			cmd.Printf("✓ %s — %s\n", name, detail)
			continue
		}
		failed++
		cmd.Printf("✗ %s — %s\n", name, detail)
		if c.fix != "" {
			cmd.Printf("    fix: %s\n", termsafe.Sanitize(c.fix))
		}
	}
	return failed
}

// runDoctorChecks evaluates every check in the order an operator reads:
// process liveness first, then each later check's prerequisites. Tests pin it.
func runDoctorChecks(ctx context.Context, app *config.App, state state.State, b backend.Backend, probes doctorProbes) []checkResult {
	// The jail runner sets XDG_RUNTIME_DIR from the run user's uid, and rootless
	// podman finds agent containers only under the right one; the default (501,
	// the OrbStack host uid) is wrong on Lima (1000). A down machine keeps the
	// default and the checks that need it report it.
	_ = b.ResolveRunUser(ctx)
	jr := b.JailRunner()
	project := hubProjectKey(b.MountDest())

	// The hub is read through the jail: a host-side call cannot reach it on Lima.
	// A down jail or hub is "not checked", not a finding.
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
	measureAgentList := func(ctx context.Context, project string) (int, error) {
		// %v, not %w: a project list over its cap must not read as an agent
		// list over its own.
		if _, err := hc.ProjectID(ctx, project, scionpkg.DefaultHubEndpoint); err != nil {
			return 0, fmt.Errorf("looking up project %s: %v", project, err)
		}
		_, largest, err := hc.AgentsLargestPage(ctx, project, scionpkg.DefaultHubEndpoint)
		return largest, err
	}
	readCeiling := func(ctx context.Context, project string) (hubapi.RoleCeiling, error) {
		return hc.AgentRoleCeiling(ctx, project, scionpkg.DefaultHubEndpoint)
	}
	// The role check needs the jail's scion: the binary that resolves the stored
	// role.
	jailScion := brokerctl.HostScionClient(jr, state, app.Scion.AgentRole)
	rolesSupported := jailScion.RolesSupported
	hubAuth := newDoctorHubAuth(ctx, jr, state)
	// scion's own listing, not the hub's: it carries phase and container status
	// and names the project by its in-jail path.
	listAgents := func(ctx context.Context, project string) ([]scionpkg.Agent, error) {
		return jailScion.List(ctx, project)
	}
	inspectMounts := func(ctx context.Context, containerID string) ([]string, error) {
		return jail.ContainerMountTargets(ctx, jr, containerID)
	}
	inspectMountsRW := func(ctx context.Context, ref string) ([]jail.Mount, error) {
		return jail.ContainerMounts(ctx, jr, ref)
	}
	recordVolumes := func(ctx context.Context, project, agent string) ([]jail.Mount, error) {
		return hubRecordVolumes(ctx, hc, project, agent)
	}
	// The live half reads from the guest side, never a program in the container.
	liveMounts := func(ctx context.Context, ref string, want []jail.LiveMount) ([]jail.LiveMountProblem, error) {
		return jail.ContainerLiveMounts(ctx, jr, ref, want)
	}
	probeDevice := func(ctx context.Context, ref, path string) (bool, error) {
		return jail.ContainerHasCharDevice(ctx, jr, ref, path)
	}
	inspectNetMode := func(ctx context.Context, ref string) (string, error) {
		return jail.ContainerNetworkMode(ctx, jr, ref)
	}
	// One variable of a container's env, filtered in the guest (see
	// jail.ContainerEnvValue).
	readContainerEnv := func(ctx context.Context, ref, key string) (string, bool, error) {
		return jail.ContainerEnvValue(ctx, jr, ref, key)
	}
	// Only a time and a boolean come back from the agent's container.
	agentSession := jail.AgentProbe{R: jr}
	guestKernel := sync.OnceValues(func() (guestKernelProbe, error) { return readGuestKernelProbe(ctx, jr) })
	workerNames := make([]string, 0, len(app.Workers))
	workerDirs := make(map[string]string, len(app.Workers))
	workerClaude := make(map[string]*wire.Claude, len(app.Workers))
	for _, w := range app.Workers {
		workerNames = append(workerNames, w.Name)
		workerDirs[w.Name] = app.WorkerDir(w)
		workerClaude[w.Name] = w.Claude()
	}

	checks := []func() checkResult{
		func() checkResult { return checkBrokerAlive(state, app.EffectiveJailPort(), probes) },
		func() checkResult {
			return checkInstancePorts(app, state, instancePortsProbesFor(ctx, state, probes))
		},
		func() checkResult {
			return checkManagerLive(ctx, b.MountDest(), app.Name, listAgents, agentSession, agentSession.LogTail, time.Now())
		},
		func() checkResult {
			return checkAgentHubTokens(ctx, b.MountDest(), app.Name, networkCheckedAgents(app.Name, workerNames), listAgents, agentSession)
		},
		func() checkResult {
			return checkClaudeSettings(ctx, b.MountDest(), claudeAgents(app.Name, app.ManagerClaude(), workerNames, workerClaude), listAgents, agentSession.ClaudeSettings)
		},
		func() checkResult {
			return checkBackgroundSessions(ctx, b.MountDest(), networkCheckedAgents(app.Name, workerNames), listAgents, agentSession.BackgroundSessions)
		},
		func() checkResult { return checkGuestDNS(ctx, app.ClosedInternetEgress(), jr) },
		func() checkResult {
			return checkPodmanStorage(ctx, func(ctx context.Context) (jail.StoreHealth, error) { return jail.ReadStoreHealth(ctx, jr) })
		},
		func() checkResult {
			return checkNestedVirt(ctx, app.NestedVirt, app.Backend == config.BackendLima, jr, jail.ContainerName(hubProjectKey(b.MountDest()), app.Name), probeDevice)
		},
		func() checkResult {
			p, err := guestKernel()
			return checkGuestCrashRecovery(app.Backend, p, err)
		},
		func() checkResult {
			p, err := guestKernel()
			return checkGuestKernel(app, p, err)
		},
		func() checkResult {
			return checkScionTelemetry(ctx, app.EffectiveScionTelemetry(), readJailScionSettings(jr), b.MountDest(), app.Name, listAgents, readContainerEnv)
		},
		func() checkResult { return checkAgentCert(state, time.Now()) },
		func() checkResult { return checkLastBringup(state, time.Now()) },
		func() checkResult { return checkToolBackends(app.Broker.Tools, app.InstanceDir(), probes) },
		func() checkResult { return checkClaudeVersion(app.ManagerImage(), app.ManagerImageTarPath(), probes) },
		func() checkResult {
			return checkLeverVersion(app.ManagerImage(), app.ManagerImageTarPath(), cli.Version, cli.VersionString(), jailabi.JailABI, probes)
		},
		func() checkResult {
			return checkManagerImage(ctx, b.MountDest(), app.Name, app.ManagerImage(), listAgents)
		},
		func() checkResult { return checkCredentialFile(app.Manager.CredentialFile) },
		func() checkResult { return checkMcpJsonInTree(app.Tree) },
		func() checkResult { return checkTreeBackup(app, state, time.Now()) },
		func() checkResult { return checkDailyReset(app, state, time.Now()) },
		func() checkResult { return checkGoToolchain(app.Scion, scionPrebuilt(app), probes) },
		func() checkResult { return checkNodeToolchain(app, webUIPrebuilt(app), probes) },
		func() checkResult { return checkOperatorSkills(app, state) },
		func() checkResult { return checkDirectives(app, state) },
		func() checkResult { return checkRemote(ctx, app, state, probes, jr) },
		func() checkResult { return checkRemoteExposure(app) },
		func() checkResult { return checkVerifiedChat(app, state) },
		func() checkResult { return checkChatLabels(app) },
		func() checkResult { return checkPush(app, state) },
		func() checkResult { return checkFiles(app, state) },
		func() checkResult { return checkVoice(app) },
		func() checkResult { return checkVoiceTool(ctx, app) },
		func() checkResult { return checkVoiceModel(app) },
		func() checkResult { return checkTTSTool(ctx, app) },
		func() checkResult { return checkTTSModel(app) },
		func() checkResult { return checkAgentMessages(app, state, liveAgentMessages(ctx, app, state)) },
		func() checkResult { return checkSentLedger(app, state) },
		func() checkResult { return checkGuestClock(ctx, jr, time.Now, app.AgentMessagesOn()) },
		func() checkResult {
			return checkPATTokens(ctx, state, app.RemoteEnabled(), jailScion.KnowsUATScope, time.Now())
		},
		func() checkResult { return checkHubAuthentication(ctx, hubAuth, jailScion.RunningHub) },
		func() checkResult { return checkHubIdentities(ctx, hubAuth) },
		func() checkResult { return checkRemoteWebRole(ctx, app, hubAuth) },
		func() checkResult { return checkHubExperiments(ctx, state, jailScion.KnowsUATScope, hc) },
		func() checkResult { return checkHubRuntime(ctx, hc) },
		func() checkResult { return checkProjectSharedDirs(ctx, project, listSharedDirs) },
		func() checkResult { return checkAgentRoles(ctx, project, rolesSupported, listAgentRoles) },
		func() checkResult { return checkAgentListSize(ctx, project, measureAgentList) },
		func() checkResult {
			return checkAgentRoleCeiling(ctx, project, scionpkg.EffectiveAgentRole(app.Scion.AgentRole), rolesSupported, readCeiling)
		},
		func() checkResult {
			return checkWorkerTicketMounts(ctx, b.MountDest(), workerNames, listAgents, inspectMounts)
		},
		func() checkResult {
			return checkManagerReadOnly(ctx, b.MountDest(), app.Tree, app.Name, app.ManagerTreeMounts(), listAgents, inspectMountsRW, recordVolumes, liveMounts)
		},
		func() checkResult {
			return checkSharedFolders(ctx, b.MountDest(), app, listAgents, inspectMountsRW, recordVolumes, liveMounts)
		},
		func() checkResult { return checkWorkerTreeBootstraps(app.Tree, workerDirs) },
		func() checkResult { return checkRecyclableWorkers(app.Workers) },
		func() checkResult {
			return checkAgentNetwork(ctx, b.MountDest(), networkCheckedAgents(app.Name, workerNames), jail.ForceHostNetworkFromEnv(), listAgents, inspectNetMode)
		},
		func() checkResult { return checkScionProjectInJail(ctx, b) },
	}
	out := make([]checkResult, 0, len(checks))
	for _, run := range checks {
		// A check with nothing to say (voice rows while voice is off) adds no row.
		if r := run(); r.name != "" {
			out = append(out, r)
		}
	}
	return out
}

// checkScionProjectInJail reports a read error as the jail not being up
// rather than a marker verdict doctor cannot make.
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
