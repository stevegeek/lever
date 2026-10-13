package apply

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

func testHubAuthEnv() map[string]string { return HubAuthEnv("inst1", "sha256:fp1") }

// matchingHubArgv is the argv scion's daemon re-exec shows for a hub lever
// started for app: normalised to the equals form, with --foreground added.
func matchingHubArgv(app *config.App) []string {
	opts := HubServerOpts(app, "", nil)
	argv := []string{"/usr/local/bin/scion", "server", "start", "--foreground", "--enable-hub=true"}
	if opts.EnableWeb {
		argv = append(argv, "--enable-web=true")
	}
	argv = append(argv, "--dev-auth=false", "--web-port=8080")
	if opts.WebAssetsDir != "" {
		argv = append(argv, "--web-assets-dir="+opts.WebAssetsDir)
	}
	return argv
}

func TestHubAuthEnvPinsTheProxyJWTConfiguration(t *testing.T) {
	got := HubAuthEnv("inst1", "sha256:fp1")
	want := map[string]string{
		"SCION_SERVER_AUTH_MODE":                    "proxy",
		"SCION_SERVER_AUTH_USERACCESSMODE":          "invite_only",
		"SCION_SERVER_AUTH_PROXY_PROVIDER":          "jwt",
		"SCION_SERVER_AUTH_PROXY_JWT_ALGORITHM":     "ES256",
		"SCION_SERVER_AUTH_PROXY_JWT_HEADER":        hubauth.Header,
		"SCION_SERVER_AUTH_PROXY_JWT_ISSUER":        "lever:inst1",
		"SCION_SERVER_AUTH_PROXY_JWT_AUDIENCE":      "scion-hub:inst1",
		"SCION_SERVER_AUTH_PROXY_JWT_PUBLICKEYFILE": hubauth.GuestPublicKeyPath,
		"SCION_SERVER_HUB_ADMINEMAILS":              hubauth.AdminEmail,
		"LEVER_HUB_AUTH_KEY":                        "sha256:fp1",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("HubAuthEnv =\n%v\nwant\n%v", got, want)
	}
	if got["SCION_SERVER_HUB_ADMINEMAILS"] != "lever-admin@lever.local" {
		t.Fatalf("admin emails = %q", got["SCION_SERVER_HUB_ADMINEMAILS"])
	}
	other := HubAuthEnv("inst2", "sha256:fp2")
	if other["SCION_SERVER_AUTH_PROXY_JWT_ISSUER"] != "lever:inst2" || other["SCION_SERVER_AUTH_PROXY_JWT_AUDIENCE"] != "scion-hub:inst2" ||
		other["LEVER_HUB_AUTH_KEY"] != "sha256:fp2" {
		t.Fatalf("iss/aud/key do not follow the instance and key: %v", other)
	}
}

// scion maps a compound setting only when it is written without inner
// underscores and silently ignores the underscored spelling; an ignored
// user-access mode admits any signed-in email.
func TestHubAuthEnvUsesTheSpellingsScionReads(t *testing.T) {
	env := testHubAuthEnv()
	for _, k := range []string{"SCION_SERVER_AUTH_USERACCESSMODE", "SCION_SERVER_AUTH_PROXY_JWT_PUBLICKEYFILE"} {
		if env[k] == "" {
			t.Fatalf("%s missing: %v", k, env)
		}
	}
	for k := range env {
		for _, bad := range []string{"USER_ACCESS_MODE", "PUBLIC_KEY_FILE", "ADMIN_EMAILS"} {
			if strings.Contains(k, bad) {
				t.Fatalf("%s uses a spelling scion ignores", k)
			}
		}
	}
}

// RunningHub reads back only HubEnvPrefixes; a pinned key outside them would
// read as missing and restart the hub on every apply.
func TestHubAuthEnvKeysAreReadBackByRunningHub(t *testing.T) {
	for k := range testHubAuthEnv() {
		if !slices.ContainsFunc(scion.HubEnvPrefixes, func(p string) bool { return strings.HasPrefix(k, p) }) {
			t.Fatalf("%s is outside scion.HubEnvPrefixes %v", k, scion.HubEnvPrefixes)
		}
		if strings.HasPrefix(k, "SCION_SERVER_SESSION") {
			t.Fatalf("%s is filtered out by the running-hub probe", k)
		}
	}
}

