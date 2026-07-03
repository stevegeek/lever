package cli

import (
	"errors"
	"testing"
)

// TestPhaseOrAbsent covers the definitive-absence fallback: a failed phase
// probe whose error proves the manager cannot be running must be treated as
// "manager absent" so `up` falls through to upDecision -> "apply". Two such
// signatures: hub unreachable (fresh machine — the hub is only started by
// apply's scion-server step: "is not responding" / "connection refused") and
// hub-side project not found (hub up but the manager project never
// hub-registered, e.g. partial bring-up where `scion init` ran but
// `scion hub link` didn't: "Project not found (status: 404)"). ANY OTHER
// probe error must propagate: `lever apply` is not fully idempotent (each
// run leaves a duplicate scion project-configs entry), so a transient list
// failure on an already-up instance must NOT force a re-apply.
func TestPhaseOrAbsent(t *testing.T) {
	// The live fresh-machine repro (both signature fragments present).
	freshErr := errors.New(`scion list --format json -g /lever: Error: Hub at http://127.0.0.1:8080 is not responding: Get "http://127.0.0.1:8080/api/health": dial tcp 127.0.0.1:8080: connect: connection refused`)
	// The live partial-bring-up repro: hub up, manager project not registered.
	notFoundErr := errors.New(`scion list --format json -g /lever: Error: failed to list agents via Hub: not_found: Project not found (status: 404)`)
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
