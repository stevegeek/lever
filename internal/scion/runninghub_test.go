package scion

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion/layout"
)

func TestParseRunningHubNoServer(t *testing.T) {
	for _, out := range []string{"", "\n", "  \n", "stale:77\n"} {
		h, err := parseRunningHub(out)
		if err != nil {
			t.Fatalf("%q: %v", out, err)
		}
		if h.Running || h.PID != 0 || h.Argv != nil || h.Env != nil {
			t.Fatalf("%q: got %+v, want not running", out, h)
		}
	}
}

func TestParseRunningHubFullOutput(t *testing.T) {
	out := "4242\n" +
		"/usr/local/bin/scion\nserver\nstart\n--foreground\n--enable-web=true\n--dev-auth=false\n--web-port=8080\n" +
		"--lever-env--\n" +
		"SCION_SERVER_AUTH_MODE=proxy\n" +
		"SCION_SERVER_AUTH_PROXY_JWT_AUDIENCE=scion-hub:x=y\n" +
		"LEVER_HUB_AUTH_KEY=sha256:abc\n"
	h, err := parseRunningHub(out)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Running || h.PID != 4242 {
		t.Fatalf("got %+v", h)
	}
	wantArgv := []string{"/usr/local/bin/scion", "server", "start", "--foreground", "--enable-web=true", "--dev-auth=false", "--web-port=8080"}
	if !reflect.DeepEqual(h.Argv, wantArgv) {
		t.Fatalf("argv = %q", h.Argv)
	}
	// The value keeps everything after the first "=".
	wantEnv := map[string]string{
		"SCION_SERVER_AUTH_MODE":               "proxy",
		"SCION_SERVER_AUTH_PROXY_JWT_AUDIENCE": "scion-hub:x=y",
		"LEVER_HUB_AUTH_KEY":                   "sha256:abc",
	}
	if !reflect.DeepEqual(h.Env, wantEnv) {
		t.Fatalf("env = %v", h.Env)
	}
}

func TestParseRunningHubEmptyEnvSection(t *testing.T) {
	h, err := parseRunningHub("12\nscion\nserver\n--lever-env--\n")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Running || h.PID != 12 || len(h.Env) != 0 || h.Env == nil {
		t.Fatalf("got %+v", h)
	}
}

func TestParseRunningHubMalformed(t *testing.T) {
	for _, out := range []string{
		"notapid\nscion\n--lever-env--\n",
		"12x\n--lever-env--\n",
		// A pid without the separator: the env section cannot be told apart.
		"12\nscion\nserver\n",
	} {
		if h, err := parseRunningHub(out); err == nil {
			t.Fatalf("%q: want an error, got %+v", out, h)
		}
	}
}

func TestRunningHubScriptFiltersTheEnvInTheGuest(t *testing.T) {
	for _, p := range HubEnvPrefixes {
		if !strings.Contains(runningHubScript, p) {
			t.Fatalf("script does not admit %s", p)
		}
	}
	if !strings.Contains(runningHubScript, "grep -E '^(") {
		t.Fatal("env is not filtered by an anchored allow-list")
	}
	if !strings.Contains(runningHubScript, "grep -v '^SCION_SERVER_SESSION'") {
		t.Fatal("script does not drop SCION_SERVER_SESSION*")
	}
	for _, p := range HubEnvPrefixes {
		if strings.HasPrefix("SCION_HUB_TOKEN", p) || strings.HasPrefix("SCION_SERVER_SESSION_SECRET", p) {
			t.Fatalf("prefix %s admits a credential", p)
		}
	}
}

func TestRunningHubCallsTheScriptOnThePidFile(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("sh -c", proc.Result{Stdout: "7\nscion\nserver\n--lever-env--\nLEVER_HUB_AUTH_KEY=k\n"})
	c := New(f, Options{})
	h, err := c.RunningHub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !h.Running || h.PID != 7 || h.Env["LEVER_HUB_AUTH_KEY"] != "k" {
		t.Fatalf("got %+v", h)
	}
	call := f.Calls[0]
	if call.Name != "sh" || len(call.Args) != 5 || call.Args[1] != runningHubScript || call.Args[3] != layout.ServerPIDRel || call.Args[4] != layout.ScionBin {
		t.Fatalf("call = %+v", call)
	}
}

func TestRunningHubProbeFailureIsAnError(t *testing.T) {
	c := New(proc.NewFakeRunner(), Options{})
	if _, err := c.RunningHub(context.Background()); err == nil {
		t.Fatal("want an error when the probe cannot run")
	}
}

