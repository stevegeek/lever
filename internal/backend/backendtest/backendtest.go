// Package backendtest holds the exec.Runner doubles and fixtures the backend
// packages' tests share (orbstack, lima, guest). Test-only consumers; nothing
// in production imports it.
package backendtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

// ClosedChainRunner answers `iptables -S LEVER_EGRESS` through Host (the
// prefix binary, "orb" or "limactl") with an ACTIVE closed chain, and records
// whether the chain was flushed or the host alias re-resolved. It intercepts
// those substrings in a fixed switch order BEFORE falling through to the
// embedded FakeRunner, so results are deterministic — FakeRunner.Script matches
// by HasPrefix over its (randomized-iteration-order) map, so two overlapping
// keys like "...iptables -S LEVER_EGRESS" and the shorter generic "...iptables"
// are both valid prefixes of the same call, and which one "wins" is
// nondeterministic.
//
// With Open set the chain probe falls through to the FakeRunner instead (no
// active chain), so a test can drive one full rebuild before flipping to the
// closed posture.
type ClosedChainRunner struct {
	*exec.FakeRunner
	Host              string
	Open              bool
	Flushed, Resolved bool
}

// ClosedChain is the `iptables -S LEVER_EGRESS` output of a live closed
// posture: loopback accepted, one allowlisted port to the alias, the alias
// dropped, everything else dropped.
const ClosedChain = "-N LEVER_EGRESS\n-A LEVER_EGRESS -o lo -j ACCEPT\n-A LEVER_EGRESS -d 0.250.250.254/32 -p tcp -m tcp --dport 8443 -j ACCEPT\n-A LEVER_EGRESS -d 0.250.250.254/32 -j DROP\n-A LEVER_EGRESS -j DROP\n"

func (r *ClosedChainRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	argv := strings.Join(args, " ")
	if name == r.Host {
		switch {
		case strings.Contains(argv, "iptables -S LEVER_EGRESS"):
			if !r.Open {
				return exec.Result{Stdout: ClosedChain}, nil
			}
		case strings.Contains(argv, "-F LEVER_EGRESS"):
			r.Flushed = true
		case strings.Contains(argv, "getent ahosts"):
			r.Resolved = true
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

// Run must be re-declared, not inherited: the embedded FakeRunner's Run calls
// ITS OWN RunIn, so a caller using Run would bypass the interception above.
func (r *ClosedChainRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// FakeScionCheckout writes the minimum of a scion checkout that the web-asset
// path inspects: a web/ holding package.json.
func FakeScionCheckout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// StageFakeBuildOutput creates the file a faked `go build` would have written,
// at the exact path scionbin.Resolve passes to `-o`. The install path
// hashes that file for real, so it has to exist even when the build is a stub.
func StageFakeBuildOutput(t *testing.T, machine string) {
	t.Helper()
	p := filepath.Join(os.TempDir(), "lever-scion-"+machine)
	if err := os.WriteFile(p, []byte("fake-scion-"+machine), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
}

// ScriptScionInstall scripts the host build + guest install of the scion
// binary so a test can reach whatever runs AFTER it. userPrefix is the guest's
// run-user prefix joined by spaces (e.g. "orb -m lever-jail").
func ScriptScionInstall(t *testing.T, f *exec.FakeRunner, userPrefix, machine string) {
	t.Helper()
	f.Script("go build", exec.Result{})
	// A digest mismatch, so the install streams rather than skipping.
	f.Script(userPrefix+" /usr/bin/sha256sum", exec.Result{Code: 1})
	StageFakeBuildOutput(t, machine)
}

// ScriptRunUser scripts the two probes common.Base.ReadRunUser issues through
// userPrefix (e.g. "orb -m lever-jail"), so a test can resolve the run user
// without EnsureUp.
func ScriptRunUser(f *exec.FakeRunner, userPrefix, user, uid string) {
	f.Script(userPrefix+" whoami", exec.Result{Stdout: user + "\n"})
	f.Script(userPrefix+" id -u", exec.Result{Stdout: uid + "\n"})
}