func TestHubServerOptsCarriesTheAuthEnv(t *testing.T) {
	env := testHubAuthEnv()
	opts := HubServerOpts(helloApp("/t"), "secret-hex", env)
	if !maps.Equal(opts.Env, env) || opts.DevAuth || opts.SessionSecret != "secret-hex" || opts.WebPort != scion.DefaultHubPort {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestHubRestartReason(t *testing.T) {
	env := testHubAuthEnv()
	with := func(k, v string) map[string]string {
		m := maps.Clone(env)
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	base := []string{"/usr/local/bin/scion", "server", "start", "--foreground", "--enable-hub=true", "--dev-auth=false", "--web-port=8080"}
	argv := func(extra ...string) []string { return append(slices.Clone(base), extra...) }
	noDevAuth := slices.DeleteFunc(slices.Clone(base), func(a string) bool { return strings.HasPrefix(a, "--dev-auth") })
	want := scion.ServerOpts{Env: env}
	wantAssets := scion.ServerOpts{Env: env, WebAssetsDir: "/x"}
	for _, tc := range []struct {
		name    string
		live    scion.RunningHub
		want    scion.ServerOpts
		restart bool
	}{
		{"not running", scion.RunningHub{}, want, false},
		{"not running, nothing else read", scion.RunningHub{Env: map[string]string{"X": "y"}}, want, false},
		{"equal", scion.RunningHub{Running: true, Argv: base, Env: maps.Clone(env)}, want, false},
		{"scion's normalised argv", scion.RunningHub{Running: true, Env: env,
			Argv: []string{"scion", "server", "start", "--enable-web=true", "--dev-auth=false", "--web-port=8080", "--web-assets-dir=/x"}}, wantAssets, false},
		{"space-separated flags", scion.RunningHub{Running: true, Env: env,
			Argv: []string{"scion", "server", "start", "--dev-auth", "false", "--web-assets-dir", "/x"}}, wantAssets, false},
		{"missing lever key", scion.RunningHub{Running: true, Argv: base, Env: with("LEVER_HUB_AUTH_KEY", "")}, want, true},
		{"other key fingerprint", scion.RunningHub{Running: true, Argv: base, Env: with("LEVER_HUB_AUTH_KEY", "sha256:old")}, want, true},
		{"missing user access mode", scion.RunningHub{Running: true, Argv: base, Env: with("SCION_SERVER_AUTH_USERACCESSMODE", "")}, want, true},
		{"open user access mode", scion.RunningHub{Running: true, Argv: base, Env: with("SCION_SERVER_AUTH_USERACCESSMODE", "open")}, want, true},
		{"other audience", scion.RunningHub{Running: true, Argv: base, Env: with("SCION_SERVER_AUTH_PROXY_JWT_AUDIENCE", "scion-hub:other")}, want, true},
		{"no env at all", scion.RunningHub{Running: true, Argv: base, Env: map[string]string{}}, want, true},
		{"extra pinned key", scion.RunningHub{Running: true, Argv: base, Env: with("SCION_SERVER_AUTH_DEVMODE", "true")}, want, true},
		{"underscored extra key", scion.RunningHub{Running: true, Argv: base, Env: with("SCION_SERVER_AUTH_USER_ACCESS_MODE", "open")}, want, true},
		{"dev-auth=true", scion.RunningHub{Running: true, Env: env,
			Argv: slices.Concat(noDevAuth, []string{"--dev-auth=true"})}, want, true},
		{"bare --dev-auth", scion.RunningHub{Running: true, Env: env,
			Argv: slices.Concat(noDevAuth, []string{"--dev-auth"})}, want, true},
		{"dev-auth absent", scion.RunningHub{Running: true, Env: env, Argv: noDevAuth}, want, true},
		{"web assets dir differs", scion.RunningHub{Running: true, Env: env, Argv: argv("--web-assets-dir=/y")}, wantAssets, true},
		{"web assets dir added", scion.RunningHub{Running: true, Env: env, Argv: base}, wantAssets, true},
		{"web assets dir removed", scion.RunningHub{Running: true, Env: env, Argv: argv("--web-assets-dir=/x")}, want, true},
		{"web assets dir equal", scion.RunningHub{Running: true, Env: env, Argv: argv("--web-assets-dir=/x")}, wantAssets, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := HubRestartReason(tc.live, tc.want)
			if (got != "") != tc.restart {
				t.Fatalf("HubRestartReason = %q, want restart=%v", got, tc.restart)
			}
		})
	}
}

// The reason names the key, never its value: it lands in the apply log.
func TestHubRestartReasonNamesTheKey(t *testing.T) {
	env := testHubAuthEnv()
	live := maps.Clone(env)
	live["LEVER_HUB_AUTH_KEY"] = "sha256:old"
	got := HubRestartReason(scion.RunningHub{Running: true, Env: live, Argv: []string{"--dev-auth=false"}}, scion.ServerOpts{Env: env})
	if !strings.Contains(got, "LEVER_HUB_AUTH_KEY") {
		t.Fatalf("reason = %q", got)
	}
}

type scionServerRun struct {
	f    *proc.FakeRunner
	logs *logSink
	err  error
}

func (r scionServerRun) serverCalls() []string {
	var out []string
	for _, c := range r.f.Calls {
		if c.Name == "scion" && len(c.Args) >= 2 && c.Args[0] == "server" {
			out = append(out, c.Args[1])
		}
	}
	return out
}

func (r scionServerRun) startEnv(t *testing.T) map[string]string {
	t.Helper()
	i := r.f.CallIndex(proc.ArgvPrefix("scion", "server", "start"))
	if i < 0 {
		t.Fatalf("no server start; calls=%s", joinedCalls(r.f))
	}
	return r.f.Calls[i].Env
}

// runScionServer applies helloApp against a jail whose hub probe prints live
// ("" is no hub running).
func runScionServer(t *testing.T, live string, guestChanged bool, guestErr error) scionServerRun {
	t.Helper()
	f := scionOKRunner()
	scriptRunningHub(f, live)
	app := helloApp(t.TempDir())
	sink := &logSink{}
	err := runApply(app, Deps{
		Scion:         hubScion(f, app),
		HubAuthEnv:    testHubAuthEnv(),
		EnsureHubAuth: func(context.Context) (bool, error) { return guestChanged, guestErr },
		Log:           sink.logf,
	})
	return scionServerRun{f: f, logs: sink, err: err}
}

func wantStartEnv(t *testing.T, r scionServerRun) {
	t.Helper()
	got := r.startEnv(t)
	for k, v := range testHubAuthEnv() {
		if got[k] != v {
			t.Errorf("server start env %s = %q, want %q", k, got[k], v)
		}
	}
	if got["SCION_HUB_ENDPOINT"] != testHubEndpoint {
		t.Errorf("server start env lost the client's endpoint: %q", got["SCION_HUB_ENDPOINT"])
	}
}

func TestScionServerKeepsAMatchingHub(t *testing.T) {
	app := helloApp("")
	r := runScionServer(t, runningHubOut(42, matchingHubArgv(app), testHubAuthEnv()), false, nil)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if got := r.serverCalls(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("server calls = %q, want just the tolerated start: a restart drops every agent's hub connection", got)
	}
	wantStartEnv(t, r)
	for _, l := range r.logs.lines {
		if strings.Contains(l, "restarting the hub") {
			t.Fatalf("logged a restart: %q", l)
		}
	}
}

func TestScionServerRestartsAHubWithOtherAuthEnv(t *testing.T) {
	app := helloApp("")
	for name, live := range map[string]map[string]string{
		"old key":         HubAuthEnv("inst1", "sha256:old"),
		"no auth env":     {},
		"other instance":  HubAuthEnv("inst2", "sha256:fp1"),
		"extra lever key": func() map[string]string { m := testHubAuthEnv(); m["LEVER_HUB_X"] = "1"; return m }(),
	} {
		t.Run(name, func(t *testing.T) {
			r := runScionServer(t, runningHubOut(42, matchingHubArgv(app), live), false, nil)
			if r.err != nil {
				t.Fatalf("Run: %v", r.err)
			}
			if got := r.serverCalls(); !slices.Equal(got, []string{"stop", "start"}) {
				t.Fatalf("server calls = %q, want stop then start", got)
			}
			wantStartEnv(t, r)
			if !slices.ContainsFunc(r.logs.lines, func(l string) bool { return strings.Contains(l, "restarting the hub") }) {
				t.Fatalf("no restart logged: %q", r.logs.lines)
			}
		})
	}
}

// A hub an operator started by hand gets scion's dev-auth default.
func TestScionServerRestartsADevAuthHub(t *testing.T) {
	argv := slices.DeleteFunc(matchingHubArgv(helloApp("")), func(a string) bool { return strings.HasPrefix(a, "--dev-auth") })
	r := runScionServer(t, runningHubOut(42, argv, testHubAuthEnv()), false, nil)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if got := r.serverCalls(); !slices.Equal(got, []string{"stop", "start"}) {
		t.Fatalf("server calls = %q, want stop then start", got)
	}
}

func TestScionServerRestartsARunningHubWhenTheGuestChanged(t *testing.T) {
	r := runScionServer(t, runningHubOut(42, matchingHubArgv(helloApp("")), testHubAuthEnv()), true, nil)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if got := r.serverCalls(); !slices.Equal(got, []string{"stop", "start"}) {
		t.Fatalf("server calls = %q, want stop then start", got)
	}
	wantStartEnv(t, r)
}

func TestScionServerJustStartsWhenNoHubRuns(t *testing.T) {
	for _, changed := range []bool{false, true} {
		r := runScionServer(t, "", changed, nil)
		if r.err != nil {
			t.Fatalf("changed=%v: Run: %v", changed, r.err)
		}
		if got := r.serverCalls(); !slices.Equal(got, []string{"start"}) {
			t.Fatalf("changed=%v: server calls = %q, want just start", changed, got)
		}
		wantStartEnv(t, r)
	}
}

// A hub must never start on auth settings lever could not install.
func TestScionServerFailsWhenEnsureHubAuthFails(t *testing.T) {
	boom := errors.New("key install failed")
	r := runScionServer(t, "", false, boom)
	if !errors.Is(r.err, boom) || !strings.Contains(r.err.Error(), "hub auth") {
		t.Fatalf("err = %v, want the hub auth failure", r.err)
	}
	if got := r.serverCalls(); len(got) != 0 {
		t.Fatalf("server calls = %q after the auth step failed", got)
	}
}

func TestScionServerFailsWhenTheHubCannotBeRead(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "ok"}) // the running-hub probe stays unscripted
	app := helloApp(t.TempDir())
	err := runApply(app, Deps{Scion: hubScion(f, app), HubAuthEnv: testHubAuthEnv()})
	if err == nil || !strings.Contains(err.Error(), "running hub") {
		t.Fatalf("err = %v, want the probe failure", err)
	}
	if f.Called(proc.ArgvPrefix("scion", "server", "start")) {
		t.Fatal("started a hub without knowing what runs")
	}
}

