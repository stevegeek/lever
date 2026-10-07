package host

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

type stubBackend struct {
	up, down, stopped bool
	scionState        types.ScionProjectState
	scionErr          error
	resolveRunUserErr error       // when set, ResolveRunUser returns it instead of nil
	resolvedRunUser   bool        // ResolveRunUser was called
	runnerBeforeUser  bool        // JailRunner was called before ResolveRunUser
	runner            proc.Runner // JailRunner override; nil ⇒ proc.RealRunner{}
	removeScionCalls  []string    // workspace paths passed to RemoveScionProjectConfigs
	removeScionErr    error
	registeredResult  bool // ScionProjectRegistered return value
	registeredErr     error
	registeredCalls   []string // workspace paths passed to ScionProjectRegistered
	hubLoginChanged   bool     // EnsureHubLogin's "the hub config changed" answer
	hubLoginErr       error
	hubLoginCalls     []types.HubLogin
	hubLoginDisabled  int            // DisableHubLogin call count
	hubLoginRemoved   bool           // DisableHubLogin's "the hub config changed" answer
	telemetryCalls    []bool         // EnsureScionTelemetry "off" arguments, in call order
	telemetryChanged  bool           // EnsureScionTelemetry's "the file changed" answer
	leverTemplates    int            // EnsureLeverTemplate call count
	upCfg             backend.Config // the Config the last EnsureUp received
}

func (s *stubBackend) EnsureUp(_ context.Context, cfg backend.Config) error {
	s.up, s.upCfg = true, cfg
	return nil
}
func (s *stubBackend) DockerHost() string                             { return "unix:///x" }
func (s *stubBackend) HostToolAlias() string                          { return "host.orb.internal" }
func (s *stubBackend) MountDest() string                              { return "/lever" }
func (s *stubBackend) ApplyEgress(context.Context, []int, bool) error { return nil }
func (s *stubBackend) Teardown(context.Context) error                 { s.down = true; return nil }
func (s *stubBackend) Stop(context.Context) error                     { s.stopped = true; return nil }
func (s *stubBackend) Profile() backend.Profile                       { return backend.Profile{Name: "stub"} }
func (s *stubBackend) HostAliasV4() string                            { return "" }
func (s *stubBackend) RunUser() string                                { return "stub" }
func (s *stubBackend) RunUID() string                                 { return "501" }
func (s *stubBackend) ResolveRunUser(context.Context) error {
	s.resolvedRunUser = true
	return s.resolveRunUserErr
}
func (s *stubBackend) JailRunner() proc.Runner {
	if !s.resolvedRunUser {
		s.runnerBeforeUser = true
	}
	if s.runner != nil {
		return s.runner
	}
	return proc.RealRunner{}
}
func (s *stubBackend) AttachArgv(inner []string) []string {
	return append([]string{"stub-attach"}, inner...)
}
func (s *stubBackend) LoadImage(context.Context, string) error  { return nil }
func (s *stubBackend) ImageLoaded(context.Context, string) bool { return false }
func (s *stubBackend) LoadImageTar(context.Context, string, string, func(string) error) error {
	return nil
}
func (s *stubBackend) ImageLoadedTar(context.Context, string, string) bool      { return false }
func (s *stubBackend) PruneJailImages(context.Context) error                    { return nil }
func (s *stubBackend) InstallGuestBinary(context.Context, string, string) error { return nil }
func (s *stubBackend) EnsureHubLogin(_ context.Context, spec types.HubLogin) (bool, error) {
	s.hubLoginCalls = append(s.hubLoginCalls, spec)
	return s.hubLoginChanged, s.hubLoginErr
}
func (s *stubBackend) DisableHubLogin(context.Context) (bool, error) {
	s.hubLoginDisabled++
	return s.hubLoginRemoved, nil
}
func (s *stubBackend) EnsureScionTelemetry(_ context.Context, off bool) (bool, error) {
	s.telemetryCalls = append(s.telemetryCalls, off)
	return s.telemetryChanged, nil
}
func (s *stubBackend) EnsureLeverTemplate(context.Context) (bool, error) {
	s.leverTemplates++
	return true, nil
}
func (s *stubBackend) ReadScionProjectState(context.Context) (types.ScionProjectState, error) {
	return s.scionState, s.scionErr
}
func (s *stubBackend) RemoveScionProjectConfigs(_ context.Context, workspacePath string) error {
	s.removeScionCalls = append(s.removeScionCalls, workspacePath)
	return s.removeScionErr
}
func (s *stubBackend) RepairScionHubEndpoint(_ context.Context, _, _ string) error {
	return nil
}

