package guest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/proc"
)

// The warm path (lever#187). A bring-up of a jail that is already provisioned
// used to make one guest round trip per check (whoami, id -u, the five
// runtime scripts, uname, sha256sum, four chain probes, the alias lookup, the
// resolver reads, two commits, two nested-virt scripts): on Lima each is a
// `limactl shell` of a quarter to half a second. Probe reads all of that in
// two guest calls, one as the run user and one as root, and the backend
// decides from the Snapshot what it can leave alone:
//
//   - the runtimes, by a root-owned provisioning stamp (ProvisionStampPath)
//     that names the exact scripts EnsureRuntimes runs and the boot they ran
//     in;
//   - the scion binary, by the digest of the installed file itself;
//   - the egress chains, by VERIFYING them: VerifyEgress compares both live
//     chains and the OUTPUT jumps against the exact ruleset ApplyEgress would
//     commit. The stamp never stands in for the firewall: a chain that is
//     missing, tampered with, half-applied or merely stale is rebuilt;
//   - nested virt, by the device mode and the udev rule.
//
// Whatever the probe cannot read, or reads in a form it does not expect,
// sends that part down the full path, which is what ran before.

// ProvisionStampPath is the root-owned record that EnsureRuntimes completed
// in this guest: "<ProvisionStampKey> <boot key>". The boot key makes it per
// boot, so a rebooted guest provisions once more (cheap next to the boot)
// and nothing a boot resets is ever assumed.
const ProvisionStampPath = "/var/lib/lever/provision.stamp"

// ProbeSpec says what Probe reads beyond the fixed items.
type ProbeSpec struct {
	// HostAlias is resolved with getent as the run user, as the backends'
	// ResolveHostAlias does. Empty skips the lookup (the closed posture,
	// whose DROP blocks DNS by design).
	HostAlias string
	// DNSChain is a nat-table chain to list (Lima's LIMADNS); empty skips.
	DNSChain string
	// ResolvConf is a resolver file to read; empty skips.
	ResolvConf string
	// HashPaths are files whose sha256 the root probe reports.
	HashPaths []string
}

// probeItem is one probed command: its stdout and its exit code.
type probeItem struct {
	out  string
	code int
}

// Snapshot is what Probe read. Every accessor reports whether its item was
// read and the command succeeded; a caller treats anything else as unknown.
type Snapshot struct {
	items map[string]probeItem
	spec  ProbeSpec
}

func (s *Snapshot) item(key string) (string, bool) {
	if s == nil {
		return "", false
	}
	it, ok := s.items[key]
	if !ok || it.code != 0 {
		return "", false
	}
	return it.out, true
}

// RunUser returns the guest run user and uid, as ReadRunUser reads them.
func (s *Snapshot) RunUser() (user, uid string, ok bool) {
	u, ok1 := s.item("user")
	id, ok2 := s.item("uid")
	u, id = strings.TrimSpace(u), strings.TrimSpace(id)
	if !ok1 || !ok2 || u == "" || id == "" {
		return "", "", false
	}
	return u, id, true
}

// GOARCH returns the guest arch as Guest.GOARCH reports it.
func (s *Snapshot) GOARCH() (string, bool) {
	u, ok := s.item("arch")
	if !ok {
		return "", false
	}
	a, err := goarchOf(u)
	return a, err == nil
}

// Digest returns the sha256 the root probe computed for path (one of
// ProbeSpec.HashPaths), or false when it could not.
func (s *Snapshot) Digest(path string) (string, bool) {
	if s == nil {
		return "", false
	}
	for i, p := range s.spec.HashPaths {
		if p == path {
			out, ok := s.item("hash" + strconv.Itoa(i))
			if f := strings.Fields(out); ok && len(f) > 0 && len(f[0]) == 64 {
				return f[0], true
			}
		}
	}
	return "", false
}