type registerRun struct {
	f                                      *proc.FakeRunner
	order                                  []string
	linkedCalls, stripCalls, ceilingCalled int
	err                                    error
}

// runSoundRegister applies against a sound registration whose ProjectLinked
// answers linked in turn.
func runSoundRegister(t *testing.T, linked []bool, linkedErr error, hubLinkFails bool) registerRun {
	t.Helper()
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, ".scion"), []byte("project-id: real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	app := helloApp(tree)
	r := &registerRun{f: f}
	runner := &hubLinkRunner{agentLifecycleRunner: &agentLifecycleRunner{FakeRunner: f, slug: app.Name}, fail: hubLinkFails,
		onLink: func() { r.order = append(r.order, "hub link") }}
	r.err = runApply(app, Deps{
		JailMount:              "/lever",
		Scion:                  scion.New(runner, scion.Options{HubEndpoint: testHubEndpoint}),
		ScionProjectRegistered: func(context.Context, string) (bool, error) { return true, nil },
		RepairScionHubEndpoint: func(_ context.Context, jp string) error {
			r.order = append(r.order, "repair "+jp)
			return nil
		},
		ProjectLinked: func(_ context.Context, jp string) (bool, error) {
			r.order = append(r.order, "linked? "+jp)
			if linkedErr != nil {
				return false, linkedErr
			}
			i := min(r.linkedCalls, len(linked)-1)
			r.linkedCalls++
			return linked[i], nil
		},
		StripProjectSharedDirs: func(context.Context, string) error {
			r.stripCalls++
			r.order = append(r.order, "settle")
			return nil
		},
		EnsureAgentRoleCeiling: func(context.Context, string) error { r.ceilingCalled++; return nil },
	})
	return *r
}

