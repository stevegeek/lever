package scion

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

func TestContainerLive(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"running", true}, {"Up 6 seconds", true}, {"Up About a minute", true},
		{"stopped", false}, {"Exited (1) 4 minutes ago", false}, {"", false},
	} {
		if got := ContainerLive(c.in); got != c.want {
			t.Fatalf("ContainerLive(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestListParsesAgents(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion list --format json -g /g/a --non-interactive", proc.Result{Stdout: `[{"slug":"a","phase":"running","activity":"building"}]`})
	c := New(f, Options{})
	agents, err := c.List(context.Background(), "/g/a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 || agents[0].Slug != "a" || agents[0].Phase != "running" || agents[0].Activity != "building" {
		t.Fatalf("agents=%+v", agents)
	}
}

// fakeScion scripts a runner for the Start tests. Start first probes
// `scion start --help` to learn whether this scion understands --role
// (scion#1089), so the probe needs its own scripted answer. The two keys do not
// overlap as prefixes, which keeps FakeRunner's prefix match deterministic.
func fakeScion(roleSupported bool) *proc.FakeRunner {
	f := proc.NewFakeRunner()
	help := "Flags:\n      --harness-auth string   Override auth method\n"
	if roleSupported {
		help += "      --role string   Agent authorization role\n"
	}
	f.Script("scion start --help", proc.Result{Stdout: help})
	f.Script("scion -g", proc.Result{})
	return f
}

// startArgv is the argv of the actual start call — the LAST call, since the
// capability probe runs first.
func startArgv(f *proc.FakeRunner) string {
	return strings.Join(f.Calls[len(f.Calls)-1].Args, " ")
}

func TestStartArgv(t *testing.T) {
	f := fakeScion(false)
	c := New(f, Options{})
	err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "do x", Harness: "claude", Project: "/g/a", Image: "img:1", Workspace: "/lever"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := startArgv(f)
	for _, want := range []string{"-g /g/a", "start a do x", "--harness claude", "--harness-auth oauth-token", "--image img:1", "--workspace /lever"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
}

// On a scion WITHOUT roles (pre-#1089) the flag must be omitted: emitting it
// would make lever unable to start an agent at all on that pin. Nothing is
// widened by omitting it, because the roles system does not exist there.
func TestStartOmitsRoleFlagWhenUnsupported(t *testing.T) {
	assertStartArgvLacks(t, "--role", "must not carry --role on a scion that has no such flag")
}

// assertStartArgvLacks starts a plain agent on a roles-less scion and fails
// when the start argv carries flag.
func assertStartArgvLacks(t *testing.T, flag, why string) {
	t.Helper()
	f := fakeScion(false)
	c := New(f, Options{})
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := startArgv(f); strings.Contains(got, flag) {
		t.Fatalf("argv %q %s", got, why)
	}
}

// THE SECURITY-CRITICAL CASE. On a scion WITH roles, lever must stamp baseline
// even when the instance named no role. scion#1090 flipped the unspecified-role
// default to FULL — agent create, lifecycle and project-secret-read — so
// staying silent hands every agent hub authority and breaks lever's core
// invariant. Detection is by capability probe, not by pin, because a commit
// hash says nothing about which side of #1090 it sits on.
func TestStartStampsBaselineWhenRolesSupported(t *testing.T) {
	f := fakeScion(true)
	c := New(f, Options{}) // no role configured
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := startArgv(f); !strings.Contains(got, "--role baseline") {
		t.Fatalf("argv %q must stamp --role baseline; scion#1090 defaults an unspecified role to FULL", got)
	}
}

// A configured role overrides the default, so an operator can widen or narrow
// deliberately.
func TestStartStampsConfiguredRole(t *testing.T) {
	f := fakeScion(true)
	c := New(f, Options{AgentRole: "readonly"})
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a", APIKey: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := startArgv(f)
	if !strings.Contains(got, "--role readonly") {
		t.Fatalf("argv %q missing the configured --role readonly", got)
	}
}

// Asking for a role a scion cannot honour must fail loudly. Silently dropping
// it would leave the operator believing authority is pinned when it is not.
func TestStartRejectsConfiguredRoleWhenUnsupported(t *testing.T) {
	f := fakeScion(false)
	c := New(f, Options{AgentRole: "baseline"})
	err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a"})
	if err == nil {
		t.Fatal("expected an error: the configured role cannot be honoured")
	}
	if !strings.Contains(err.Error(), "1089") {
		t.Errorf("error should explain which scion change is missing, got %v", err)
	}
}

// A probe that cannot answer must fail closed. Guessing "unsupported" would
// silently omit the flag on a post-#1090 scion and grant FULL authority.
func TestStartFailsClosedWhenProbeFails(t *testing.T) {
	f := proc.NewFakeRunner() // nothing scripted: the probe errors
	c := New(f, Options{})
	err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a"})
	if err == nil {
		t.Fatal("expected an error: agent-role support could not be determined")
	}
	if !strings.Contains(err.Error(), "agent-role support") {
		t.Errorf("error should name the probe, got %v", err)
	}
}

// The probe runs on EVERY start, not once per client. Memoising it let a stale
// answer outlive the binary it described: `scion.source`/`scion.binary` are
// paths, so swapping the artifact leaves brokerctl.ConfigHash identical and the
// long-lived broker keeps running with its cached verdict. A stale "no --role"
// disarms both the stamp and the pre-role record guard at once.
func TestRoleProbeRunsOnEveryStart(t *testing.T) {
	f := fakeScion(true)
	c := New(f, Options{})
	for i := 0; i < 3; i++ {
		if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a"}); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}
	probes := 0
	for _, call := range f.Calls {
		if strings.Join(call.Args, " ") == "start --help" {
			probes++
		}
	}
	if probes != 3 {
		t.Errorf("probed %d times across 3 starts, want 3 (a memoised verdict can outlive the binary)", probes)
	}
}

func TestStartWorkspaceSubdirEmitsRelativeFlag(t *testing.T) {
	f := fakeScion(false)
	c := New(f, Options{})
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a", WorkspaceSubdir: "workers/a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := startArgv(f)
	if !strings.Contains(got, "--workspace workers/a") {
		t.Fatalf("argv %q must contain --workspace workers/a", got)
	}
	if strings.Contains(got, "--workspace /") {
		t.Fatalf("argv %q must not emit an absolute --workspace when a subdir is set (the relative form is what scopes the mount to the subtree)", got)
	}
}

func TestStartWorkspaceSubdirWinsOverWorkspace(t *testing.T) {
	f := fakeScion(false)
	c := New(f, Options{})
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Project: "/g/a", Workspace: "/lever", WorkspaceSubdir: "workers/a"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := startArgv(f)
	if !strings.Contains(got, "--workspace workers/a") || strings.Contains(got, "--workspace /lever") {
		t.Fatalf("subdir must take precedence over absolute workspace; argv %q", got)
	}
}

func TestStartAPIKeyUsesAPIKeyAuth(t *testing.T) {
	f := fakeScion(false)
	c := New(f, Options{})
	// api-key mode: scion starts with --harness-auth api-key, satisfied by a
	// placeholder ANTHROPIC_API_KEY (Hub secret); the real credential is the
	// in-container broker capability token (settings.json). Must NOT request
	// oauth-token (no CLAUDE_CODE_OAUTH_TOKEN exists in api-key mode).
	if err := c.Start(context.Background(), StartOpts{Worker: "a", Task: "x", Harness: "claude", Project: "/g/a", APIKey: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := startArgv(f)
	if !strings.Contains(got, "--harness-auth api-key") {
		t.Fatalf("api-key Start argv %q must use --harness-auth api-key", got)
	}
	if strings.Contains(got, "oauth-token") {
		t.Fatalf("api-key Start argv %q must NOT request oauth-token auth", got)
	}
}

func TestStartOmitsWorkspaceWhenEmpty(t *testing.T) {
	assertStartArgvLacks(t, "--workspace", "should not contain --workspace when Workspace empty")
}

func TestResumeStopSuspendArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{})
	c := New(f, Options{})
	_ = c.Resume(context.Background(), "a", "/g/a")
	_ = c.Stop(context.Background(), "a", "/g/a")
	_ = c.Suspend(context.Background(), "a", "/g/a")
	_ = c.ResumeForce(context.Background(), "a", "/g/a")
	joined := []string{}
	for _, cc := range f.Calls {
		joined = append(joined, strings.Join(cc.Args, " "))
	}
	all := strings.Join(joined, "|")
	for _, want := range []string{"resume a -g /g/a", "stop a -g /g/a", "suspend a -g /g/a", "resume a --force -g /g/a"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %q", want, all)
		}
	}
}

func TestListParsesContainerStatusAndIgnoresUnknownFields(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion list --format json -g /lever --non-interactive", proc.Result{Stdout: `[
		{"slug":"assistant","phase":"running","containerStatus":"running","other":"ignored"},
		{"slug":"scratch","phase":"suspended","containerStatus":"stopped"}
	]`})
	c := New(f, Options{})
	agents, err := c.List(context.Background(), "/lever")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if want := "list --format json -g /lever --non-interactive"; got != want {
		t.Fatalf("argv = %q, want exactly %q", got, want)
	}
	if len(agents) != 2 {
		t.Fatalf("agents=%+v", agents)
	}
	if agents[0].Slug != "assistant" || agents[0].Phase != "running" || agents[0].ContainerStatus != "running" {
		t.Fatalf("agents[0]=%+v", agents[0])
	}
	if agents[1].Slug != "scratch" || agents[1].Phase != "suspended" || agents[1].ContainerStatus != "stopped" {
		t.Fatalf("agents[1]=%+v", agents[1])
	}
}

func TestListEmptyStdoutIsEmptySlice(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion list --format json -g /lever --non-interactive", proc.Result{Stdout: "   \n"})
	c := New(f, Options{})
	agents, err := c.List(context.Background(), "/lever")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("agents=%+v, want empty slice", agents)
	}
}

func TestListMalformedJSONErrors(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion list --format json -g /lever --non-interactive", proc.Result{Stdout: `[{"slug": "a", `})
	c := New(f, Options{})
	if _, err := c.List(context.Background(), "/lever"); err == nil {
		t.Fatal("expected error parsing malformed JSON")
	}
}

func TestDeleteArgv(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{})
	c := New(f, Options{})
	if err := c.Delete(context.Background(), "scratch", "/lever"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got := startArgv(f)
	if want := "delete scratch -g /lever --non-interactive"; got != want {
		t.Fatalf("argv = %q, want exactly %q", got, want)
	}
}

func TestAttachArgvNotRun(t *testing.T) {
	f := proc.NewFakeRunner()
	c := New(f, Options{Bin: "scion"})
	argv := c.AttachArgv("a", "/g/a")
	want := []string{"scion", "attach", "a", "-g", "/g/a"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv=%v", argv)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("AttachArgv must NOT execute anything")
	}
}

// TestAttachArgvEmbedsHubTokenWhenPresent: the attach/TTY path bypasses
// Client.env() (it's exec()'d directly, never through the runner), so the
// controller PAT must be embedded into the returned argv itself — mirroring
// how the jail env is embedded for attach (internal/jail/attach.go).
func TestAttachArgvEmbedsHubTokenWhenPresent(t *testing.T) {
	f := proc.NewFakeRunner()
	c := New(f, Options{Bin: "scion", HubTokenSource: func() string { return "pat123" }})
	argv := c.AttachArgv("a", "/g/a")
	want := []string{"env", "SCION_HUB_TOKEN=pat123", "scion", "attach", "a", "-g", "/g/a"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv=%v, want=%v", argv, want)
	}
}

// TestAttachArgvOmitsHubTokenWhenEmpty: no token means no env prefix at all —
// exact argv shape preserved (see TestAttachArgvNotRun) so subscription-mode
// attach is unaffected.
func TestAttachArgvOmitsHubTokenWhenEmpty(t *testing.T) {
	f := proc.NewFakeRunner()
	c := New(f, Options{Bin: "scion"})
	argv := c.AttachArgv("a", "/g/a")
	for _, tok := range argv {
		if strings.HasPrefix(tok, "SCION_HUB_TOKEN=") || tok == "env" {
			t.Fatalf("argv=%v should not contain an env/SCION_HUB_TOKEN prefix when no token set", argv)
		}
	}
}

func TestFindAgent(t *testing.T) {
	agents := []Agent{
		{Slug: "alpha", Phase: "running"},
		{Slug: "beta", Phase: "suspended"},
	}
	if got := FindAgent(agents, "beta"); got == nil || got.Phase != "suspended" {
		t.Fatalf("FindAgent(beta) = %v, want the suspended beta record", got)
	}
	// Returned pointer must alias the slice element (callers read the live record).
	if got := FindAgent(agents, "alpha"); got != &agents[0] {
		t.Fatalf("FindAgent must return a pointer into the slice, got %p want %p", got, &agents[0])
	}
	if got := FindAgent(agents, "missing"); got != nil {
		t.Fatalf("FindAgent(missing) = %v, want nil", got)
	}
	if got := FindAgent(nil, "x"); got != nil {
		t.Fatalf("FindAgent(nil) = %v, want nil", got)
	}
}

// TestWaitAgentLiveRecordVanishesMidPollResetsToEmpty pins the reset-to-empty
// behavior (B4 caution 2): once observed with a non-live phase, the record then
// disappears from the listing on the final attempt. The exhaustion error MUST
// report the LAST observation ("" / "") — the vanished state — not the stale
// earlier phase, or the message lies about what scion last reported.
func TestWaitAgentLiveRecordVanishesMidPollResetsToEmpty(t *testing.T) {
	call := 0
	list := func(context.Context) ([]Agent, error) {
		call++
		if call == 1 {
			// Present but not yet live — records "starting"/"Up 1s".
			return []Agent{{Slug: "mgr", Phase: "starting", ContainerStatus: "Up 1s"}}, nil
		}
		// Record gone from the listing.
		return []Agent{{Slug: "other", Phase: "running", ContainerStatus: "running"}}, nil
	}
	err := WaitAgentLive(context.Background(), list, "mgr", 2, time.Millisecond)
	if !errors.Is(err, ErrAgentNotLive) {
		t.Fatalf("WaitAgentLive should fail when the record never becomes live: %v", err)
	}
	if strings.Contains(err.Error(), "starting") || strings.Contains(err.Error(), "Up 1s") {
		t.Fatalf("error must reflect the reset last observation, not the stale earlier phase: %v", err)
	}
	if !strings.Contains(err.Error(), `phase ""`) || !strings.Contains(err.Error(), `container ""`) {
		t.Fatalf("error must report the vanished (empty) last phase/container: %v", err)
	}
}

// TestAttachArgvPinsTheHubEndpoint: attach EXECS scion rather than going through
// Client.run, so it must carry the endpoint itself. Without it scion falls back
// to the endpoint persisted in the jail's project config — state lever does not
// own, and which the controller-PAT mint window can leave pointing at the
// throwaway hub (live failure 2026-08-11: attach dialled 127.0.0.1:48080 and got
// connection refused while every other verb worked).
func TestAttachArgvPinsTheHubEndpoint(t *testing.T) {
	c := New(proc.NewFakeRunner(), Options{HubEndpoint: "http://127.0.0.1:8080"})
	argv := strings.Join(c.AttachArgv("a", "/g/a"), " ")
	if !strings.Contains(argv, "SCION_HUB_ENDPOINT=http://127.0.0.1:8080") {
		t.Fatalf("attach argv must pin the hub endpoint, got %q", argv)
	}
	if !strings.HasPrefix(argv, "env ") {
		t.Fatalf("env assignments must lead the argv, got %q", argv)
	}
}

// Both env assignments ride the same `env` prefix, and the scion command still
// follows them.
func TestAttachArgvCarriesTokenAndEndpointTogether(t *testing.T) {
	c := New(proc.NewFakeRunner(), Options{
		HubEndpoint:    "http://127.0.0.1:8080",
		HubTokenSource: func() string { return "tok" },
	})
	argv := c.AttachArgv("a", "/g/a")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "SCION_HUB_TOKEN=tok") || !strings.Contains(joined, "SCION_HUB_ENDPOINT=http://127.0.0.1:8080") {
		t.Fatalf("both env assignments must be present, got %q", joined)
	}
	if i := indexOf(argv, "attach"); i < 0 || argv[i-1] != "scion" {
		t.Fatalf("the scion attach command must follow the env prefix, got %q", joined)
	}
}

func indexOf(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}

// With neither a token nor an endpoint there is no env prefix at all — the bare
// scion invocation, as before.
func TestAttachArgvHasNoEnvPrefixWhenNothingToPin(t *testing.T) {
	c := New(proc.NewFakeRunner(), Options{})
	argv := c.AttachArgv("a", "/g/a")
	if argv[0] == "env" {
		t.Fatalf("no env assignments means no env prefix, got %q", strings.Join(argv, " "))
	}
}

// TestWaitAgentLiveZeroAttemptsExhaustsImmediately pins that a non-positive
// budget is exhaustion, not retry.Until's "unbounded" reading of <= 0.
func TestWaitAgentLiveZeroAttemptsExhaustsImmediately(t *testing.T) {
	calls := 0
	list := func(context.Context) ([]Agent, error) {
		calls++
		return []Agent{{Slug: "mgr", Phase: "running", ContainerStatus: "running"}}, nil
	}
	err := WaitAgentLive(context.Background(), list, "mgr", 0, time.Millisecond)
	if !errors.Is(err, ErrAgentNotLive) {
		t.Fatalf("err = %v, want exhaustion", err)
	}
	if calls != 0 {
		t.Fatalf("list called %d times with a zero budget", calls)
	}
}
