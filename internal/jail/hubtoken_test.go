package jail

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// The controller PAT must never appear on the HOST command line: every
// argument of `orb`/`limactl` is readable by any local host user via `ps`.
// The runner hands it to the guest on the child's stdin instead, where a
// wrapper exports it and execs the real command; the public env keys keep
// their `env K=V` prefix.
func TestRunKeepsHubTokenOffHostArgv(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: "ok"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-jail", "leveruser"), UID: "501"})
	_, err := jr.Run(context.Background(),
		map[string]string{"SCION_HUB_TOKEN": "pat-secret-value", "SCION_HUB_ENDPOINT": "http://127.0.0.1:8080"},
		"scion", "list", "--format", "json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	call := host.Calls[0]
	argv := append([]string{call.Name}, call.Args...)
	if strings.Contains(strings.Join(argv, " "), "pat-secret-value") {
		t.Fatalf("token on the host argv: %q", argv)
	}
	if len(call.Env) != 0 {
		t.Fatalf("host env must stay empty (the prefix binary is not the consumer), got %v", call.Env)
	}
	want := []string{"orb", "-m", "lever-jail", "-u", "leveruser", "env",
		"XDG_RUNTIME_DIR=/run/user/501", "PATH=/usr/local/bin:/usr/bin:/bin", "SCION_HUB_ENABLED=true",
		"sh", "-c", hubTokenStdinScript, "_",
		"env", "SCION_HUB_ENDPOINT=http://127.0.0.1:8080", "scion", "list", "--format", "json"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv =\n %q\nwant\n %q", argv, want)
	}
	if call.Stdin != "pat-secret-value\n" {
		t.Fatalf("stdin = %q, want the token line", call.Stdin)
	}
}

// RunIn keeps `env -C <dir>` inside the wrapper, so the exec'd command still
// runs in the requested directory.
func TestRunInKeepsDirBehindHubTokenWrapper(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("limactl", proc.Result{})
	jr := New(Config{Host: host, Prefix: []string{"limactl", "shell", "lever-x"}, UID: "501"})
	_, err := jr.RunIn(context.Background(), "/lever/workers/a", map[string]string{"SCION_HUB_TOKEN": "pat-secret-value"}, "scion", "init")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(host.Calls[0].Args, " ")
	if !strings.HasSuffix(got, " _ env -C /lever/workers/a scion init") {
		t.Fatalf("argv %q must end with the wrapped env -C command", got)
	}
	if strings.Contains(got, "pat-secret-value") {
		t.Fatalf("token on the host argv: %q", got)
	}
}

// A stdin payload (an inline config for `--config -`, an image archive)
// follows the token line on the same stream; the guest wrapper consumes only
// the first line.
func TestRunStdinPrependsHubTokenLineToPayload(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	_, err := jr.RunStdin(context.Background(), strings.NewReader(`{"agent_instructions":"m"}`),
		map[string]string{"SCION_HUB_TOKEN": "pat-secret-value"}, "scion", "start", "--config", "-", "--", "a", "t")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := host.Calls[0].Stdin; got != "pat-secret-value\n"+`{"agent_instructions":"m"}` {
		t.Fatalf("stdin = %q", got)
	}
	if strings.Contains(strings.Join(host.Calls[0].Args, " "), "pat-secret-value") {
		t.Fatalf("token on the host argv: %q", host.Calls[0].Args)
	}
}

// Without a token nothing changes: no wrapper, no stdin, the exact argv every
// existing test pins (TestPrefixIsBackendShaped).
func TestRunWithoutHubTokenHasNoWrapper(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, err := jr.Run(context.Background(), map[string]string{"A": "1"}, "true"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(host.Calls[0].Args, " "); strings.Contains(got, "sh -c") {
		t.Fatalf("no wrapper without a token, got %q", got)
	}
	if host.Calls[0].Stdin != "" {
		t.Fatalf("no stdin without a token, got %q", host.Calls[0].Stdin)
	}
}

// The wrapper reads ONE line, so a token holding a line break could only
// reach the guest truncated. Refuse it rather than authenticate with half a
// credential.
func TestRunRefusesHubTokenWithLineBreak(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	for _, tok := range []string{"a\nb", "a\r", "\n"} {
		if _, err := jr.Run(context.Background(), map[string]string{"SCION_HUB_TOKEN": tok}, "true"); err == nil {
			t.Fatalf("token %q: want an error", tok)
		}
	}
	if len(host.Calls) != 0 {
		t.Fatalf("nothing may run with a malformed token; got %d call(s)", len(host.Calls))
	}
}

// The wrapper is a real shell script: run it through the host's own sh (no
// prefix) to prove it exports the token to the exec'd command and hands the
// REST of stdin on untouched — `read` on a pipe must consume exactly one
// line. On Linux CI this is dash, the guest's sh; on macOS it is bash in sh
// mode. Both read a pipe a byte at a time.
func TestHubTokenStdinScriptWithRealShell(t *testing.T) {
	jr := New(Config{Host: proc.RealRunner{}, Prefix: nil, UID: "501"})
	res, err := jr.RunStdin(context.Background(), strings.NewReader("payload line 1\npayload line 2"),
		map[string]string{"SCION_HUB_TOKEN": "pat-secret-value"},
		"sh", "-c", `printf '%s|' "$SCION_HUB_TOKEN"; cat`)
	if err != nil {
		t.Fatalf("run: %v (stderr %q)", err, res.Stderr)
	}
	if res.Stdout != "pat-secret-value|payload line 1\npayload line 2" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

// StageHubToken writes the PAT to a 0600 file in the run user's runtime
// directory (0700 tmpfs, gone on reboot), through the jail runner: the token
// travels on stdin, never on the host argv.
func TestStageHubTokenWritesThroughStdin(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if err := StageHubToken(context.Background(), jr, "pat-secret-value"); err != nil {
		t.Fatalf("StageHubToken: %v", err)
	}
	if len(host.Calls) != 1 {
		t.Fatalf("want exactly one guest call, got %d", len(host.Calls))
	}
	call := host.Calls[0]
	joined := strings.Join(append([]string{call.Name}, call.Args...), " ")
	if strings.Contains(joined, "pat-secret-value") {
		t.Fatalf("token on the host argv: %q", joined)
	}
	if call.Stdin != "pat-secret-value" {
		t.Fatalf("stdin = %q, want the token", call.Stdin)
	}
	if !strings.Contains(joined, "sh -c "+stageHubTokenScript) || !strings.Contains(joined, "umask 077") {
		t.Fatalf("argv %q must run the staging script under umask 077", joined)
	}
	if !strings.Contains(stageHubTokenScript, hubTokenFile) || !strings.Contains(hubTokenReadScript, hubTokenFile) {
		t.Fatal("writer and reader must name the same guest path")
	}
}

func TestStageHubTokenRefusesEmptyOrMultilineToken(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	for _, tok := range []string{"", "a\nb"} {
		if err := StageHubToken(context.Background(), jr, tok); err == nil {
			t.Fatalf("token %q: want an error", tok)
		}
	}
	if len(host.Calls) != 0 {
		t.Fatalf("nothing may run; got %d call(s)", len(host.Calls))
	}
}

// WithHubTokenFromFile wraps an interactive inner command so the guest shell
// reads the staged file into SCION_HUB_TOKEN before exec — the attach path
// cannot use stdin (that is the user's terminal), and the host argv then
// carries only a fixed script, no secret.
func TestWithHubTokenFromFileWrapsInner(t *testing.T) {
	inner := []string{"env", "SCION_HUB_ENDPOINT=http://127.0.0.1:8080", "scion", "attach", "a", "-g", "/lever"}
	got := WithHubTokenFromFile(inner)
	want := append([]string{"sh", "-c", hubTokenReadScript, "_"}, inner...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n %q\nwant\n %q", got, want)
	}
	if strings.Contains(hubTokenReadScript, "$1") {
		t.Fatal("the read script must not consume a positional: every positional is the inner command")
	}
}

// The read script, run by a real sh against a staged file, exports the token
// and execs the inner command with every positional intact.
func TestHubTokenReadScriptWithRealShell(t *testing.T) {
	dir := t.TempDir()
	r := proc.RealRunner{}
	// Stage through the same script the runner uses, with XDG_RUNTIME_DIR
	// pointed at a temp dir instead of the guest's /run/user/<uid>.
	res, err := r.RunStdin(context.Background(), strings.NewReader("pat-secret-value"),
		map[string]string{"XDG_RUNTIME_DIR": dir}, "sh", "-c", stageHubTokenScript)
	if err != nil {
		t.Fatalf("stage: %v (stderr %q)", err, res.Stderr)
	}
	argv := WithHubTokenFromFile([]string{"sh", "-c", `printf '%s|%s|%s' "$SCION_HUB_TOKEN" "$1" "$2"`, "_", "a b", "-g"})
	res, err = r.Run(context.Background(), map[string]string{"XDG_RUNTIME_DIR": dir}, argv[0], argv[1:]...)
	if err != nil {
		t.Fatalf("read: %v (stderr %q)", err, res.Stderr)
	}
	if res.Stdout != "pat-secret-value|a b|-g" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	res, err = r.Run(context.Background(), nil, "sh", "-c", `stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"`, "_", dir+"/lever-hub.pat")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "600" {
		t.Fatalf("staged file mode = %q, want 600", got)
	}
}

// No runtime dir means no place to stage: the read script and the stage
// script both refuse rather than fall back to a world-readable path.
func TestHubTokenScriptsRefuseWithoutRuntimeDir(t *testing.T) {
	r := proc.RealRunner{}
	if _, err := r.RunStdin(context.Background(), strings.NewReader("tok"),
		map[string]string{"XDG_RUNTIME_DIR": ""}, "sh", "-c", stageHubTokenScript); err == nil {
		t.Fatal("stage script must fail without XDG_RUNTIME_DIR")
	}
	argv := WithHubTokenFromFile([]string{"true"})
	if _, err := r.Run(context.Background(), map[string]string{"XDG_RUNTIME_DIR": ""}, argv[0], argv[1:]...); err == nil {
		t.Fatal("read script must fail without XDG_RUNTIME_DIR")
	}
}
