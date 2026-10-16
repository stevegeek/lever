package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/progress"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// TestPhaseOrAbsent: a probe error proving the manager cannot be running reads
// as absent, so up applies. Three signatures: hub unreachable (fresh machine),
// hub project not found (registered locally but never hub-linked), and no
// local registration (scion's git-origin fallback). Any other error must
// propagate: apply is not fully idempotent, so a transient list failure must
// not force a re-apply.
func TestPhaseOrAbsent(t *testing.T) {
	freshErr := errors.New(`scion list --format json -g /lever: Error: Hub at http://127.0.0.1:8080 is not responding: Get "http://127.0.0.1:8080/api/health": dial tcp 127.0.0.1:8080: connect: connection refused`)
	notFoundErr := errors.New(`scion list --format json -g /lever: Error: failed to list agents via Hub: not_found: Project not found (status: 404)`)
	// scion's git-origin fallback when the path is not a registered project (see
	// internal/scion/bringup.go).
	noOriginErr := errors.New(`scion list --format json -g /lever: Error: no git origin remote found for this project.`)
	cases := []struct {
		name    string
		phase   string
		err     error
		want    string
		wantErr bool
	}{
		{"no error passes phase through unchanged", "running", nil, "running", false},
		{"no error, absent phase stays absent", "", nil, "", false},
		{"live fresh-machine hub error is treated as absent", "", freshErr, "", false},
		{"'is not responding' alone is treated as absent, case-insensitively", "", errors.New("Hub at http://127.0.0.1:8080 IS NOT RESPONDING"), "", false},
		{"'connection refused' alone is treated as absent, case-insensitively", "", errors.New("dial tcp 127.0.0.1:8080: connect: Connection Refused"), "", false},
		{"hub-unreachable overrides any stale phase value", "running", freshErr, "", false},
		{"live project-not-found (404) error is treated as absent", "", notFoundErr, "", false},
		{"'project not found' alone is treated as absent, case-insensitively", "running", errors.New("not_found: PROJECT NOT FOUND (status: 404)"), "", false},
		{"live no-git-origin (unregistered project) error is treated as absent", "", noOriginErr, "", false},
		{"'no git origin remote found' alone is treated as absent, case-insensitively", "running", errors.New("Error: NO GIT ORIGIN REMOTE FOUND for this project."), "", false},
		{"any other error propagates (no forced re-apply)", "running", errors.New("could not parse scion JSON output: unexpected JSON"), "", true},
		{"auth-ish error propagates", "", errors.New("scion list: 401 unauthorized"), "", true},
	}
	for _, c := range cases {
		got, err := phaseOrAbsent(c.phase, c.err)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: phaseOrAbsent(%q,%v) err=%v wantErr=%v", c.name, c.phase, c.err, err, c.wantErr)
			continue
		}
		if c.wantErr && err != c.err {
			t.Errorf("%s: propagated error %v is not the original %v", c.name, err, c.err)
		}
		if got != c.want {
			t.Errorf("%s: phaseOrAbsent(%q,%v)=%q want %q", c.name, c.phase, c.err, got, c.want)
		}
	}
}

func TestUpDecision(t *testing.T) {
	cases := []struct {
		phase string // "" = absent
		fresh bool
		want  upAction
	}{
		{"", false, upApply},
		{"suspended", false, upKeep},
		{"running", false, upKeep},
		{"stopped", false, upApply},
		// --fresh is always the apply path: start-manager discards any present record
		// once the hub is up. After `lever stop` the hub is down and the record
		// invisible to the probe, and apply keeps an error-phase record whose forced
		// resume died, so --fresh must reach apply.
		{"running", true, upApply},
		{"suspended", true, upApply},
		{"stopped", true, upApply},
		{"error", true, upApply},
		{"", true, upApply},
	}
	for _, c := range cases {
		if got := upDecision(c.phase, c.fresh); got != c.want {
			t.Errorf("upDecision(%q,%v)=%q want %q", c.phase, c.fresh, got, c.want)
		}
	}
}

// The --fresh discard itself (`scion delete`, never `scion stop`, which leaves
// a resumable record) is pinned in TestStartManagerFreshDiscardsPresentRecord.

// TestFirstLine: scion's error carries its usage dump after the first line,
// which must never reach the terminal.
func TestFirstLine(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"multi-line input keeps only the first line",
			"Error: Hub at http://127.0.0.1:8080 is not responding: dial tcp: connect: connection refused\n\nUsage:\n  scion list [flags]\n\nFlags:\n  -h, --help   help for list\n",
			"Error: Hub at http://127.0.0.1:8080 is not responding: dial tcp: connect: connection refused",
		},
		{"single line input is unchanged", "no git origin remote found for this project.", "no git origin remote found for this project."},
		{"empty input stays empty", "", ""},
		{"leading/trailing whitespace on the first line is trimmed", "  Error: project not found (status: 404)  \nUsage:\n  scion list\n", "Error: project not found (status: 404)"},
		// scion colours its errors; a compromised jail can emit anything.
		{"ANSI colour on the first line is stripped", "\x1b[1;31mError:\x1b[0m hub down\nUsage:\n", "Error: hub down"},
		{"OSC title and CR on the first line are neutralised", "ok\x1b]0;pwned\x07\rrow\nrest", "ok\ufffdrow"},
	}
	for _, c := range cases {
		if got := firstLine(c.input); got != c.want {
			t.Errorf("%s: firstLine(%q)=%q want %q", c.name, c.input, got, c.want)
		}
	}
}

