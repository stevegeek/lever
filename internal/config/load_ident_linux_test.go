package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

const bindChildEnv = "LEVER_CONFIG_BIND_CHILD"

// A bind mount of the tree gives it a second path that shares no prefix
// with the tree's: the check at load compares file identity, not spelling.
// Under unshare -rm the test binary re-runs itself with the tree bound at
// base/alias.
func TestLoadRejectsScionSourceInATreeBindMount(t *testing.T) {
	if cfg := os.Getenv(bindChildEnv); cfg != "" {
		_, err := LoadNoHostChecks(cfg)
		testutil.WantErrContaining(t, err, "scion.source", "inside the mounted tree")
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	if out, err := exec.Command("unshare", "-rm", "true").CombinedOutput(); err != nil {
		t.Skipf("unshare -rm is not available: %v: %s", err, out)
	}
	base := t.TempDir()
	inst := filepath.Join(base, "inst")
	for _, d := range []string{filepath.Join(inst, "ws", "src"), filepath.Join(base, "alias")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(inst, "lever.yaml")
	body := "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\nbroker:\n  llm_auth: subscription\nscion:\n  source: " +
		filepath.Join(base, "alias", "src") + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Outside the namespace the alias is an empty directory, so the path is
	// outside the tree.
	if _, err := LoadNoHostChecks(cfg); err != nil {
		t.Fatalf("without the mount: %v", err)
	}
	script := `mount --bind "$1/inst/ws" "$1/alias" && exec "$2" -test.run '^TestLoadRejectsScionSourceInATreeBindMount$' -test.v`
	cmd := exec.Command("unshare", "-rm", "sh", "-c", script, "sh", base, os.Args[0])
	cmd.Env = append(os.Environ(), bindChildEnv+"="+cfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	t.Logf("child:\n%s", out)
}
