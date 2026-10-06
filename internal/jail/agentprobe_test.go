package jail

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
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
	if !strings.Contains(argv, "podman exec lever--assistant sh -c") || !strings.Contains(argv, "cut -d. -f2") {
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
		{"no tmux server", proc.Result{Code: 1, Stderr: "no server running on /tmp/tmux-1000/default"}, false, false},
		{"container not running", proc.Result{Code: 125, Stderr: "Error: can only create exec sessions on running containers"}, false, true},
	} {
		var calls []string
		jr := New(Config{Host: codeRunner{c.res, &calls}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
		alive, err := AgentProbe{R: jr}.HarnessAlive(context.Background(), "lever--assistant")
		if alive != c.alive || (err != nil) != c.wantErr {
			t.Errorf("%s: alive=%v err=%v, want %v err=%v", c.name, alive, err, c.alive, c.wantErr)
		}
		if argv := calls[0]; !strings.Contains(argv, "podman exec lever--assistant tmux list-panes -t scion:agent") {
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
	if !strings.HasSuffix(argv, "podman exec lever--assistant sciontool hook --dialect=claude SessionStart") {
		t.Fatalf("argv %q", argv)
	}
	if strings.Contains(argv, "--user") {
		t.Fatal("the hook runs as the container's own user, like Claude Code's")
	}
	jr = New(Config{Host: failingRunner{`Error: no such container "x"`}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if err := (AgentProbe{R: jr}).ReportSessionRunning(context.Background(), "x"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("err = %v", err)
	}
}
