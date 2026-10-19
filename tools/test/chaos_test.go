package test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The chaos e2e sources its helpers from the stack e2e and lima-e2e.sh by
// name: a renamed or reshaped helper must fail here, not an hour into a live
// run.
func TestChaosE2EParsesAndFindsItsHelpers(t *testing.T) {
	for _, tool := range []string{"bash", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	if out, err := exec.Command("bash", "-n", "lima-chaos-e2e.sh").CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	cmd := exec.Command("bash", "lima-chaos-e2e.sh")
	cmd.Env = append(os.Environ(), "CHAOS_SELFTEST=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "chaos e2e helpers: ok") {
		t.Fatalf("self test: %v\n%s", err, out)
	}
}
