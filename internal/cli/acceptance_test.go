package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/config"
	leverexec "github.com/lever-to/lever/internal/exec"
)

// TestClassifyCurlResult asserts the pure egress-classification helper correctly
// maps curl exit codes to blocked/allowed/uncertain without touching the live jail.
// This makes the critical egress discrimination CI-testable.
func TestClassifyCurlResult(t *testing.T) {
	errNonZero := fmt.Errorf("exit 1") // sentinel — classifyCurlResult only checks err == nil, not the value

	cases := []struct {
		name        string
		res         leverexec.Result
		err         error
		wantState   string
		wantErrNil  bool
		wantErrSnip string // substring expected in error message (if non-nil)
	}{
		{
			name:       "exit 0 = allowed (egress open)",
			res:        leverexec.Result{Code: 0, Stdout: "<html>", Stderr: ""},
			err:        nil,
			wantState:  "allowed",
			wantErrNil: true,
		},
		{
			name:       "exit 7 = blocked (CURLE_COULDNT_CONNECT)",
			res:        leverexec.Result{Code: 7, Stderr: "curl: (7) Failed to connect"},
			err:        errNonZero,
			wantState:  "blocked",
			wantErrNil: true,
		},
		{
			name:       "exit 28 = blocked (CURLE_OPERATION_TIMEDOUT)",
			res:        leverexec.Result{Code: 28, Stderr: "curl: (28) Operation timed out"},
			err:        errNonZero,
			wantState:  "blocked",
			wantErrNil: true,
		},
		{
			name:        "exit 127 = uncertain (curl not found by exit code)",
			res:         leverexec.Result{Code: 127, Stderr: "bash: curl: command not found"},
			err:         errNonZero,
			wantState:   "uncertain",
			wantErrNil:  false,
			wantErrSnip: "not found",
		},
		{
			name:        "exit 126 with not-found text = uncertain (curl absent, shell absorbed code)",
			res:         leverexec.Result{Code: 126, Stderr: "curl: No such file or directory"},
			err:         errNonZero,
			wantState:   "uncertain",
			wantErrNil:  false,
			wantErrSnip: "not found",
		},
		{
			name:        "exit 6 (DNS failure) = uncertain (FAIL-CLOSED)",
			res:         leverexec.Result{Code: 6, Stderr: "curl: (6) Could not resolve host"},
			err:         errNonZero,
			wantState:   "uncertain",
			wantErrNil:  false,
			wantErrSnip: "FAIL-CLOSED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, err := classifyCurlResult(tc.res, tc.err)
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if tc.wantErrNil && err != nil {
				t.Errorf("want nil error, got %v", err)
			}
			if !tc.wantErrNil && err == nil {
				t.Errorf("want non-nil error, got nil")
			}
			if tc.wantErrSnip != "" && err != nil && !strings.Contains(err.Error(), tc.wantErrSnip) {
				t.Errorf("error %q missing expected snippet %q", err.Error(), tc.wantErrSnip)
			}
		})
	}
}

// TestVMIDDirPerRole asserts the per-role VM identity dirs are distinct and
// non-empty (the manager delegates, the worker exercises — they must not share
// an identity directory).
func TestVMIDDirPerRole(t *testing.T) {
	m := vmIDDir("manager")
	w := vmIDDir("worker")
	if m == w || m == "" || w == "" {
		t.Fatalf("per-role VM identity dirs must be distinct and non-empty: %q %q", m, w)
	}
}

// TestEgressVerdict asserts the pure egress decision: PASS iff the allowlisted
// broker jail port is reachable AND the non-allowlisted admin port is blocked;
// every other combination FAILS (fail-closed).
func TestEgressVerdict(t *testing.T) {
	cases := []struct {
		name      string
		jail      string
		admin     string
		wantPass  bool
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "jail reachable + admin blocked = PASS",
			jail:     "reachable",
			admin:    "blocked",
			wantPass: true,
			wantErr:  false,
		},
		{
			name:      "jail reachable + admin reachable = FAIL (admin not contained)",
			jail:      "reachable",
			admin:     "reachable",
			wantPass:  false,
			wantErr:   true,
			errSubstr: "ADMIN port",
		},
		{
			name:      "jail blocked = FAIL-CLOSED (allowlist or broker down)",
			jail:      "blocked",
			admin:     "blocked",
			wantPass:  false,
			wantErr:   true,
			errSubstr: "jail port not reachable",
		},
		{
			name:      "jail uncertain = FAIL-CLOSED",
			jail:      "uncertain",
			admin:     "blocked",
			wantPass:  false,
			wantErr:   true,
			errSubstr: "jail port not reachable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pass, err := egressVerdict(tc.jail, tc.admin)
			if pass != tc.wantPass {
				t.Errorf("pass = %v, want %v", pass, tc.wantPass)
			}
			if tc.wantErr && err == nil {
				t.Errorf("want non-nil error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want nil error, got %v", err)
			}
			if tc.errSubstr != "" && err != nil && !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error %q missing expected snippet %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

// TestClassifyEgressProbe asserts the probe classifier: exit 0 and TLS-layer
// errors (35/60) count as reachable (the TCP connection succeeded — connecting is
// the point); exit 7/28 are blocked; curl-absent / unexpected exits are uncertain.
func TestClassifyEgressProbe(t *testing.T) {
	errNonZero := fmt.Errorf("exit") // classifyEgressProbe keys on err==nil + res.Code

	cases := []struct {
		name      string
		res       leverexec.Result
		err       error
		wantState string
	}{
		{name: "exit 0 = reachable", res: leverexec.Result{Code: 0}, err: nil, wantState: "reachable"},
		{name: "exit 35 (TLS connect) = reachable", res: leverexec.Result{Code: 35}, err: errNonZero, wantState: "reachable"},
		{name: "exit 60 (cert verify) = reachable", res: leverexec.Result{Code: 60}, err: errNonZero, wantState: "reachable"},
		{name: "exit 7 (refused) = blocked", res: leverexec.Result{Code: 7}, err: errNonZero, wantState: "blocked"},
		{name: "exit 28 (timeout/dropped) = blocked", res: leverexec.Result{Code: 28}, err: errNonZero, wantState: "blocked"},
		{name: "exit 127 (curl absent) = uncertain", res: leverexec.Result{Code: 127, Stderr: "command not found"}, err: errNonZero, wantState: "uncertain"},
		{name: "exit 6 (DNS) = uncertain", res: leverexec.Result{Code: 6, Stderr: "could not resolve"}, err: errNonZero, wantState: "uncertain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, err := classifyEgressProbe(tc.res, tc.err)
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if tc.wantState == "uncertain" && err == nil {
				t.Errorf("uncertain must carry a non-nil error")
			}
			if tc.wantState != "uncertain" && err != nil {
				t.Errorf("non-uncertain must carry a nil error, got %v", err)
			}
		})
	}
}

// TestAcceptanceWiredAndEnumeratesSixChecks asserts the `lever acceptance`
// command is wired into the host root and that exactly the six acceptance checks are
// enumerated, in the spec order.
func TestAcceptanceWiredAndEnumeratesSixChecks(t *testing.T) {
	root := newHostRootWith(defaultFactory)
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "acceptance" {
			found = true
		}
	}
	if !found {
		t.Fatal("`lever acceptance` not wired")
	}
	names := acceptanceCheckNames()
	if len(names) != 6 {
		t.Fatalf("expected 6 acceptance checks, got %d", len(names))
	}
	want := []string{
		"delegated-read",
		"no-table-c",
		"no-drop-filter",
		"no-self-path",
		"egress-refused",
		"revocation",
	}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("check[%d] = %q, want %q (order must match acceptance)", i, names[i], w)
		}
	}
}

