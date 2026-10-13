// Package backendtest holds the proc.Runner doubles and fixtures the backend
// packages' tests share. Nothing in production imports it.
package backendtest

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
)

// ClosedChainRunner answers `iptables -S LEVER_EGRESS` through Host (the
// prefix binary) with an ACTIVE closed chain, and records whether the chain
// was flushed or the host alias re-resolved. It intercepts before the
// embedded FakeRunner because FakeRunner.Script matches by prefix over a map,
// so with overlapping keys like "...iptables" the winner is nondeterministic.
//
// With Open set the chain probe falls through to the FakeRunner instead (no
// active chain), so a test can drive one full rebuild before flipping to the
// closed posture.
type ClosedChainRunner struct {
	*proc.FakeRunner
	Host string
	Open bool
	// V6Open answers the ip6tables probe with no chain even while the v4
	// chain is closed: a closed apply interrupted between its two commits.
	V6Open            bool
	Flushed, Resolved bool
}

// ClosedChain6 is the `ip6tables -S LEVER_EGRESS` output of the same closed
// posture: exactly what the closed ruleset for HostAliasV6 and port 8443
// lists, as the re-apply skip requires (guest.existingClosedAlias).
var ClosedChain6 = closedListing(egress.IPv6)

// ClosedChain is the `iptables -S LEVER_EGRESS` output of a live closed
// posture for HostAliasV4 and port 8443.
var ClosedChain = closedListing(egress.IPv4)

// JumpListing is `iptables -S OUTPUT` (or ip6tables) of a converged guest:
// the jump to the lever chain is OUTPUT's first and only rule.
const JumpListing = "-P OUTPUT ACCEPT\n" + egress.JumpRule + "\n"

// HostAliasV6 is the IPv6 alias the closed fixtures name.
const HostAliasV6 = "fd07::fe"

func closedListing(f egress.Family) string {
	rules := egress.BuildRulesDNS(HostAliasV4, HostAliasV6, []int{8443}, true, nil)
	return strings.Join(egress.Listing(rules, f), "\n") + "\n"
}

