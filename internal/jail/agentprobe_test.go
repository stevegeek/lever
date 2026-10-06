package jail

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// tokenPayload is the base64url payload segment of a JWT carrying claims.
func tokenPayload(claims string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(claims))
}

func TestAgentProbeHubToken(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: "1791307956\n" + tokenPayload(`{"sub":"a","scopes":["x"],"exp":1791313904}`) + "\n"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	got, err := AgentProbe{R: jr}.HubToken(context.Background(), "lever--assistant")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Expiry.Equal(time.Unix(1791313904, 0)) || !got.Now.Equal(time.Unix(1791307956, 0)) {
		t.Fatalf("times = %+v", got)
	}
	if got.Expired() {
		t.Fatal("a token 1.6 h from expiry is not expired")
	}
	if !got.RefreshOverdue() {
		t.Fatal("a token 1.6 h from expiry is past its 2 h refresh point")
	}
	argv := host.Calls[0].Argv()
	if !strings.Contains(argv, "podman exec --user scion lever--assistant sh -c") || !strings.Contains(argv, "cut -s -d. -f2") ||
		!strings.HasSuffix(argv, "sh "+AgentTokenPath) {
		t.Fatalf("argv %q", argv)
	}
	// The header and signature segments never leave the container.
	if strings.Contains(argv, "-f1") || strings.Contains(argv, "-f3") {
		t.Fatalf("argv %q reads more than the payload", argv)
	}
}

func TestHubTokenTimes(t *testing.T) {
	exp := time.Unix(1_000_000, 0)
	for _, c := range []struct {
		name             string
		now              time.Time
		expired, overdue bool
	}{
		{"fresh", exp.Add(-9 * time.Hour), false, false},
		{"refresh due", exp.Add(-2 * time.Hour), false, true},
		{"last minute", exp.Add(-time.Second), false, true},
		{"at expiry", exp, true, false},
		{"long expired", exp.Add(5 * time.Hour), true, false},
	} {
		tt := HubTokenTimes{Expiry: exp, Now: c.now}
		if tt.Expired() != c.expired || tt.RefreshOverdue() != c.overdue {
			t.Errorf("%s: expired=%v overdue=%v, want %v %v", c.name, tt.Expired(), tt.RefreshOverdue(), c.expired, c.overdue)
		}
	}
}

// TestParseHubTokenTimesNeverEchoes: the token file is the agent's to write,
// so a malformed one fails with a fixed message that repeats none of it.
func TestParseHubTokenTimesNeverEchoes(t *testing.T) {
	for _, out := range []string{
		"",
		"1791307956",
		"notaclock\n" + tokenPayload(`{"exp":1}`),
		"1791307956\n!!!SECRET-NOT-BASE64",
		"1791307956\n" + tokenPayload(`SECRET not json`),
		"1791307956\n" + tokenPayload(`{"exp":"SECRET"}`),
		"1791307956\n" + tokenPayload(`{"sub":"SECRET"}`),
		"1791307956\n" + tokenPayload(`{"exp":-5}`),
	} {
		_, err := parseHubTokenTimes(out)
		if err == nil {
			t.Fatalf("%q must not parse", out)
		}
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "notaclock") {
			t.Fatalf("error %q echoes the output", err)
		}
	}
	// Padding, as some encoders add it, is accepted.
	if _, err := parseHubTokenTimes("5\n" + base64.URLEncoding.EncodeToString([]byte(`{"exp":7}`))); err != nil {
		t.Fatalf("padded payload: %v", err)
	}
}