// bootKey identifies this boot of the guest: the kernel's boot id and the
// start time of pid 1, so a machine restarted on a kernel that kept running
// (an OrbStack machine shares the VM's) counts as a new boot too.
func (s *Snapshot) bootKey() (string, bool) {
	out, ok := s.item("boot")
	f := strings.Fields(out)
	if !ok || len(f) != 2 {
		return "", false
	}
	return f[0] + ":" + f[1], true
}

// Provisioned reports whether the provisioning stamp records key in this
// boot, and the run user's pasta drop-in is still the one the provisioning
// writes for this guest's pasta: that file belongs to the run user, so the
// stamp cannot vouch for it, and the full path rewrites it on every run.
func (s *Snapshot) Provisioned(key string) bool {
	boot, ok := s.bootKey()
	if !ok {
		return false
	}
	stamp, ok := s.item("stamp")
	return ok && strings.TrimSpace(stamp) == key+" "+boot && s.pastaConverged()
}

// pastaConverged reports whether the drop-in holds what pastaDropInScript
// would write for the pasta --help text the probe read.
func (s *Snapshot) pastaConverged() bool {
	got, ok := s.item("pasta")
	help, read := s.items["pastahelp"]
	if !ok || !read {
		return false
	}
	var want string
	switch {
	case strings.Contains(help.out, "--map-host-loopback"):
		want = pastaDropInModern
	case strings.Contains(help.out, "--no-map-gw"):
		want = pastaDropInGateway
	default:
		return false
	}
	return strings.TrimSpace(got) == strings.TrimSpace(want)
}

// NestedVirtConverged reports whether the guest already holds what
// EnsureNestedVirt(on) would leave: on, a character /dev/kvm of mode 0666
// and lever's udev rule; off, no rule and no /dev/kvm usable by everyone.
// Either way the legacy podman drop-in must be gone.
func (s *Snapshot) NestedVirtConverged(on bool) bool {
	if s == nil {
		return false
	}
	legacy, ok := s.items["kvmlegacy"]
	if !ok || legacy.code == 0 {
		return false // present, or unknown
	}
	isChar := s.items["kvmchar"]
	mode, modeOK := s.item("kvmmode")
	rule, ruleOK := s.item("udev")
	if on {
		return isChar.code == 0 && modeOK && strings.TrimSpace(mode) == "666" &&
			ruleOK && strings.TrimSpace(rule) == strings.TrimSpace(kvmUdevRule)
	}
	if _, read := s.items["udevexists"]; !read || s.items["udevexists"].code == 0 {
		return false // the rule is there, or unknown
	}
	if isChar.code != 0 {
		return true // no device at all
	}
	return modeOK && strings.TrimSpace(mode) == "660"
}

// DNSProbe is how a backend's DNSForward hook is computed from a Snapshot:
// the nat chain and resolver file the probe reads (ProbeSpec.DNSChain and
// ResolvConf) and the pure function the hook applies to them.
type DNSProbe struct {
	NATChain   string
	ResolvConf string
	// Forwards returns the targets from the nat listing (natOK: the listing
	// was read) and the resolver file (resolvOK: it was read).
	Forwards func(nat string, natOK bool, resolv string, resolvOK bool, aliasV4 string) []egress.DNSForward
}