func (r *ClosedChainRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	argv := strings.Join(args, " ")
	if name == r.Host {
		switch {
		case strings.Contains(argv, "ip6tables -S LEVER_EGRESS"):
			if !r.Open && !r.V6Open {
				return proc.Result{Stdout: ClosedChain6}, nil
			}
		case strings.Contains(argv, "iptables -S LEVER_EGRESS"):
			if !r.Open {
				return proc.Result{Stdout: ClosedChain}, nil
			}
		case strings.Contains(argv, "tables -S OUTPUT"):
			// A live closed posture: the jump is OUTPUT's first rule.
			if !r.Open {
				return proc.Result{Stdout: JumpListing}, nil
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

// NATChainRunner answers `iptables -t nat -S <Chain>` through Host with Out
// and records that the read happened. It intercepts for the same reason as
// ClosedChainRunner.
type NATChainRunner struct {
	*proc.FakeRunner
	Host  string
	Chain string
	Out   string
	Read  bool
}

// LimaDNSChain is the `iptables -t nat -S LIMADNS` output of a booted Lima
// guest with the host resolver on: the guest resolver (192.168.5.3:53) DNATed
// to the host alias on two per-boot ports. HostAliasV4 stands in for
// 192.168.5.2 so the fixtures' ahosts answers line up.
const LimaDNSChain = "-N LIMADNS\n" +
	"-A LIMADNS -d 192.168.5.3/32 -p udp -m udp --dport 53 -j DNAT --to-destination " + HostAliasV4 + ":41234\n" +
	"-A LIMADNS -d 192.168.5.3/32 -p tcp -m tcp --dport 53 -j DNAT --to-destination " + HostAliasV4 + ":41235\n"

func (r *NATChainRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	if name == r.Host && strings.Contains(strings.Join(args, " "), "iptables -t nat -S "+r.Chain) {
		r.Read = true
		// Recorded like any other call, so argv assertions can see the read.
		r.Calls = append(r.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		return proc.Result{Stdout: r.Out}, nil
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

// Run is re-declared for the same reason as ClosedChainRunner.Run.
func (r *NATChainRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
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

// IsolateCache points os.UserCacheDir at a fresh per-test directory — HOME
// for the darwin resolution, XDG_CACHE_HOME for the Linux one — so no test
// touches the operator's real cache. It uses t.Setenv, so the calling test
// must not be parallel. It returns the new HOME.
func IsolateCache(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	return home
}

// StageFakeBuildOutput creates the file a faked `go build` would have written,
// at the exact path scionbin.Resolve passes to `-o`. The install path
// hashes that file for real, so it has to exist even when the build is a stub.
// The cache is isolated first (IsolateCache): several packages stage the
// same machine name, and their test processes run in parallel, so the real
// per-user cache would be one shared, racy path that outlives the tests.
func StageFakeBuildOutput(t *testing.T, machine string) {
	t.Helper()
	IsolateCache(t)
	p, err := scionbin.OutputPath(machine)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("fake-scion-"+machine), 0o755); err != nil {
		t.Fatal(err)
	}
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

// ScriptRuntimes scripts the guest-side runtime install scripts as succeeding.
func (g Guest) ScriptRuntimes(f *proc.FakeRunner) {
	f.Script(g.User+" bash", proc.Result{Stdout: "ok\n"})
	f.Script(g.Root+" bash", proc.Result{Stdout: "ok\n"})
}

func (g Guest) ScriptArch(f *proc.FakeRunner, arch string) {
	f.Script(g.User+" uname -m", proc.Result{Stdout: arch + "\n"})
}

// ScriptEgress scripts what ApplyEgress needs, answering the alias resolve
// with ahosts.
func (g Guest) ScriptEgress(f *proc.FakeRunner, ahosts string) {
	f.Script(g.User+" getent ahosts "+g.Alias, proc.Result{Stdout: ahosts})
	g.scriptFirewall(f)
}

func (g Guest) scriptFirewall(f *proc.FakeRunner) {
	f.Script(g.Root+" iptables", proc.Result{})
	f.Script(g.Root+" ip6tables", proc.Result{})
	// A converged OUTPUT, so no jump repair is made.
	f.Script(g.Root+" iptables -S OUTPUT", proc.Result{Stdout: JumpListing})
	f.Script(g.Root+" ip6tables -S OUTPUT", proc.Result{Stdout: JumpListing})
	// The atomic commit (guest.commitEgressChain).
	f.Script(g.Root+" bash -c exec ip", proc.Result{})
}

// RestoreCommits returns the stdin of every chain commit that declares
// LEVER_EGRESS, in order, keyed by the restore binary.
func RestoreCommits(f *proc.FakeRunner) (bins []string, inputs []string) {
	return restoreCalls(f, true)
}

// JumpCommits returns the restore commits that only (re)place the OUTPUT
// jump, in order, keyed by the restore binary.
func JumpCommits(f *proc.FakeRunner) (bins []string, inputs []string) {
	return restoreCalls(f, false)
}

const chainDecl = "\n:" + egress.Chain + " "

func restoreCalls(f *proc.FakeRunner, chain bool) (bins []string, inputs []string) {
	for _, c := range f.Calls {
		argv := c.Argv()
		for _, bin := range []string{"ip6tables-restore", "iptables-restore"} {
			if strings.Contains(argv, "exec "+bin+" --noflush") && strings.Contains(c.Stdin, chainDecl) == chain {
				bins, inputs = append(bins, bin), append(inputs, c.Stdin)
				break
			}
		}
	}
	return bins, inputs
}

func firstChainCommit(f *proc.FakeRunner) int {
	return f.CallIndex(func(c proc.Call) bool {
		return strings.Contains(c.Argv(), "exec ip6tables-restore --noflush") && strings.Contains(c.Stdin, chainDecl)
	})
}

// ScriptProvision scripts everything EnsureUp does AFTER the machine is up.
// Machine lifecycle stays with the backend's own fixture.
func (g Guest) ScriptProvision(f *proc.FakeRunner, uid, ahosts string) {
	g.ScriptRunUser(f, "leveruser", uid)
	g.ScriptRuntimes(f)
	g.ScriptArch(f, "arm64")
	g.ScriptEgress(f, ahosts)
}

// ScriptScionInstall scripts the host build and guest install of the scion
// binary.
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

// AssertNoSubcommand fails when any recorded call is `host sub ...`.
func AssertNoSubcommand(t *testing.T, f *proc.FakeRunner, host string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if i := f.CallIndex(proc.Subcommand(host, sub)); i >= 0 {
			t.Fatalf("`%s %s` must not be called here: %+v", host, sub, f.Calls[i])
		}
	}
}

// AssertEgressRules checks the committed IPv4 ruleset has an ACCEPT for port
// and the DROP to HostAliasV4.
func AssertEgressRules(t *testing.T, f *proc.FakeRunner, port string) {
	t.Helper()
	bins, inputs := RestoreCommits(f)
	v4 := ""
	for i, b := range bins {
		if b == "iptables-restore" {
			v4 = inputs[i]
		}
	}
	sawAccept := strings.Contains(v4, "--dport "+port+" -j ACCEPT")
	sawDrop := strings.Contains(v4, "-d "+HostAliasV4+" -j DROP")
	if !sawAccept || !sawDrop {
		t.Fatalf("accept=%t drop=%t in the committed ruleset:\n%s", sawAccept, sawDrop, v4)
	}
}

// AssertAtomicCommitOrder requires exactly two commits, IPv6 then IPv4.
func AssertAtomicCommitOrder(t *testing.T, f *proc.FakeRunner) {
	t.Helper()
	if bins, _ := RestoreCommits(f); len(bins) != 2 || bins[0] != "ip6tables-restore" || bins[1] != "iptables-restore" {
		t.Fatalf("want one ip6tables-restore then one iptables-restore commit, got %v", bins)
	}
}

// AssertAtomicCommit pins how ApplyEgress replaces the chain: never a flush
// (that would open egress), the alias resolved first under the live chain,
// then ONE chain-replacing restore commit per family.
func AssertAtomicCommit(t *testing.T, f *proc.FakeRunner, alias string) {
	t.Helper()
	if i := f.CallIndex(proc.ArgvContains("-F LEVER_EGRESS")); i >= 0 {
		t.Fatalf("ApplyEgress must never flush LEVER_EGRESS (that opens egress until the rules are back): %+v", f.Calls[i])
	}
	getentIdx := f.CallIndex(proc.ArgvContains("getent ahosts " + alias))
	commitIdx := firstChainCommit(f)
	if getentIdx < 0 || commitIdx < 0 || getentIdx > commitIdx {
		t.Fatalf("resolve (idx %d) must precede the commit (idx %d)", getentIdx, commitIdx)
	}
	bins, inputs := RestoreCommits(f)
	// IPv6 first, IPv4 last: the v4 closed marker the I2 skip reads must
	// only ever land after v6 is final.
	if len(bins) != 2 || bins[0] != "ip6tables-restore" || bins[1] != "iptables-restore" {
		t.Fatalf("want one ip6tables-restore then one iptables-restore commit, got %v", bins)
	}
	for i, in := range inputs {
		if !strings.HasPrefix(in, "*filter\n:LEVER_EGRESS - [0:0]\n") || !strings.HasSuffix(in, "COMMIT\n") {
			t.Fatalf("%s input is not a single chain-replacing commit:\n%s", bins[i], in)
		}
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
// path: enough for scionbin.VerifyELFArch without a real binary.
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

// AssertLastSubcommand fails unless the last recorded call is `host sub ...`.
func AssertLastSubcommand(t *testing.T, f *proc.FakeRunner, host, sub string) {
	t.Helper()
	if len(f.Calls) == 0 || !f.Calls[len(f.Calls)-1].Subcommand(host, sub) {
		t.Fatalf("expected last call `%s %s`; got %+v", host, sub, f.Calls)
	}
}

// AssertClosedChainKept pins the I2 invariant after a re-apply against an
// already-closed chain: neither flushed nor re-resolved (either would briefly
// open egress for a running agent), and the alias read from the live chain.
func AssertClosedChainKept(t *testing.T, r *ClosedChainRunner, gotV4 string) {
	t.Helper()
	if r.Flushed {
		t.Fatal("must not flush LEVER_EGRESS when the closed posture is already active (would open egress)")
	}
	if r.Resolved {
		t.Fatal("must not re-resolve the alias (DNS) when already closed — read it from the chain")
	}
	if bins, _ := RestoreCommits(r.FakeRunner); len(bins) != 0 {
		t.Fatalf("must not commit a new ruleset over an active closed chain, committed %v", bins)
	}
	if gotV4 != HostAliasV4 {
		t.Fatalf("alias should be read from the existing chain, got %q", gotV4)
	}
}

// VersionCase is one row of RunVersionCases.
type VersionCase struct {
	Name    string
	Stdout  string
	WantOK  bool
	WantErr bool
	WantGot string
}

// RunVersionCases drives common.VersionAtLeast-style probes: check scripts
// stdout on a fresh runner and returns the probe's (ok, got, err).
func RunVersionCases(t *testing.T, cases []VersionCase, check func(f *proc.FakeRunner, stdout string) (bool, string, error)) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			ok, got, err := check(proc.NewFakeRunner(), tc.Stdout)
			if tc.WantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (ok=%t got=%q)", ok, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.WantOK {
				t.Errorf("ok: want %t got %t", tc.WantOK, ok)
			}
			if got != tc.WantGot {
				t.Errorf("got version string: want %q got %q", tc.WantGot, got)
			}
		})
	}
}

// ProbeItem is one answer of the warm-path probe (guest.Probe): a command's
// stdout and exit code.
type ProbeItem struct {
	Out  string
	Code int
}

// ProbeRunner answers the warm-path probe through Host from Items, keyed by
// the section names the script emits; a section with no item answers exit 1
// and no output. It records each probe and each provisioning-stamp write.
type ProbeRunner struct {
	*proc.FakeRunner
	Host   string
	Items  map[string]ProbeItem
	Probes int
	Stamps []string // the key of each stamp write
	// OnProbe, when set, runs before probe n (1-based) is answered, so a
	// test can change Items between the probe and a later re-read.
	OnProbe func(n int, items map[string]ProbeItem)
}

var emitRe = regexp.MustCompile(`(?m)^emit ([a-z0-9]+) `)

func (r *ProbeRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	n := len(args)
	if name == r.Host && n >= 4 && args[n-2] == "lever-probe" && args[n-4] == "-c" {
		r.Probes++
		if r.OnProbe != nil {
			r.OnProbe(r.Probes, r.Items)
		}
		r.Calls = append(r.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		var b strings.Builder
		for _, m := range emitRe.FindAllStringSubmatch(args[n-3], -1) {
			it, ok := r.Items[m[1]]
			if !ok {
				it = ProbeItem{Code: 1}
			}
			fmt.Fprintf(&b, "@@%s %s %d\n%s\n", args[n-1], m[1], it.Code, strings.TrimRight(it.Out, "\n"))
		}
		return proc.Result{Stdout: b.String()}, nil
	}
	if name == r.Host && n >= 2 && args[n-2] == "lever-stamp" {
		r.Stamps = append(r.Stamps, args[n-1])
		r.Calls = append(r.Calls, proc.Call{Name: name, Args: args, Env: env, Dir: dir})
		return proc.Result{}, nil
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

// Run is re-declared for the same reason as ClosedChainRunner.Run.
func (r *ProbeRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// PastaDropIn mirrors guest.pastaDropInModern, which this package cannot
// import; TestWarmFixturePastaDropIn in guest pins the two together.
const PastaDropIn = `[containers]
host_containers_internal_ip = "169.254.1.2"

[network]
default_rootless_network_cmd = "pasta"
pasta_options = ["--map-host-loopback", "169.254.1.2"]
`

// ProbeBoot is the boot key a WarmItems guest reports.
const ProbeBoot = "4c1d7a64-0b1e-4c55-9d2e-d0e1f2a3b4c5\n42\n"

// WarmItems is the probe of a converged guest for an open-posture, nested
// virt off EnsureUp with allowedPorts, provisioned in this boot with stampKey
// (guest.ProvisionStampKey("leveruser")). forwards are the DNS targets the
// chain already accepts; nat, when not empty, is the listing they come from.
func WarmItems(stampKey string, allowedPorts []int, forwards []egress.DNSForward, nat string) map[string]ProbeItem {
	rules := egress.BuildRulesDNS(HostAliasV4, HostAliasV6, allowedPorts, false, forwards)
	jump := JumpListing
	items := map[string]ProbeItem{
		"user":      {Out: "leveruser\n"},
		"uid":       {Out: "501\n"},
		"arch":      {Out: "aarch64\n"},
		"ahosts":    {Out: AhostsDual},
		"boot":      {Out: ProbeBoot},
		"pasta":     {Out: PastaDropIn},
		"pastahelp": {Out: "Usage: pasta [OPTION]... [COMMAND] [ARGS]...\n  --map-host-loopback ADDR\n"},
		"stamp":     {Out: stampKey + " 4c1d7a64-0b1e-4c55-9d2e-d0e1f2a3b4c5:42\n"},
		"out4":      {Out: jump},
		"out6":      {Out: jump},
		"chain4":    {Out: strings.Join(egress.Listing(rules, egress.IPv4), "\n") + "\n"},
		"chain6":    {Out: strings.Join(egress.Listing(rules, egress.IPv6), "\n") + "\n"},
	}
	if nat != "" {
		items["dnschain"] = ProbeItem{Out: nat}
	}
	return items
}