// TestAcceptanceFixtureLoads asserts the live-path fixture lever.yaml parses
// and carries the acceptance shape: manager delegates db.read → worker, worker has an
// empty obtain, and the db tool is declared. (The full live run needs a real
// jail; this guards the fixture the RunE loads.)
func TestAcceptanceFixtureLoads(t *testing.T) {
	app, err := config.Load("testdata/acceptance/lever.yaml")
	if err != nil {
		t.Fatalf("acceptance fixture must load: %v", err)
	}
	if len(app.Manager.Delegate) != 1 || app.Manager.Delegate[0].Tool != "db" || app.Manager.Delegate[0].To[0] != "worker" {
		t.Fatalf("fixture must delegate db.read → worker, got %+v", app.Manager.Delegate)
	}
	var worker *config.Grove
	for i := range app.Groves {
		if app.Groves[i].Name == "worker" {
			worker = &app.Groves[i]
		}
	}
	if worker == nil {
		t.Fatal("fixture must declare grove `worker`")
	}
	if len(worker.Obtain) != 0 {
		t.Fatalf("worker must have an empty obtain (pure executor), got %+v", worker.Obtain)
	}
	if len(app.Broker.Tools) != 1 || app.Broker.Tools[0].Name != "db" {
		t.Fatalf("fixture must declare the `db` tool, got %+v", app.Broker.Tools)
	}
}

// TestFormatNoteDeterministicAndVerdict asserts formatNote renders a dated note
// that iterates acceptanceCheckNames() order (NOT map order), marks each check
// PASS/FAIL, and that a single FAIL flips the overall verdict to FAIL.
func TestFormatNoteDeterministicAndVerdict(t *testing.T) {
	date := "2026-06-25"

	// All-pass case: overall PASS.
	allPass := map[string]bool{
		"delegated-read": true,
		"no-table-c":     true,
		"no-drop-filter": true,
		"no-self-path":   true,
		"egress-refused": true,
		"revocation":     true,
	}
	note := formatNote(allPass, date)

	if !strings.Contains(note, date) {
		t.Fatalf("note missing date %q:\n%s", date, note)
	}
	// Every check named, in order, each marked PASS.
	lastIdx := -1
	for _, name := range acceptanceCheckNames() {
		idx := strings.Index(note, name)
		if idx < 0 {
			t.Fatalf("note missing check %q:\n%s", name, note)
		}
		if idx <= lastIdx {
			t.Fatalf("check %q out of order (expected acceptanceCheckNames() order):\n%s", name, note)
		}
		lastIdx = idx
	}
	if strings.Count(note, "PASS") < 6 {
		t.Fatalf("all-pass note should mark every check PASS:\n%s", note)
	}
	if !overallPass(allPass) {
		t.Fatal("overallPass(all true) should be true")
	}
	// Overall verdict line present and PASS.
	if !strings.Contains(note, "Overall: PASS") {
		t.Fatalf("all-pass note should declare Overall: PASS:\n%s", note)
	}

	// One FAIL flips the overall verdict.
	oneFail := map[string]bool{
		"delegated-read": true,
		"no-table-c":     true,
		"no-drop-filter": true,
		"no-self-path":   true,
		"egress-refused": false, // the one failure
		"revocation":     true,
	}
	if overallPass(oneFail) {
		t.Fatal("overallPass with a FAIL should be false")
	}
	failNote := formatNote(oneFail, date)
	if !strings.Contains(failNote, "Overall: FAIL") {
		t.Fatalf("note with a failing check should declare Overall: FAIL:\n%s", failNote)
	}
	if !strings.Contains(failNote, "egress-refused") || !strings.Contains(failNote, "FAIL") {
		t.Fatalf("note should mark egress-refused FAIL:\n%s", failNote)
	}
}