// hubLinkRunner reports every `scion hub link` and fails it when fail is set.
type hubLinkRunner struct {
	*agentLifecycleRunner
	fail   bool
	onLink func()
}

func (r *hubLinkRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == "scion" && len(args) >= 2 && args[0] == "hub" && args[1] == "link" {
		r.onLink()
		if r.fail {
			r.record(dir, env, name, args)
			return proc.Result{Code: 1, Stderr: "not authenticated to Hub"}, errors.New("exit status 1")
		}
	}
	return r.agentLifecycleRunner.RunIn(ctx, dir, env, name, args...)
}

func (r *hubLinkRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func hubLinks(f *proc.FakeRunner) int {
	n := 0
	for _, c := range f.Calls {
		if c.HasPrefix("scion", "hub", "link") {
			n++
		}
	}
	return n
}

func TestRegisterSoundAndLinkedSettlesWithoutRelinking(t *testing.T) {
	r := runSoundRegister(t, []bool{true}, nil, false)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if n := hubLinks(r.f); n != 0 {
		t.Fatalf("hub link ran %d times on a linked project", n)
	}
	if want := []string{"repair /lever", "linked? /lever", "settle"}; !slices.Equal(r.order, want) {
		t.Fatalf("order = %q, want %q", r.order, want)
	}
	if r.ceilingCalled != 1 {
		t.Fatalf("role ceiling ensured %d times", r.ceilingCalled)
	}
	if !managerStarted(r.f) {
		t.Fatalf("the manager did not start; calls=%s", joinedCalls(r.f))
	}
}

func TestRegisterSoundButUnlinkedRelinks(t *testing.T) {
	r := runSoundRegister(t, []bool{false, true}, nil, false)
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
	if want := []string{"repair /lever", "linked? /lever", "hub link", "linked? /lever", "settle"}; !slices.Equal(r.order, want) {
		t.Fatalf("order = %q, want %q", r.order, want)
	}
	if n := hubLinks(r.f); n != 1 {
		t.Fatalf("hub link ran %d times, want 1", n)
	}
	if r.linkedCalls != 2 {
		t.Fatalf("ProjectLinked called %d times, want 2 (before and after the link)", r.linkedCalls)
	}
	// Linking must not fall into the destructive re-init path.
	if r.f.Called(func(c proc.Call) bool { return c.HasPrefix("scion", "init") && !slices.Contains(c.Args, "--machine") }) {
		t.Fatalf("a sound registration was re-initialised; calls=%s", joinedCalls(r.f))
	}
}

func TestRegisterSoundStillUnlinkedAfterHubLinkFails(t *testing.T) {
	r := runSoundRegister(t, []bool{false, false}, nil, false)
	if r.err == nil || !strings.Contains(r.err.Error(), "still not linked") {
		t.Fatalf("err = %v, want the still-not-linked failure", r.err)
	}
	if r.stripCalls != 0 || r.ceilingCalled != 0 {
		t.Fatalf("an unlinked project was settled (strip=%d ceiling=%d)", r.stripCalls, r.ceilingCalled)
	}
	if managerStarted(r.f) {
		t.Fatal("the manager started on an unlinked project")
	}
}

func TestRegisterSoundLinkedProbeErrorFails(t *testing.T) {
	boom := errors.New("settings unreadable")
	r := runSoundRegister(t, []bool{true}, boom, false)
	if !errors.Is(r.err, boom) || !strings.Contains(r.err.Error(), "register-project") {
		t.Fatalf("err = %v, want the ProjectLinked failure", r.err)
	}
	if n := hubLinks(r.f); n != 0 || r.stripCalls != 0 {
		t.Fatalf("hub link=%d strip=%d after the probe failed", n, r.stripCalls)
	}
}

func TestRegisterSoundRelinkFailureFails(t *testing.T) {
	r := runSoundRegister(t, []bool{false, true}, nil, true)
	if r.err == nil || !strings.Contains(r.err.Error(), "not authenticated") {
		t.Fatalf("err = %v, want the hub link failure", r.err)
	}
	if r.linkedCalls != 1 || r.stripCalls != 0 {
		t.Fatalf("linked=%d strip=%d after the link failed", r.linkedCalls, r.stripCalls)
	}
}

// managerStarted reports an agent create (`scion [-g p] start ...`), not a
// hub start or the role probe.
func managerStarted(f *proc.FakeRunner) bool {
	return f.Called(func(c proc.Call) bool {
		args := c.Args
		if len(args) > 2 && args[0] == "-g" {
			args = args[2:]
		}
		return c.Name == "scion" && len(args) > 0 && args[0] == "start" && !slices.Contains(args, "--help")
	})
}
