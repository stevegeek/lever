package manager

import (
	"testing"

	"github.com/stevegeek/lever/internal/cli/clitest"
)

func TestManagerRootHasOrchestrationOnly(t *testing.T) {
	n := clitest.Names(NewRoot())
	for _, want := range []string{"agent", "msg", "watch", "version"} {
		if !n[want] {
			t.Errorf("manager root missing %q", want)
		}
	}
	for _, unwanted := range []string{"up", "apply", "down", "doctor", "provision"} {
		if n[unwanted] {
			t.Errorf("manager root should not have %q", unwanted)
		}
	}
}