func TestAgentProbeHubTokenErrors(t *testing.T) {
	jr := New(Config{Host: failingRunner{`Error: no such container "x"`}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, err := (AgentProbe{R: jr}).HubToken(context.Background(), "x"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("err = %v, want ErrNoContainer", err)
	}
	jr = New(Config{Host: failingRunner{"cut: /home/scion/.scion/scion-token: SECRET"}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	_, err := AgentProbe{R: jr}.HubToken(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("a failed read must error without the container's stderr: %v", err)
	}
	host := proc.NewFakeRunner()
	if _, err := (AgentProbe{R: host}).HubToken(context.Background(), "--all"); err == nil || len(host.Calls) != 0 {
		t.Fatal("a flag-shaped ref must be refused before any call")
	}
}

// codeRunner answers every call with one result, as an error when its code
// is non-zero (FakeRunner returns a nil error for any scripted result), and
// records the argv.
type codeRunner struct {
	res   proc.Result
	calls *[]string
}

func (c codeRunner) Run(_ context.Context, _ map[string]string, name string, args ...string) (proc.Result, error) {
	*c.calls = append(*c.calls, proc.Call{Name: name, Args: args}.Argv())
	if c.res.Code != 0 {
		return c.res, errors.New("exit status")
	}
	return c.res, nil
}
func (c codeRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return c.Run(ctx, env, name, args...)
}
func (c codeRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return c.Run(ctx, env, name, args...)
}

func TestAgentProbeHarnessAlive(t *testing.T) {
	for _, c := range []struct {
		name    string
		res     proc.Result
		alive   bool
		wantErr bool
	}{
		{"pane alive", proc.Result{Stdout: "0\n"}, true, false},
		{"pane dead", proc.Result{Stdout: "1\n"}, false, false},
		{"window gone", proc.Result{Code: 1, Stderr: "can't find window: agent"}, false, false},
		{"session gone", proc.Result{Code: 1, Stderr: "can't find session: scion"}, false, false},
		{"no tmux server is unknown", proc.Result{Code: 1, Stderr: "no server running on /tmp/tmux-1000/default"}, false, true},
		{"socket error is unknown", proc.Result{Code: 1, Stderr: "error connecting to /tmp/tmux-1000/default (Permission denied)"}, false, true},
		{"container not running", proc.Result{Code: 125, Stderr: "Error: can only create exec sessions on running containers"}, false, true},
	} {
		var calls []string
		jr := New(Config{Host: codeRunner{c.res, &calls}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
		alive, err := AgentProbe{R: jr}.HarnessAlive(context.Background(), "lever--assistant")
		if alive != c.alive || (err != nil) != c.wantErr {
			t.Errorf("%s: alive=%v err=%v, want %v err=%v", c.name, alive, err, c.alive, c.wantErr)
		}
		if argv := calls[0]; !strings.Contains(argv, "podman exec --user scion lever--assistant tmux list-panes -t scion:agent") {
			t.Errorf("%s: argv %q", c.name, argv)
		}
	}
}

func TestAgentProbeReportSessionRunning(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if err := (AgentProbe{R: jr}).ReportSessionRunning(context.Background(), "lever--assistant"); err != nil {
		t.Fatal(err)
	}
	argv := host.Calls[0].Argv()
	if !strings.HasSuffix(argv, "podman exec --user scion lever--assistant sciontool hook --dialect=claude SessionStart") {
		t.Fatalf("argv %q: the hook runs as scion, the user scion's own execs use", argv)
	}
	jr = New(Config{Host: failingRunner{`Error: no such container "x"`}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if err := (AgentProbe{R: jr}).ReportSessionRunning(context.Background(), "x"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("err = %v", err)
	}
}

// runTokenScript runs agentTokenScript in a real sh against a file holding
// content, as the container would.
func runTokenScript(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scion-token")
	if content != nil {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := exec.Command("sh", "-c", agentTokenScript, "sh", path).Output()
	return string(out)
}

// TestAgentTokenScriptRealShell: only the payload segment crosses, bounded;
// a dotless file and a missing one yield no payload at all.
func TestAgentTokenScriptRealShell(t *testing.T) {
	payload := tokenPayload(`{"exp":1791313904}`)
	out := runTokenScript(t, []byte("HEADERSECRET."+payload+".SIGNATURESECRET\n"))
	if strings.Contains(out, "SECRET") {
		t.Fatalf("header or signature crossed: %q", out)
	}
	if got, err := parseHubTokenTimes(out); err != nil || got.Expiry.Unix() != 1791313904 {
		t.Fatalf("parse %q: %+v %v", out, got, err)
	}
	// A dotless file (not a JWT) prints the clock line only.
	out = runTokenScript(t, []byte("BARESECRETVALUE\n"))
	if strings.Contains(out, "BARESECRET") || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("a dotless file crossed: %q", out)
	}
	if _, err := parseHubTokenTimes(out); err == nil {
		t.Fatal("a dotless file must not parse")
	}
	// A missing file: no payload.
	out = runTokenScript(t, nil)
	if _, err := parseHubTokenTimes(out); err == nil {
		t.Fatalf("a missing file must not parse: %q", out)
	}
	// An oversize payload segment is cut at 8 KiB.
	big := "h." + strings.Repeat("A", 20000) + ".s"
	out = runTokenScript(t, []byte(big))
	if _, line, _ := strings.Cut(out, "\n"); len(line) > 8192 {
		t.Fatalf("payload line is %d bytes, want <= 8192", len(line))
	}
}

// hangRunner blocks every call until its context ends, like a probe reading
// a FIFO the agent put in place of its token file, and records the deadline
// and output cap it was given.
type hangRunner struct {
	deadlines []time.Duration
	limits    []int
}

func (h *hangRunner) Run(ctx context.Context, _ map[string]string, _ string, _ ...string) (proc.Result, error) {
	d, _ := ctx.Deadline()
	h.deadlines = append(h.deadlines, time.Until(d))
	n, _ := proc.OutputLimit(ctx)
	h.limits = append(h.limits, n)
	<-ctx.Done()
	return proc.Result{Code: -1}, ctx.Err()
}
func (h *hangRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return h.Run(ctx, env, name, args...)
}
func (h *hangRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return h.Run(ctx, env, name, args...)
}

// TestAgentProbeExecsAreBounded: every probe exec, and the read-only write
// probe, runs under its own deadline whatever the caller's context, so a
// hanging exec (a FIFO token file, a fake tmux server, a sciontool that never
// returns) ends in an error instead of hanging doctor, apply or the broker.
func TestAgentProbeExecsAreBounded(t *testing.T) {
	old := agentExecTimeout
	agentExecTimeout = 50 * time.Millisecond
	t.Cleanup(func() { agentExecTimeout = old })
	h := &hangRunner{}
	p := AgentProbe{R: h}
	ctx := context.Background() // no deadline of its own
	start := time.Now()
	_, err1 := p.HubToken(ctx, "lever--x")
	_, err2 := p.HarnessAlive(ctx, "lever--x")
	err3 := p.ReportSessionRunning(ctx, "lever--x")
	_, err4 := ContainerPathWritable(ctx, h, "lever--x", "/workspace/a")
	for i, err := range []error{err1, err2, err3, err4} {
		if err == nil {
			t.Fatalf("exec %d: a hung exec must end in an error", i)
		}
	}
	for i, d := range h.deadlines {
		if d <= 0 || d > agentExecTimeout {
			t.Fatalf("exec %d had deadline %s, want within %s", i, d, agentExecTimeout)
		}
	}
	if len(h.deadlines) != 4 || time.Since(start) > 5*time.Second {
		t.Fatalf("deadlines %v after %s", h.deadlines, time.Since(start))
	}
	for i, n := range h.limits {
		if n != agentExecOutputLimit {
			t.Fatalf("exec %d output cap = %d, want %d", i, n, agentExecOutputLimit)
		}
	}
}