// The script against a real shell and /proc (Linux only): a live process
// whose argv looks like a scion server, with credentials and unrelated keys in
// its environment.
func TestRunningHubScriptRunsForReal(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("needs /proc")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, layout.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// "; :" keeps sh from exec'ing sleep, so the pid keeps this argv.
	hub := exec.Command("sh", "-c", "sleep 30; :", "scion", "server", "start", "--foreground", "--dev-auth=false")
	hub.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"SCION_SERVER_AUTH_MODE=proxy",
		"SCION_SERVER_AUTH_USERACCESSMODE=invite_only",
		"SCION_SERVER_HUB_ADMINEMAILS=lever-admin@lever.local",
		"LEVER_HUB_AUTH_KEY=fp",
		"SCION_SERVER_SESSION_SECRET=mustnotleak",
		"SCION_HUB_TOKEN=mustnotleak",
		"SCION_SERVER_DEV_MODE=mustnotleak",
		"OTHER=mustnotleak",
	}
	if err := hub.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hub.Process.Kill(); _ = hub.Wait() }()
	if err := os.WriteFile(filepath.Join(home, layout.ServerPIDRel), []byte(strconv.Itoa(hub.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The script counts only a process running the installed scion ($2): here
	// that is the shell the fake hub runs in.
	bin, err := os.Readlink("/proc/" + strconv.Itoa(hub.Process.Pid) + "/exe")
	if err != nil {
		t.Skip("cannot read the fake hub's exe")
	}
	decoy := exec.Command("sh", "-c", runningHubScript, "_", layout.ServerPIDRel, layout.ScionBin)
	decoy.Env = append(os.Environ(), "HOME="+home)
	if out, err := decoy.Output(); err != nil || len(out) != 0 {
		t.Fatalf("a server argv on another binary read as the hub: %q %v", out, err)
	}
	var out []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmd := exec.Command("sh", "-c", runningHubScript, "_", layout.ServerPIDRel, bin)
		cmd.Env = append(os.Environ(), "HOME="+home)
		var err error
		if out, err = cmd.Output(); err != nil {
			t.Fatalf("script: %v", err)
		}
		if len(out) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(string(out), "mustnotleak") {
		t.Fatalf("script read out a key outside the allow-list:\n%s", out)
	}
	h, err := parseRunningHub(string(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SCION_SERVER_AUTH_MODE":           "proxy",
		"SCION_SERVER_AUTH_USERACCESSMODE": "invite_only",
		"SCION_SERVER_HUB_ADMINEMAILS":     "lever-admin@lever.local",
		"LEVER_HUB_AUTH_KEY":               "fp",
	}
	if !h.Running || h.PID != hub.Process.Pid || !reflect.DeepEqual(h.Env, want) {
		t.Fatalf("got %+v", h)
	}
	if !strings.Contains(strings.Join(h.Argv, " "), "scion server start --foreground --dev-auth=false") {
		t.Fatalf("argv = %q", h.Argv)
	}
}

// A pid file naming a process that is not a scion server reads as no hub;
// unlike the stop probe this one never removes the file.
func TestRunningHubScriptIgnoresANonServerPid(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("needs /proc")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, layout.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	pidFile := filepath.Join(home, layout.ServerPIDRel)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", runningHubScript, "_", layout.ServerPIDRel)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("script printed %q (err %v), want nothing", out, err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("pid file gone: %v", err)
	}
}

func TestServerStartCarriesEnvWithoutOverridingTheClientsOwn(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "ok"})
	c := New(f, Options{HubEndpoint: "http://127.0.0.1:8080", HubTokenSource: func() string { return "scion_pat_real" }})
	env := map[string]string{
		"SCION_SERVER_AUTH_MODE":           "proxy",
		"SCION_SERVER_AUTH_USERACCESSMODE": "invite_only",
		"SCION_HUB_TOKEN":                  "scion_pat_other",
		"SCION_HUB_ENDPOINT":               "http://evil",
		"SCION_HUB_ENABLED":                "false",
	}
	if err := c.ServerStart(context.Background(), ServerOpts{WebPort: 8080, Env: env}); err != nil {
		t.Fatal(err)
	}
	got := serverStartCall(t, f).Env
	for k, v := range map[string]string{
		"SCION_SERVER_AUTH_MODE":           "proxy",
		"SCION_SERVER_AUTH_USERACCESSMODE": "invite_only",
		"SCION_HUB_TOKEN":                  "scion_pat_real",
		"SCION_HUB_ENDPOINT":               "http://127.0.0.1:8080",
		"SCION_HUB_ENABLED":                "",
	} {
		if got[k] != v {
			t.Errorf("start env %s = %q, want %q", k, got[k], v)
		}
	}
	// Only the start carries it: the other calls of the same start do not.
	for _, call := range f.Calls {
		if call.HasPrefix("scion", "server", "start") {
			continue
		}
		if _, ok := call.Env["SCION_SERVER_AUTH_MODE"]; ok {
			t.Errorf("%v carries the hub auth env", call.Args)
		}
	}
}

func TestServerStartNoLongerEmitsThrowawayFlags(t *testing.T) {
	for _, o := range []ServerOpts{
		{WebPort: 8080},
		{WebPort: 8080, EnableWeb: true, WebAssetsDir: "/w", Env: map[string]string{"A": "b"}},
	} {
		f, c := okScion()
		if err := c.ServerStart(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		args := serverStartCall(t, f).Args
		for _, a := range args {
			if a == "--port" || strings.HasPrefix(a, "--port=") || a == "--enable-web=false" || strings.HasPrefix(a, "--enable-runtime-broker") {
				t.Fatalf("args = %q", args)
			}
			if strings.Contains(a, "=b") {
				t.Fatalf("env leaked into the argv: %q", args)
			}
		}
	}
}
