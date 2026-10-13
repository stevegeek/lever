package host

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/testutil"
)

func TestClearStagedRuntimeState(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(tree, ".lever"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(tree, ".lever", "bootstrap.json")
	manifest := filepath.Join(tree, config.ManifestName)
	if err := os.WriteFile(bootstrap, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	clearStagedRuntimeState(&config.App{Tree: tree})

	if _, err := os.Stat(bootstrap); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap.json should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(manifest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tree, ".lever")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty .lever dir should be removed, stat err = %v", err)
	}
}

func TestClearStagedRuntimeStateMissingIsNoop(t *testing.T) {
	clearStagedRuntimeState(&config.App{Tree: t.TempDir()})
}

// TestRemoveControllerPAT: destroy clears the controller PAT: it is signed
// against the hub database that died with the machine, and reusing it fails
// the new hub's readiness auth.
func TestRemoveControllerPAT(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pat := st.ControllerPAT()
	if err := os.WriteFile(pat, []byte("stale-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeIfPresent(st.ControllerPAT()); err != nil {
		t.Fatalf("removeIfPresent: %v", err)
	}
	if _, err := os.Stat(pat); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("controller.pat should be removed, stat err = %v", err)
	}
}

func TestRemoveControllerPATMissingIsNoop(t *testing.T) {
	if err := removeIfPresent(state.ForConfig(t.TempDir()).ControllerPAT()); err != nil {
		t.Fatalf("missing PAT must be a no-op, got %v", err)
	}
}

// TestRemoveRemotePAT: destroy clears the remote PAT too, minted against the
// same dead hub database; left behind, ensureControllerPAT would skip its
// re-mint after a fresh `up`.
func TestRemoveRemotePAT(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pat := st.RemotePAT()
	if err := os.WriteFile(pat, []byte("stale-remote-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeIfPresent(st.RemotePAT()); err != nil {
		t.Fatalf("removeIfPresent: %v", err)
	}
	if _, err := os.Stat(pat); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote.pat should be removed, stat err = %v", err)
	}
}

func TestRemoveRemotePATMissingIsNoop(t *testing.T) {
	if err := removeIfPresent(state.ForConfig(t.TempDir()).RemotePAT()); err != nil {
		t.Fatalf("missing remote PAT must be a no-op, got %v", err)
	}
}

// TestDestroyAlsoStopsRemoteProxy: destroy must stop the remote proxy as stop
// does. A surviving proxy keeps its listeners and the tailscale front pointed
// at a machine that is gone, and with its stamp left the next `lever up`
// reuses it, with a cached jail prefix naming the destroyed machine.
func TestDestroyAlsoStopsRemoteProxy(t *testing.T) {
	dir := writeInstance(t, managerYAML)
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A real child, not the test's pid: destroy kills whatever remote.pid names.
	proxy, proxyDone := testutil.StartServeProcess(t, state.RemoteDaemon, filepath.Join(dir, "lever.yaml"))
	if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(proxy.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Only the removal matters, whatever the stamp holds.
	if err := os.WriteFile(st.RemoteStamp(), []byte("v0.17.0 deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := &stubBackend{}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"destroy"})
	if err := root.Execute(); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !sb.down {
		t.Fatal("destroy must still tear the jail down")
	}

	// Reaped with Wait, not signal 0: signal 0 succeeds on an unreaped zombie.
	select {
	case <-proxyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the remote proxy outlived `lever destroy`: it keeps serving a destroyed instance, and the next `up` would reuse it")
	}
	if _, err := os.Stat(st.RemotePID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote.pid should be removed by destroy, stat err = %v", err)
	}
	if _, err := os.Stat(st.RemoteStamp()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote.stamp should be removed by destroy — a surviving stamp is what makes a later `up` accept a stale proxy as current; stat err = %v", err)
	}
}

func TestDestroyCallsTeardown(t *testing.T) { wantTeardownVia(t, "destroy") }

// TestDownAliasCallsTeardown: `lever down` must keep working as a hidden alias
// of `destroy`.
func TestDownAliasCallsTeardown(t *testing.T) { wantTeardownVia(t, "down") }

func wantTeardownVia(t *testing.T, verb string) {
	t.Helper()
	sb := &stubBackend{}
	if _, err := clitest.Exec(t, stubRoot(sb), verb, "--machine", "lever-x"); err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	if !sb.down {
		t.Fatalf("`lever %s` must call Backend.Teardown", verb)
	}
}

// The records go with the tokens: a stale record would describe the wrong
// token. The remote web role record goes too, or the fresh hub would never
// get the role.
func TestRemovePATsClearsTokensAndRecords(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "c")
	seedPAT(t, st, "remote", "r")
	if err := st.SaveRemoteRoleRecord(state.RemoteRoleRecord{RoleID: "role"}); err != nil {
		t.Fatal(err)
	}
	if err := removePATs(st); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{st.ControllerPAT(), st.ControllerPATRecord(), st.RemotePAT(), st.RemotePATRecord(), st.RemoteRole()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be removed, stat err = %v", p, err)
		}
	}
	if err := removePATs(st); err != nil {
		t.Fatalf("a second removal must be a no-op, got %v", err)
	}
}

// One failed removal must not shield the others: a remote.pat left behind is
// reused and rejected by the fresh hub. Every path is attempted.
func TestRemovePATsAttemptsEveryFileOnFailure(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "c")
	seedPAT(t, st, "remote", "r")
	if err := os.Remove(st.ControllerPAT()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(st.ControllerPAT(), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := removePATs(st)
	if err == nil || !strings.Contains(err.Error(), "controller.pat") {
		t.Fatalf("want an error naming controller.pat, got %v", err)
	}
	for _, p := range []string{st.ControllerPATRecord(), st.RemotePAT(), st.RemotePATRecord()} {
		if _, serr := os.Stat(p); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("%s should still be removed, stat err = %v", p, serr)
		}
	}
}
