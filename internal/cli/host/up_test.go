package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/wire"
)

// TestPhaseOrAbsent covers the definitive-absence fallback: a failed phase
// probe whose error proves the manager cannot be running must be treated as
// "manager absent" so `up` falls through to upDecision -> "apply". Three such
// signatures: hub unreachable (fresh machine — the hub is only started by
// apply's scion-server step: "is not responding" / "connection refused"),
// hub-side project not found (hub up but the manager project never
// hub-registered, e.g. partial bring-up where `scion init` ran but
// `scion hub link` didn't: "Project not found (status: 404)"), and no local
// registration at all (scion's git-origin fallback when a path isn't a
// registered project: "no git origin remote found"). ANY OTHER probe error
// must propagate: `lever apply` is not fully idempotent (each run leaves a
// duplicate scion project-configs entry), so a transient list failure on an
// already-up instance must NOT force a re-apply.
func TestPhaseOrAbsent(t *testing.T) {
	// The live fresh-machine repro (both signature fragments present).
	freshErr := errors.New(`scion list --format json -g /lever: Error: Hub at http://127.0.0.1:8080 is not responding: Get "http://127.0.0.1:8080/api/health": dial tcp 127.0.0.1:8080: connect: connection refused`)
	// The live partial-bring-up repro: hub up, manager project not registered.
	notFoundErr := errors.New(`scion list --format json -g /lever: Error: failed to list agents via Hub: not_found: Project not found (status: 404)`)
	// The live no-local-registration repro: scion's git-origin fallback when
	// the path isn't a registered project at all (this exact string is
	// documented in internal/scion/bringup.go).
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
		{"suspended", false, upResume},
		{"running", false, upNone},
		{"running", true, upRestart},
		{"suspended", true, upRestart},
		{"stopped", false, upApply},
		// --fresh discards ANY present record: since 0.12 apply preserves an
		// error-phase record whose forced resume comes up dead (#3), so
		// --fresh must be the escape hatch for a bricked record — not resume
		// the very conversation the user asked to discard.
		{"stopped", true, upRestart},
		{"error", true, upRestart},
		{"", true, upApply},
	}
	for _, c := range cases {
		if got := upDecision(c.phase, c.fresh); got != c.want {
			t.Errorf("upDecision(%q,%v)=%q want %q", c.phase, c.fresh, got, c.want)
		}
	}
}

// TestRestartManagerFreshIssuesDelete pins the "restart" decision's action:
// `--fresh` over a running/suspended manager must discard the record with
// `scion delete`, NOT `scion stop`. `scion stop` would leave a stopped record
// that start-manager's observe-first switch (internal/apply/run.go) treats as
// resumable — resuming it with `claude --continue` would restore the very
// conversation `--fresh` asked to discard.
func TestRestartManagerFreshIssuesDelete(t *testing.T) {
	f := scionOKRunner()
	sc := scion.New(f, scion.Options{})

	if err := restartManagerFresh(context.Background(), sc, "hello", "/lever"); err != nil {
		t.Fatalf("restartManagerFresh: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("expected exactly one scion call, got %+v", f.Calls)
	}
	call := f.Calls[0]
	if len(call.Args) == 0 || call.Args[0] != "delete" {
		got := strings.Join(call.Args, " ")
		t.Fatalf("restart must issue `scion delete`, not `scion stop`; got argv %q", got)
	}
}

// TestFirstLine covers the extraction used to keep the fresh-bring-up probe
// message to one short line: scion's error includes its entire usage dump
// after the first line, which must never reach the user's terminal.
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
	}
	for _, c := range cases {
		if got := firstLine(c.input); got != c.want {
			t.Errorf("%s: firstLine(%q)=%q want %q", c.name, c.input, got, c.want)
		}
	}
}

// brokerReusable (#19): apply's broker-reuse shortcut may only keep a running broker
// whose /epoch identity matches this binary AND this broker config. A broker
// predating the fields reports them empty — always a mismatch.
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

// TestVerifyManagerRoleGuardsUpsFastPaths: `up` resumes a suspended manager and
// no-ops a running one WITHOUT calling apply.Run, so the pre-role record guard
// has to be invoked on those paths explicitly. This pins that it is wired to
// the same Deps hook, keyed by the hub's project name (the mount basename).
func TestVerifyManagerRoleGuardsUpsFastPaths(t *testing.T) {
	var gotProject, gotAgent string
	deps := apply.Deps{
		VerifyAgentRole: func(_ context.Context, project, agent string) error {
			gotProject, gotAgent = project, agent
			return errors.New("no stored role")
		},
	}
	err := verifyManagerRole(context.Background(), deps, "/lever", "assistant")
	if err == nil {
		t.Fatal("the guard's refusal must reach the caller")
	}
	if gotProject != "lever" || gotAgent != "assistant" {
		t.Errorf("guard called with (%q, %q), want (\"lever\", \"assistant\")", gotProject, gotAgent)
	}
}
