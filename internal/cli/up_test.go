package cli

import (
	"errors"
	"testing"
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
		want  string // "apply" | "resume" | "none" | "restart"
	}{
		{"", false, "apply"},
		{"suspended", false, "resume"},
		{"running", false, "none"},
		{"running", true, "restart"},
		{"suspended", true, "restart"},
		{"stopped", false, "apply"},
	}
	for _, c := range cases {
		if got := upDecision(c.phase, c.fresh); got != c.want {
			t.Errorf("upDecision(%q,%v)=%q want %q", c.phase, c.fresh, got, c.want)
		}
	}
}

// TestHubUnreachable covers the classifier that decides when a warm-resume
// restart-and-reprobe is worth attempting: ONLY when the error proves the hub
// PROCESS itself is down (as opposed to "hub up, but nothing registered"),
// since restarting the hub can't fix the latter.
func TestHubUnreachable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"'is not responding'", errors.New("Hub at http://127.0.0.1:8080 is not responding: dial tcp: connect: connection refused"), true},
		{"'is not responding', case-insensitive", errors.New("HUB IS NOT RESPONDING"), true},
		{"'connection refused' alone", errors.New("dial tcp 127.0.0.1:8080: connect: connection refused"), true},
		{"'connection refused', case-insensitive", errors.New("Connection Refused"), true},
		{"project-not-found is NOT hub-unreachable (hub is up)", errors.New("not_found: Project not found (status: 404)"), false},
		{"no-git-origin is NOT hub-unreachable (hub is up)", errors.New("no git origin remote found for this project."), false},
		{"unrelated error", errors.New("401 unauthorized"), false},
	}
	for _, c := range cases {
		if got := hubUnreachable(c.err); got != c.want {
			t.Errorf("%s: hubUnreachable(%v)=%v want %v", c.name, c.err, got, c.want)
		}
	}
}

// TestResolveManagerPhase covers the warm-resume orchestration: `lever stop`
// powers the OrbStack machine off, which kills the scion hub PROCESS but
// leaves the disk (hub.db, with the suspended manager still registered)
// intact. The very first probe after `up` therefore sees "hub not
// responding" — indistinguishable from a truly fresh machine. Restarting the
// (idempotent) hub and re-probing ONCE lets a real stop->up surface the
// suspended manager (-> upDecision "resume") instead of forcing a full
// re-apply, which would add a duplicate scion project-config and start a
// fresh manager thread rather than resuming the existing one.
func TestResolveManagerPhase(t *testing.T) {
	unreachableErr := errors.New("Hub at http://127.0.0.1:8080 is not responding: connection refused")
	notFoundErr := errors.New("not_found: Project not found (status: 404)")
	authErr := errors.New("scion list: 401 unauthorized")
	restartErr := errors.New("server start: exit status 1")

	cases := []struct {
		name         string
		probeResults []struct {
			phase string
			err   error
		}
		restartErr    error
		wantPhase     string
		wantErr       error
		wantProbes    int
		wantRestarted bool
	}{
		{
			name: "hub-unreachable then restart succeeds then resume surfaces: warm-resume win",
			probeResults: []struct {
				phase string
				err   error
			}{
				{"", unreachableErr},
				{"suspended", nil},
			},
			restartErr:    nil,
			wantPhase:     "suspended",
			wantErr:       nil,
			wantProbes:    2,
			wantRestarted: true,
		},
		{
			name: "hub-unreachable, restart succeeds, but hub still unreachable on reprobe: apply",
			probeResults: []struct {
				phase string
				err   error
			}{
				{"", unreachableErr},
				{"", unreachableErr},
			},
			restartErr:    nil,
			wantPhase:     "",
			wantErr:       nil,
			wantProbes:    2,
			wantRestarted: true,
		},
		{
			name: "hub-unreachable, restart succeeds, reprobe finds no manager (truly fresh machine): apply",
			probeResults: []struct {
				phase string
				err   error
			}{
				{"", unreachableErr},
				{"", nil},
			},
			restartErr:    nil,
			wantPhase:     "",
			wantErr:       nil,
			wantProbes:    2,
			wantRestarted: true,
		},
		{
			name: "hub-unreachable but restart itself errors: fall through on original result, no reprobe crash",
			probeResults: []struct {
				phase string
				err   error
			}{
				{"", unreachableErr},
			},
			restartErr:    restartErr,
			wantPhase:     "",
			wantErr:       nil,
			wantProbes:    1,
			wantRestarted: true,
		},
		{
			name: "probe succeeds first try: restartHub never called",
			probeResults: []struct {
				phase string
				err   error
			}{
				{"running", nil},
			},
			wantPhase:     "running",
			wantErr:       nil,
			wantProbes:    1,
			wantRestarted: false,
		},
		{
			name: "project-not-found is managerDefinitelyAbsent but NOT hub-unreachable: restartHub never called",
			probeResults: []struct {
				phase string
				err   error
			}{
				{"", notFoundErr},
			},
			wantPhase:     "",
			wantErr:       nil,
			wantProbes:    1,
			wantRestarted: false,
		},
		{
			name: "transient/unknown probe error propagates: restartHub never called",
			probeResults: []struct {
				phase string
				err   error
			}{
				{"", authErr},
			},
			wantPhase:     "",
			wantErr:       authErr,
			wantProbes:    1,
			wantRestarted: false,
		},
	}

	for _, c := range cases {
		probeCalls := 0
		probe := func() (string, error) {
			if probeCalls >= len(c.probeResults) {
				t.Fatalf("%s: probe called more times (%d) than results provided (%d)", c.name, probeCalls+1, len(c.probeResults))
			}
			r := c.probeResults[probeCalls]
			probeCalls++
			return r.phase, r.err
		}
		restarted := false
		restartHub := func() error {
			restarted = true
			return c.restartErr
		}

		phase, err := resolveManagerPhase(probe, restartHub)

		if phase != c.wantPhase {
			t.Errorf("%s: phase=%q want %q", c.name, phase, c.wantPhase)
		}
		if c.wantErr == nil && err != nil {
			t.Errorf("%s: err=%v want nil", c.name, err)
		}
		if c.wantErr != nil && err != c.wantErr {
			t.Errorf("%s: err=%v want %v", c.name, err, c.wantErr)
		}
		if probeCalls != c.wantProbes {
			t.Errorf("%s: probe called %d times, want %d", c.name, probeCalls, c.wantProbes)
		}
		if restarted != c.wantRestarted {
			t.Errorf("%s: restartHub called=%v want %v", c.name, restarted, c.wantRestarted)
		}
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
