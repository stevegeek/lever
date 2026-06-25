package cli

import (
	"strings"
	"testing"

	"github.com/lever-to/lever/internal/config"
)

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