// VerifyEgress reports whether the live egress state is exactly what
// ApplyEgress would leave for (allowedPorts, closedInternet), and if so the
// IPv4 alias the chains use. Exactly means: in both families OUTPUT's FIRST
// rule is the jump to LEVER_EGRESS and it jumps there nowhere else
// (egress.JumpFirst, the state ensureEgressChain leaves; a rule ahead of the
// jump would decide packets before the lever chain saw them), and each
// family's chain lists the very rules ApplyEgress would commit, in order,
// with nothing added (egress.ListingMatches).
//
// What it cannot see: it reads the chains as `iptables -S` prints them. On
// an nf_tables guest a rule added with nft itself, in a form iptables-nft
// cannot translate back, may print as something else or not at all, and
// would then not be noticed here. The full path repairs such a rule as a
// side effect (its `iptables-restore --noflush` re-declares the chain, which
// empties it, on every apply); the warm path skips that rewrite when the
// listing matches. Comparing `nft list chain` too would need a second
// rendering of the ruleset in nft's syntax, and nft is not always installed,
// so it is not done; writing nft rules into the guest takes root, which the
// trust note on Probe covers (security-model-jail.md says so too).
//
// The aliases come from where ApplyEgress takes them: in the open posture
// from the getent lookup the probe ran (and the resolver's forward targets
// from dns, when the backend has any; a backend with a DNSForward hook and no
// DNSProbe cannot be verified here and gets dns == nil with hasDNS true); in
// the closed posture from the live chains, as the closed re-apply always has
// (existingClosedAlias), since the closed chain's DROP blocks DNS by design.
// A closed chain read that way can only hold its alias's allowlisted ports
// open; it is compared rule for rule all the same.
//
// false sends the caller to the full ApplyEgress, which converges the chains.
func (s *Snapshot) VerifyEgress(allowedPorts []int, closedInternet bool, hasDNS bool, dns *DNSProbe) (string, bool) {
	out4, ok4 := s.item("out4")
	out6, ok6 := s.item("out6")
	if !ok4 || !ok6 || !egress.JumpFirst(out4) || !egress.JumpFirst(out6) {
		return "", false
	}
	chain4, ok4 := s.item("chain4")
	chain6, ok6 := s.item("chain6")
	if !ok4 || !ok6 {
		return "", false
	}
	var rules []egress.Rule
	var v4 string
	if closedInternet {
		if !closedChain(chain4) || !closedChain(chain6) {
			return "", false
		}
		v4 = aliasOfFamily(chain4, true)
		if v4 == "" {
			return "", false
		}
		rules = egress.BuildRulesDNS(v4, aliasOfFamily(chain6, false), allowedPorts, true, nil)
	} else {
		ah, ok := s.item("ahosts")
		if !ok {
			return "", false
		}
		var v6 string
		v4, v6 = ParseAhosts(ah)
		if v4 == "" && v6 == "" {
			return "", false
		}
		var forwards []egress.DNSForward
		if hasDNS && v4 != "" {
			if dns == nil || dns.Forwards == nil {
				return "", false
			}
			nat, natOK := s.item("dnschain")
			resolv, resolvOK := s.item("resolv")
			forwards = dns.Forwards(nat, natOK, resolv, resolvOK, v4)
		}
		rules = egress.BuildRulesDNS(v4, v6, allowedPorts, false, forwards)
	}
	if !egress.ListingMatches(rules, egress.IPv4, chain4) || !egress.ListingMatches(rules, egress.IPv6, chain6) {
		return "", false
	}
	return v4, true
}

// Probe reads the guest state the warm path decides on, in two guest calls:
// one as the run user, one as root. Every section of the output is framed by
// a line carrying a fresh random nonce per call, so nothing a root section
// prints (a file, a listing) can pose as another section, and a section the
// script did not ask for fails the probe. The user call runs in the run
// user's login shell, so the run user can shape everything it reports (as it
// could the separate whoami/getent calls before); nothing it reports is
// trusted on its own: an alias it gave must still match the root-read chains
// exactly, and its pasta drop-in is its own file. An error means the snapshot
// cannot be trusted as a whole; the caller then takes the full path.
//
// The root call is only as trustworthy as the RootPrefix it runs through.
// On OrbStack that is `orb -u root`, which the run user has no part in. On
// Lima it is `limactl shell <vm> sudo ...`: it runs in the run user's own
// session, and that user has passwordless sudo, so a compromised run user
// is root in the guest and controls what the root call reports (and what
// any root command the full path runs does; that was so before the probe
// and is not made worse by it). Nothing on Lima is defended against a
// compromised run user by the root probe; the boundary there is the
// rootless container the agents run in, and the VM around it.
func (g Guest) Probe(ctx context.Context, spec ProbeSpec) (*Snapshot, error) {
	snap := &Snapshot{items: map[string]probeItem{}, spec: spec}
	for _, part := range []struct {
		prefix []string
		script string
		keys   []string
	}{
		{g.UserPrefix, userProbeScript(spec), userProbeKeys(spec)},
		{g.RootPrefix, rootProbeScript(spec), rootProbeKeys(spec)},
	} {
		items, err := g.probeCall(ctx, part.prefix, part.script, part.keys)
		if err != nil {
			return nil, err
		}
		for k, it := range items {
			snap.items[k] = it
		}
	}
	return snap, nil
}

