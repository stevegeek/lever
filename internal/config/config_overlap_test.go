package config

import (
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

func testApp(t *testing.T, dirA, dirB string) *App {
	t.Helper()
	a := &App{Name: "demo", Backend: "orbstack", Tree: t.TempDir()}
	a.Broker.LLMAuth = "subscription"
	a.Workers = []Worker{{Name: "alpha", Dir: dirA}, {Name: "beta", Dir: dirB}}
	return a
}

// Sibling isolation rests on each worker mounting only its own subtree: with
// overlapping dirs the outer worker reads and writes the inner one's whole
// workspace.
func TestValidateRejectsOverlappingWorkerDirs(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"identical dirs", "workers/x", "workers/x"},
		{"ancestor first", "workers", "workers/child"},
		{"ancestor second", "workers/child", "workers"},
		{"unclean but identical", "workers/x", "./workers/x/"},
		{"unclean ancestor", "workers/./x", "workers/x/deep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t, tc.a, tc.b)
			err := app.Validate()
			if err == nil {
				t.Fatalf("overlapping worker dirs %q and %q must be rejected", tc.a, tc.b)
			}
			if !strings.Contains(err.Error(), "one") && !strings.Contains(err.Error(), "overlap") {
				t.Fatalf("error should explain the overlap, got %v", err)
			}
			testutil.WantErrContaining(t, err, "alpha", "beta")
		})
	}
}

func TestValidateAllowsDisjointWorkerDirs(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"siblings", "workers/x", "workers/y"},
		{"prefix but not a path component", "workers/x", "workers/xy"},
		{"different roots", "a/one", "b/two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := testApp(t, tc.a, tc.b).Validate(); err != nil {
				t.Fatalf("disjoint dirs %q and %q must be accepted: %v", tc.a, tc.b, err)
			}
		})
	}
}

// "manager" is the alias the directive channel resolves to the manager
// whatever broker.manager_identity is, so a directive aimed at a worker of
// that name would reach the manager.
func TestValidateRejectsAWorkerNamedManager(t *testing.T) {
	app := testApp(t, "workers/x", "workers/y")
	app.Broker.ManagerIdentity = "mgr"
	app.Workers[1].Name = "manager"
	err := app.Validate()
	testutil.WantErrContaining(t, err, "manager")
}
