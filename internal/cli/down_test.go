package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
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
	// The now-empty .lever dir should be gone too.
	if _, err := os.Stat(filepath.Join(tree, ".lever")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty .lever dir should be removed, stat err = %v", err)
	}
}

func TestClearStagedRuntimeStateMissingIsNoop(t *testing.T) {
	// Nothing staged: must not panic or error.
	clearStagedRuntimeState(&config.App{Tree: t.TempDir()})
}

// TestRemoveControllerPAT: destroy must clear the persisted controller PAT so a
// later `up` mints a fresh one — the old PAT is signed against the hub DB that
// died with the machine, and reusing it fails the new hub's readiness auth.
func TestRemoveControllerPAT(t *testing.T) {
	st := brokerctl.StateDir(t.TempDir())
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
	// No PAT staged: destroy must not error.
	if err := removeIfPresent(brokerctl.StateDir(t.TempDir()).ControllerPAT()); err != nil {
		t.Fatalf("missing PAT must be a no-op, got %v", err)
	}
}

// TestRemoveRemotePAT: destroy must ALSO clear the persisted remote-access
// PAT, for the same reason as the controller PAT (TestRemoveControllerPAT) —
// it is minted against the same jail hub DB that dies with the machine.
// Left behind, ensureControllerPAT's needRemote check sees a non-empty
// remote.pat after a fresh `up` and skips the re-mint, so the remote proxy
// would inject a token the new hub's DB has never heard of.
func TestRemoveRemotePAT(t *testing.T) {
	st := brokerctl.StateDir(t.TempDir())
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
	// No remote PAT staged (remote never enabled): destroy must not error.
	if err := removeIfPresent(brokerctl.StateDir(t.TempDir()).RemotePAT()); err != nil {
		t.Fatalf("missing remote PAT must be a no-op, got %v", err)
	}
}

// TestDestroyAlsoStopsRemoteProxy pins the half of "full teardown" that was
// missing: the remote-access proxy is a host-side daemon exactly like the
// broker, and `destroy` stopped the broker only.
//
// A surviving proxy is not merely untidy. It keeps two loopback listeners and
// the operator's `tailscale serve` front end pointed at a machine that no
// longer exists, and — because nothing removed its stamp either — the next
// `lever up` finds pid alive, port listening and stamp matching, and REUSES
// that process: one whose cached jail prefix names the destroyed machine, so
// every request fails with no lever verb that repairs it. `lever stop` has
// always done this (TestStopAlsoStopsRemoteProxy); destroy must too.
func TestDestroyAlsoStopsRemoteProxy(t *testing.T) {
	dir := instanceDir(t, "demo")
	t.Chdir(dir)

	state := brokerctl.StateDir(dir)
	if err := os.MkdirAll(state.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A real child of this test, not the test's own pid: destroy kills
	// whatever remote.pid names.
	proxy := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Process.Kill(); _, _ = proxy.Process.Wait() })
	if err := os.WriteFile(state.RemotePID(), []byte(strconv.Itoa(proxy.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Written as bytes rather than through WriteRemoteStamp: what matters here
	// is only that destroy REMOVES it, whatever it holds.
	if err := os.WriteFile(state.RemoteStamp(), []byte("v0.17.0 deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := &stubBackend{}
	root := newHostRootWith(func(string, string) (backend.Backend, error) { return sb, nil })
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

	// Reaped with Wait, not signal(0): a killed-but-unreaped child is a
	// ZOMBIE, and signal 0 succeeds on a zombie — so the obvious liveness
	// check passes whether or not the kill landed.
	done := make(chan struct{})
	go func() { _, _ = proxy.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the remote proxy outlived `lever destroy`: it keeps serving a destroyed instance, and the next `up` would reuse it")
	}
	if _, err := os.Stat(state.RemotePID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote.pid should be removed by destroy, stat err = %v", err)
	}
	if _, err := os.Stat(state.RemoteStamp()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote.stamp should be removed by destroy — a surviving stamp is what makes a later `up` accept a stale proxy as current; stat err = %v", err)
	}
}

// TestDestroyCallsTeardown verifies the renamed command (Use: "destroy")
// still tears the jail down.
func TestDestroyCallsTeardown(t *testing.T) {
	sb := &stubBackend{}
	root := newHostRootWith(func(string, string) (backend.Backend, error) { return sb, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"destroy", "--machine", "lever-x"})
	if err := root.Execute(); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !sb.down {
		t.Fatal("destroy must call Backend.Teardown")
	}
}

// TestDownAliasCallsTeardown is the deprecation-safety regression: `lever
// down` must keep working, unchanged, as a hidden alias of `destroy`.
func TestDownAliasCallsTeardown(t *testing.T) {
	sb := &stubBackend{}
	root := newHostRootWith(func(string, string) (backend.Backend, error) { return sb, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"down", "--machine", "lever-x"})
	if err := root.Execute(); err != nil {
		t.Fatalf("down (alias): %v", err)
	}
	if !sb.down {
		t.Fatal("the `down` alias must still call Backend.Teardown")
	}
}