func (s *stubBackend) ScionProjectRegistered(_ context.Context, workspacePath string) (bool, error) {
	s.registeredCalls = append(s.registeredCalls, workspacePath)
	return s.registeredResult, s.registeredErr
}

func TestUpCommandCallsEnsureUp(t *testing.T) {
	sb := &stubBackend{}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"provision", "--machine", "lever-jail", "--tree", "/tmp/tree", "--allow-port", "3305"})
	if err := root.Execute(); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !sb.up {
		t.Fatal("EnsureUp not called")
	}
	// provision reads no lever.yaml, so it must not converge nested_virt off.
	if !sb.upCfg.NestedVirtUnknown || sb.upCfg.NestedVirt {
		t.Fatalf("provision must mark nested_virt unknown, got %+v", sb.upCfg)
	}
}

func TestDoctorPrintsProfile(t *testing.T) {
	root := newRootWith(func(string, string) (backend.Backend, error) { return &stubBackend{}, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"doctor", "--machine", "lever-jail"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("doctor printed nothing")
	}
}

func TestFactoryReceivesConfiguredBackendName(t *testing.T) {
	var gotName string
	bf := func(name, machine string) (backend.Backend, error) {
		gotName = name
		return &stubBackend{}, nil
	}
	root := newRootWith(bf)
	root.SetArgs([]string{"doctor", "--machine", "lever-x", "--backend", "orbstack"})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if gotName != "orbstack" {
		t.Fatalf("factory got name %q, want %q", gotName, "orbstack")
	}
}

// TestBringUpBackendPassesWarn: apply hands EnsureUp its log, so the
// backend's sizing-drift advice reaches the operator.
func TestBringUpBackendPassesWarn(t *testing.T) {
	sb := &stubBackend{}
	app := &config.App{Name: "x", Backend: config.BackendLima, CPUs: 8, Memory: "24GiB", NestedVirt: true}
	var got []string
	warn := func(format string, a ...any) { got = append(got, fmt.Sprintf(format, a...)) }
	if _, err := bringUpBackend(context.Background(), app, func(string, string) (backend.Backend, error) { return sb, nil }, warn); err != nil {
		t.Fatal(err)
	}
	if sb.upCfg.Warn == nil || sb.upCfg.CPUs != 8 || sb.upCfg.Memory != "24GiB" || !sb.upCfg.NestedVirt || sb.upCfg.NestedVirtUnknown {
		t.Fatalf("cfg = %+v", sb.upCfg)
	}
	sb.upCfg.Warn("w %d", 1)
	if len(got) != 1 || got[0] != "w 1" {
		t.Fatalf("warn sink = %q", got)
	}
}

// Doctor reads the run user before it builds the jail runner: the runner's
// XDG_RUNTIME_DIR comes from that uid, and on Lima (guest uid 1000, not the
// default 501) rootless podman found no agent container without it.
func TestDoctorResolvesTheRunUserBeforeTheJailRunner(t *testing.T) {
	sb := &stubBackend{runner: proc.NewFakeRunner()}
	app := &config.App{Name: "assistant"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runDoctorChecks(ctx, app, state.State{Dir: t.TempDir()}, sb, productionProbes(proc.NewFakeRunner()))
	if !sb.resolvedRunUser || sb.runnerBeforeUser {
		t.Fatalf("resolved=%v, runner before the user=%v", sb.resolvedRunUser, sb.runnerBeforeUser)
	}
}