// brokerReusable: only a broker whose /epoch identity matches this binary and
// broker config is kept; empty fields always mismatch.
func TestBrokerReusable(t *testing.T) {
	got := wire.EpochResponse{Version: "v", ConfigHash: "h"}
	if !brokerReusable(got, "v", "h") {
		t.Fatal("matching identity must be reusable")
	}
	if brokerReusable(got, "v2", "h") {
		t.Fatal("binary version drift must force a restart")
	}
	if brokerReusable(got, "v", "h2") {
		t.Fatal("config drift must force a restart")
	}
	if brokerReusable(wire.EpochResponse{}, "v", "h") {
		t.Fatal("an old broker (no identity fields) must force a restart")
	}
}

// TestUpProbeNotice: with --fresh the notice must say a record found once the
// hub is up will be discarded, not read as a silent downgrade to resume.
func TestUpProbeNotice(t *testing.T) {
	err := errors.New("hub is not responding\nUsage: scion list ...")
	plain := upProbeNotice(err, false)
	if !strings.Contains(plain, "hub is not running") || strings.Contains(plain, "Usage") || strings.Contains(plain, "--fresh") {
		t.Errorf("plain notice = %q", plain)
	}
	fresh := upProbeNotice(err, true)
	if !strings.Contains(fresh, "--fresh") || !strings.Contains(fresh, "discard") || !strings.Contains(fresh, "hub is not running") {
		t.Errorf("fresh notice = %q, want the flag, the discard, and the reason", fresh)
	}
}

// TestUpProbeReason: the probe failure in the operator's words, never the raw
// client error; an unknown error keeps its first line only.
func TestUpProbeReason(t *testing.T) {
	cases := map[string]string{
		"dial tcp 127.0.0.1:8080: connect: connection refused": "hub is not running",
		"Error: project not found":                             "not registered with the hub yet",
		"weird\nUsage: scion list":                             "no running manager (weird)",
	}
	for in, want := range cases {
		if got := upProbeReason(errors.New(in)); !strings.Contains(got, want) || strings.Contains(got, "Usage") {
			t.Errorf("upProbeReason(%q) = %q, want it to contain %q", in, got, want)
		}
	}
}

func TestUpDecisionNote(t *testing.T) {
	cases := []struct {
		phase string
		d     upAction
		want  string
	}{
		{scion.PhaseRunning, upKeep, "running"},
		{scion.PhaseSuspended, upKeep, "suspended; resuming it"},
		{"", upApply, "no manager yet; starting it"},
	}
	for _, c := range cases {
		if got := upDecisionNote(c.phase, c.d); got != c.want {
			t.Errorf("upDecisionNote(%q, %q) = %q, want %q", c.phase, c.d, got, c.want)
		}
	}
}

// TestEnsureRemoteForUp: up checks the proxy only with remote access on, and
// a proxy it cannot start is a warning naming the fix, not a failed up.
func TestEnsureRemoteForUp(t *testing.T) {
	rep := progress.New(progress.Options{Mode: progress.ModeQuiet, Command: "up"})
	on := &config.App{Name: "assistant", Remote: config.Remote{Enabled: true}}
	called := false
	if w := ensureRemoteForUp(context.Background(), rep, &config.App{Name: "assistant"}, func(context.Context) error { called = true; return nil }); w != "" || called {
		t.Fatalf("remote off: warning %q, ensure called %v; want neither", w, called)
	}
	if w := ensureRemoteForUp(context.Background(), rep, on, func(context.Context) error { return nil }); w != "" {
		t.Fatalf("a serving proxy warned: %q", w)
	}
	for _, err := range []error{errRemoteProxyNoStamp, errRemoteProxyStale, errRemoteProxyStateInTree, errors.New("bind: address already in use")} {
		w := ensureRemoteForUp(context.Background(), rep, on, func(context.Context) error { return err })
		if !strings.Contains(w, "proxy") || !strings.Contains(w, "lever apply") || !strings.Contains(w, err.Error()) {
			t.Errorf("warning for %v = %q, want the cause and `lever apply`", err, w)
		}
	}
}

// TestEnsureBrokerForUp: up's keep path starts (or reuses) the host broker
// as apply does and says whether it staged a ticket; a broker that does not
// come up fails the up.
func TestEnsureBrokerForUp(t *testing.T) {
	rep := progress.New(progress.Options{Mode: progress.ModeQuiet, Command: "up"})
	var calls []string
	deps := apply.Deps{
		StartBroker:   func(context.Context) error { calls = append(calls, "start"); return nil },
		BrokerHealthy: func(context.Context) error { calls = append(calls, "healthy"); return nil },
		MintManagerBootstrap: func(context.Context) (apply.BootstrapMaterial, error) {
			calls = append(calls, "mint")
			return apply.BootstrapMaterial{}, apply.ErrBootstrapLatched
		},
	}
	if staged, err := ensureBrokerForUp(context.Background(), rep, deps, nil, t.TempDir()); err != nil || staged {
		t.Fatalf("staged %v err %v", staged, err)
	}
	if got := strings.Join(calls, ","); got != "start,healthy,mint" {
		t.Fatalf("calls = %s", got)
	}
	deps.BrokerHealthy = func(context.Context) error { return errors.New("broker did not become healthy") }
	_, err := ensureBrokerForUp(context.Background(), rep, deps, nil, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "broker-up: broker did not become healthy") {
		t.Fatalf("err = %v", err)
	}
}
