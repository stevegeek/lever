// Package backendtest holds the proc.Runner doubles and fixtures the backend
// packages' tests share (orbstack, lima, guest). Test-only consumers; nothing
// in production imports it.
package backendtest

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
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
	*proc.FakeRunner
	Host              string
	Open              bool
	Flushed, Resolved bool
}

// ClosedChain is the `iptables -S LEVER_EGRESS` output of a live closed
// posture: loopback accepted, one allowlisted port to the alias, the alias
// dropped, everything else dropped.
const ClosedChain = "-N LEVER_EGRESS\n-A LEVER_EGRESS -o lo -j ACCEPT\n-A LEVER_EGRESS -d 0.250.250.254/32 -p tcp -m tcp --dport 8443 -j ACCEPT\n-A LEVER_EGRESS -d 0.250.250.254/32 -j DROP\n-A LEVER_EGRESS -j DROP\n"

func (r *ClosedChainRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv := strings.Join(args, " ")
	if name == r.Host {
		switch {
		case strings.Contains(argv, "iptables -S LEVER_EGRESS"):
			if !r.Open {
				return proc.Result{Stdout: ClosedChain}, nil
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
func (r *ClosedChainRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
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

// Guest names the argv prefixes one backend uses to reach its guest, so the
// tests of that backend script the shared probes (run user, runtimes, scion
// install, egress) once instead of restating the argv in every fixture. A
// command change in the backend is then one edit here.
type Guest struct {
	Machine string // machine/VM name, e.g. "lever-jail"
	User    string // run-user transport prefix, e.g. "orb -m lever-jail"
	Root    string // root transport prefix, e.g. "orb -u root -m lever-jail"
	Alias   string // host alias the guest resolves, e.g. "host.orb.internal"
}

// Guest `getent ahosts <alias>` outputs: both address families, or v4 only.
const (
	AhostsDual = "0.250.250.254 STREAM \nfd07::fe STREAM \n"
	AhostsV4   = "0.250.250.254 STREAM \n"
)

// HostAliasV4 is the v4 address AhostsDual/AhostsV4 and ClosedChain carry.
const HostAliasV4 = "0.250.250.254"

// ScriptRunUser scripts the two probes common.Base.ReadRunUser issues through
// the run-user prefix, so a test can resolve the run user without EnsureUp.
func (g Guest) ScriptRunUser(f *proc.FakeRunner, user, uid string) {
	f.Script(g.User+" whoami", proc.Result{Stdout: user + "\n"})
	f.Script(g.User+" id -u", proc.Result{Stdout: uid + "\n"})
}

// ScriptRuntimes scripts the guest-side runtime install scripts (the user and
// root `bash` calls) as succeeding.
func (g Guest) ScriptRuntimes(f *proc.FakeRunner) {
	f.Script(g.User+" bash", proc.Result{Stdout: "ok\n"})
	f.Script(g.Root+" bash", proc.Result{Stdout: "ok\n"})
}

// ScriptArch scripts the guest arch probe (`uname -m`).
func (g Guest) ScriptArch(f *proc.FakeRunner, arch string) {
	f.Script(g.User+" uname -m", proc.Result{Stdout: arch + "\n"})
}

// ScriptEgress scripts what ApplyEgress needs: the alias resolve (answered
// with ahosts, usually AhostsDual or AhostsV4) and the root iptables/ip6tables
// calls.
func (g Guest) ScriptEgress(f *proc.FakeRunner, ahosts string) {
	f.Script(g.User+" getent ahosts "+g.Alias, proc.Result{Stdout: ahosts})
	g.ScriptFirewall(f)
}

// ScriptFirewall scripts only the root iptables/ip6tables calls, for a runner
// that answers the resolve itself (ClosedChainRunner) or a test that scripts
// a specific ahosts answer.
func (g Guest) ScriptFirewall(f *proc.FakeRunner) {
	f.Script(g.Root+" iptables", proc.Result{})
	f.Script(g.Root+" ip6tables", proc.Result{})
}

// ScriptProvision scripts everything EnsureUp does AFTER the machine is up:
// run user (leveruser/uid), runtimes, arch probe and egress. Machine lifecycle
// (version preflight, list, create, start) stays with the backend's own
// fixture because its argv is backend-specific.
func (g Guest) ScriptProvision(f *proc.FakeRunner, uid, ahosts string) {
	g.ScriptRunUser(f, "leveruser", uid)
	g.ScriptRuntimes(f)
	g.ScriptArch(f, "arm64")
	g.ScriptEgress(f, ahosts)
}

// ScriptScionInstall scripts the host build + guest install of the scion
// binary so a test can reach whatever runs AFTER it.
func (g Guest) ScriptScionInstall(t *testing.T, f *proc.FakeRunner) {
	t.Helper()
	f.Script("go build", proc.Result{})
	// A digest mismatch, so the install streams rather than skipping.
	f.Script(g.User+" /usr/bin/sha256sum", proc.Result{Code: 1})
	StageFakeBuildOutput(t, g.Machine)
}

// ClosedChain returns a ClosedChainRunner for this guest's host binary with
// the firewall calls scripted; ahosts answers the resolve while Open.
func (g Guest) ClosedChain(ahosts string, open bool) *ClosedChainRunner {
	r := &ClosedChainRunner{FakeRunner: proc.NewFakeRunner(), Host: hostBinary(g.User), Open: open}
	g.ScriptEgress(r.FakeRunner, ahosts)
	return r
}

func hostBinary(prefix string) string {
	if i := strings.IndexByte(prefix, ' '); i >= 0 {
		return prefix[:i]
	}
	return prefix
}

// AssertNoSubcommand fails when any recorded call is `host sub ...` — the
// negative half of "a Running machine is not created/started again".
func AssertNoSubcommand(t *testing.T, f *proc.FakeRunner, host string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if i := f.CallIndex(proc.Subcommand(host, sub)); i >= 0 {
			t.Fatalf("`%s %s` must not be called here: %+v", host, sub, f.Calls[i])
		}
	}
}

// AssertEgressRules checks the iptables calls ApplyEgress issued: an ACCEPT
// for port and the catch-all DROP to HostAliasV4.
func AssertEgressRules(t *testing.T, f *proc.FakeRunner, port string) {
	t.Helper()
	sawAccept := f.Called(proc.ArgvContains("iptables", "--dport "+port, "ACCEPT"))
	sawDrop := f.Called(proc.ArgvContains("iptables", HostAliasV4+" -j DROP"))
	if !sawAccept || !sawDrop {
		t.Fatalf("accept=%t drop=%t", sawAccept, sawDrop)
	}
}

// AssertFlushPrecedesResolve pins the order ApplyEgress must keep: flush the
// chain BEFORE resolving the alias. Under a prior closed posture the catch-all
// DROP blocks DNS/53; flushing first restores it so the re-resolve succeeds.
func AssertFlushPrecedesResolve(t *testing.T, f *proc.FakeRunner, alias string) {
	t.Helper()
	flushIdx := f.CallIndex(proc.ArgvContains("iptables -F LEVER_EGRESS"))
	getentIdx := f.CallIndex(proc.ArgvContains("getent ahosts " + alias))
	if flushIdx < 0 {
		t.Fatal("ApplyEgress must flush LEVER_EGRESS (idempotent re-apply, no rule accumulation)")
	}
	if getentIdx < 0 || flushIdx > getentIdx {
		t.Fatalf("flush (idx %d) must precede the host-alias resolve (idx %d)", flushIdx, getentIdx)
	}
}

// AssertNoNodeTooling fails when the host invoked node or npm: an instance
// that serves no UI must not need them.
func AssertNoNodeTooling(t *testing.T, f *proc.FakeRunner) {
	t.Helper()
	for _, c := range f.Calls {
		if c.Name == "npm" || c.Name == "node" {
			t.Fatalf("an instance that serves no UI must not need node: %v %v", c.Name, c.Args)
		}
	}
}

// ELF header constants for WriteELF64 (debug/elf's EM_*/ET_* values).
const (
	EMX8664   uint16 = 62  // elf.EM_X86_64
	EMAArch64 uint16 = 183 // elf.EM_AARCH64
	ETExec    uint16 = 2
	ETDyn     uint16 = 3
)

// WriteELF64 writes a minimal but structurally valid 64-bit little-endian ELF
// header for the given machine and object type to dir/scion and returns its
// path. Program and section header counts are zero, so debug/elf parses the
// header and stops — enough to exercise scionbin.VerifyELFArch (and the
// binary-mode install flows built on it) without a real binary as test data.
func WriteELF64(t *testing.T, dir string, machine uint16, etype uint16) string {
	t.Helper()
	h := make([]byte, 64)
	copy(h, []byte{0x7f, 'E', 'L', 'F'})
	h[4] = 2 // EI_CLASS: 64-bit
	h[5] = 1 // EI_DATA: little-endian
	h[6] = 1 // EI_VERSION
	binary.LittleEndian.PutUint16(h[16:], etype)
	binary.LittleEndian.PutUint16(h[18:], machine)
	binary.LittleEndian.PutUint32(h[20:], 1)  // e_version
	binary.LittleEndian.PutUint16(h[52:], 64) // e_ehsize
	path := filepath.Join(dir, "scion")
	if err := os.WriteFile(path, h, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// AssertScionBuild finds the host `go build` of ./cmd/scion and checks it ran
// in src, cross-compiled for linux/arm64, and wrote the per-machine output
// path. It returns false (after recording errors) when no build ran.
func AssertScionBuild(t *testing.T, f *proc.FakeRunner, src, machine string) bool {
	t.Helper()
	i := f.CallIndex(proc.Subcommand("go", "build"))
	if i < 0 {
		t.Errorf("expected go build for ./cmd/scion in %q; calls=%+v", src, f.Calls)
		return false
	}
	c := f.Calls[i]
	if c.Dir != src {
		t.Errorf("build Dir: want %q got %q", src, c.Dir)
	}
	if c.Env["GOOS"] != "linux" || c.Env["GOARCH"] != "arm64" {
		t.Errorf("build env: want linux/arm64 got %+v", c.Env)
	}
	var sawCmd bool
	var binArg string
	for j, a := range c.Args {
		if a == "./cmd/scion" {
			sawCmd = true
		}
		if a == "-o" && j+1 < len(c.Args) {
			binArg = c.Args[j+1]
		}
	}
	if !sawCmd {
		t.Errorf("build args should contain ./cmd/scion; got %+v", c.Args)
	}
	if !strings.Contains(binArg, "lever-scion-"+machine) {
		t.Errorf("build output path should include per-machine name lever-scion-%s; got %q", machine, binArg)
	}
	return true
}