// probeCall runs one probe script through prefix and returns exactly the
// sections keys names, or an error.
func (g Guest) probeCall(ctx context.Context, prefix []string, script string, keys []string) (map[string]probeItem, error) {
	// A nonce per call: the run user sees the user call's.
	nonce, err := probeNonce()
	if err != nil {
		return nil, err
	}
	argv := append(append([]string{}, prefix[1:]...), "/bin/bash", "-c", script, "lever-probe", nonce)
	res, err := g.Host.Run(ctx, nil, prefix[0], argv...)
	if err != nil {
		return nil, fmt.Errorf("guest probe: %w", err)
	}
	items, err := parseProbe(res.Stdout, nonce)
	if err != nil {
		return nil, err
	}
	if len(items) != len(keys) {
		return nil, fmt.Errorf("guest probe: %d sections, want %d", len(items), len(keys))
	}
	for _, k := range keys {
		if _, ok := items[k]; !ok {
			return nil, fmt.Errorf("guest probe: no %q section", k)
		}
	}
	return items, nil
}

// RefreshEgress reads both families' OUTPUT and lever chain listings again,
// in one root call, into s, so VerifyEgress judges the chains as they are
// now rather than as Probe found them: a bring-up may spend minutes between
// the two (a scion build). When the read fails, the listings are dropped
// from s, and VerifyEgress then reports false: the full path runs. A nil s
// is left alone. The alias lookup and the resolver state (ahosts, dnschain,
// resolv) are deliberately left as Probe read them: they only shape the
// ruleset the live chains are compared with, and a chain that changed is
// caught by these four listings; re-reading the alias would take a call
// as the run user too.
func (g Guest) RefreshEgress(ctx context.Context, s *Snapshot) {
	if s == nil {
		return
	}
	items, err := g.probeCall(ctx, g.RootPrefix, probeHead+probeIptables+egressProbeScript, egressProbeKeys)
	for _, k := range egressProbeKeys {
		if err != nil {
			delete(s.items, k)
		} else {
			s.items[k] = items[k]
		}
	}
}

func probeNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("guest probe nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// probeHead is the shell function both probe scripts start with: emit runs
// a command, keeps its stdout (stderr is dropped: a warning there must not
// enter a listing that is compared line for line) and prints it under a
// header line `@@<nonce> <key> <exit code>`.
const probeHead = `N=$1
emit() { k=$1; shift; o=$("$@" 2>/dev/null); c=$?; printf '@@%s %s %d\n%s\n' "$N" "$k" "$c" "$o"; }
`

// userProbeScript reads, as the run user, what ReadRunUser, GOARCH, the
// alias lookup, the pasta drop-in check and the legacy kvm drop-in check
// read. Absolute paths keep a stray PATH entry out of the answers; they do
// not keep out the run user, who controls its own login shell (see Probe).
func userProbeScript(spec ProbeSpec) string {
	var b strings.Builder
	b.WriteString(probeHead)
	// pasta --help may exit non-zero; its text is what decides the drop-in.
	b.WriteString("pastahelp() { pasta --help 2>&1; true; }\n")
	b.WriteString("emit user /usr/bin/id -un\nemit uid /usr/bin/id -u\nemit arch /usr/bin/uname -m\n")
	b.WriteString(`emit kvmlegacy test -e "` + legacyKVMDropInPath + `"` + "\n")
	b.WriteString(`emit pasta /usr/bin/cat "` + pastaDropInPath + `"` + "\n")
	b.WriteString("emit pastahelp pastahelp\n")
	if spec.HostAlias != "" {
		b.WriteString("emit ahosts /usr/bin/getent ahosts " + shellSingleQuote(spec.HostAlias) + "\n")
	}
	return b.String()
}

func userProbeKeys(spec ProbeSpec) []string {
	keys := []string{"user", "uid", "arch", "kvmlegacy", "pasta", "pastahelp"}
	if spec.HostAlias != "" {
		keys = append(keys, "ahosts")
	}
	return keys
}

// bootKeyScript prints the boot id and pid 1's start time (field 22 of
// /proc/1/stat; pid 1's command name has no space).
const bootKeyScript = `bootkey() { /usr/bin/cat /proc/sys/kernel/random/boot_id && /usr/bin/cut -d' ' -f22 /proc/1/stat; }
`

// probeIptables picks the iptables binaries by absolute path, as the rest
// of the probe names its commands, so no PATH entry chooses them: Ubuntu
// (24.04, and OrbStack's Ubuntu machines) installs them in /usr/sbin; /sbin
// is the fallback for a guest without the merged /usr. With neither, the
// listings fail (exit 127), and an unread listing sends the egress step
// down the full path.
const probeIptables = `IPT=/usr/sbin/iptables; [ -x "$IPT" ] || IPT=/sbin/iptables
IP6T=/usr/sbin/ip6tables; [ -x "$IP6T" ] || IP6T=/sbin/ip6tables
`

// egressProbeScript lists both families' OUTPUT and lever chain, the
// sections out4, chain4, out6 and chain6.
const egressProbeScript = `emit out4 "$IPT" -S OUTPUT
emit chain4 "$IPT" -S ` + egress.Chain + `
emit out6 "$IP6T" -S OUTPUT
emit chain6 "$IP6T" -S ` + egress.Chain + `
`

// egressProbeKeys are the sections egressProbeScript prints.
var egressProbeKeys = []string{"out4", "chain4", "out6", "chain6"}

// rootProbeScript reads, as root, the stamp, both families' OUTPUT jump and
// lever chain, the resolver state, the kvm state and the file digests.
// Every command is named by absolute path except test, which is the bash
// builtin (emit runs it as a command word, and no PATH lookup is made).
func rootProbeScript(spec ProbeSpec) string {
	var b strings.Builder
	b.WriteString(probeHead)
	b.WriteString(bootKeyScript)
	b.WriteString(probeIptables)
	b.WriteString("emit boot bootkey\n")
	b.WriteString("emit stamp /usr/bin/cat " + ProvisionStampPath + "\n")
	b.WriteString(egressProbeScript)
	if spec.DNSChain != "" {
		b.WriteString(`emit dnschain "$IPT" -t nat -S ` + shellSingleQuote(spec.DNSChain) + "\n")
	}
	if spec.ResolvConf != "" {
		b.WriteString("emit resolv /usr/bin/cat " + shellSingleQuote(spec.ResolvConf) + "\n")
	}
	b.WriteString("emit kvmchar test -c /dev/kvm\nemit kvmmode /usr/bin/stat -c %a /dev/kvm\n")
	b.WriteString("emit udev /usr/bin/cat " + KVMUdevRulePath + "\nemit udevexists test -e " + KVMUdevRulePath + "\n")
	for i, p := range spec.HashPaths {
		b.WriteString("emit hash" + strconv.Itoa(i) + " /usr/bin/sha256sum " + shellSingleQuote(p) + "\n")
	}
	return b.String()
}

func rootProbeKeys(spec ProbeSpec) []string {
	keys := []string{"boot", "stamp", "out4", "chain4", "out6", "chain6", "kvmchar", "kvmmode", "udev", "udevexists"}
	if spec.DNSChain != "" {
		keys = append(keys, "dnschain")
	}
	if spec.ResolvConf != "" {
		keys = append(keys, "resolv")
	}
	for i := range spec.HashPaths {
		keys = append(keys, "hash"+strconv.Itoa(i))
	}
	return keys
}

var probeHeaderRe = regexp.MustCompile(`^@@([0-9a-f]+) ([a-z0-9]+) ([0-9]+)$`)

// parseProbe splits probe output into its sections. A header with another
// nonce, a key seen twice, or text before the first header is an error.
func parseProbe(out, nonce string) (map[string]probeItem, error) {
	items := map[string]probeItem{}
	var cur string
	var body []string
	flush := func() {
		if cur != "" {
			it := items[cur]
			it.out = strings.Join(body, "\n")
			items[cur] = it
		}
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if m := probeHeaderRe.FindStringSubmatch(line); m != nil && m[1] == nonce {
			flush()
			if _, dup := items[m[2]]; dup {
				return nil, fmt.Errorf("guest probe: section %q twice", m[2])
			}
			code, _ := strconv.Atoi(m[3])
			cur, body = m[2], nil
			items[cur] = probeItem{code: code}
			continue
		}
		if cur == "" {
			return nil, fmt.Errorf("guest probe: unexpected output before the first section")
		}
		body = append(body, line)
	}
	flush()
	return items, nil
}

// ProvisionStampKey names the exact provisioning EnsureRuntimes(runUser)
// performs: a hash of every guest command it issues, in order, recorded by
// running it against a host runner that only records. Any change to those
// scripts (a package added, a drop-in changed, a step removed) changes the
// key, so a guest provisioned by an older lever provisions again; there is no
// version number to forget to bump.
func ProvisionStampKey(runUser string) string {
	rec := &recordRunner{}
	g := Guest{Host: rec, UserPrefix: []string{"user"}, RootPrefix: []string{"root"}}
	_ = g.EnsureRuntimes(context.Background(), runUser)
	h := sha256.New()
	io.WriteString(h, "lever provision stamp v1\n")
	for _, c := range rec.calls {
		io.WriteString(h, strconv.Itoa(len(c))+"\n")
		for _, a := range c {
			io.WriteString(h, strconv.Itoa(len(a))+":"+a+"\n")
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// recordRunner records each command and succeeds without running it.
type recordRunner struct{ calls [][]string }

func (r *recordRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

func (r *recordRunner) RunIn(_ context.Context, _ string, _ map[string]string, name string, args ...string) (proc.Result, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return proc.Result{}, nil
}

func (r *recordRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// writeProvisionStampScript writes "$1 <boot key>" to the stamp atomically,
// root-owned and readable (the key is not a secret; only root may write it).
const writeProvisionStampScript = bootKeyScript + `set -e
umask 022
d=$(dirname ` + ProvisionStampPath + `)
mkdir -p "$d"
b=$(bootkey | tr '\n' ' ')
set -- "$1" $b
[ $# -eq 3 ]
printf '%s %s:%s\n' "$1" "$2" "$3" > ` + ProvisionStampPath + `.tmp
mv -f ` + ProvisionStampPath + `.tmp ` + ProvisionStampPath + `
`

// WriteProvisionStamp records that EnsureRuntimes completed for key in the
// current boot. Best-effort for the caller: a missing stamp only costs the
// next bring-up a full provisioning.
func (g Guest) WriteProvisionStamp(ctx context.Context, key string) error {
	_, err := g.RootRun(ctx, "/bin/bash", "-c", writeProvisionStampScript, "lever-stamp", key)
	return err
}
