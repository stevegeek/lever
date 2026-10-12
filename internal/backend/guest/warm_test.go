package guest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/egress"
	"github.com/stevegeek/lever/internal/proc"
)

const testNonce = "00112233445566778899aabbccddeeff"

// A section can only be framed by a header carrying the nonce: a header
// with another nonce inside a section is that section's text.
func TestParseProbeNonceFramesSections(t *testing.T) {
	out := "@@" + testNonce + " ahosts 0\n" +
		"1.2.3.4 STREAM x\n@@deadbeef chain4 0\n-A LEVER_EGRESS -j ACCEPT\n" +
		"@@" + testNonce + " user 0\nalice\n"
	items, err := parseProbe(out, testNonce)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := items["chain4"]; ok {
		t.Fatal("a header with a foreign nonce opened a section")
	}
	if !strings.Contains(items["ahosts"].out, "@@deadbeef chain4 0") || items["user"].out != "alice" {
		t.Fatalf("items = %+v", items)
	}
	for name, bad := range map[string]string{
		"text before the first section": "hello\n@@" + testNonce + " user 0\nalice\n",
		"a section twice":               "@@" + testNonce + " user 0\na\n@@" + testNonce + " user 0\nb\n",
	} {
		if _, err := parseProbe(bad, testNonce); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// Probe fails as a whole when a section it asked for is missing, so a
// partial answer can never verify anything.
func TestProbeRequiresEverySection(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -m m /bin/bash", proc.Result{Stdout: "@@x user 0\nalice\n"})
	if _, err := orbGuest(f, "m").Probe(context.Background(), ProbeSpec{}); err == nil {
		t.Fatal("a probe without its sections must fail")
	}
}

func TestProvisionStampKey(t *testing.T) {
	a, b := ProvisionStampKey("alice"), ProvisionStampKey("alice")
	if a != b || len(a) != 64 {
		t.Fatalf("key not stable: %q %q", a, b)
	}
	if ProvisionStampKey("bob") == a {
		t.Fatal("the run user is part of the provisioning (subuid, linger), so of the key")
	}
}

// The stamp the write script leaves is the one Snapshot.Provisioned
// accepts for the boot the probe reports: run both halves for real, with
// the stamp path moved to a temp dir.
func TestProvisionStampRoundTrip(t *testing.T) {
	if _, err := os.Stat("/proc/sys/kernel/random/boot_id"); err != nil {
		t.Skip("no /proc boot id here")
	}
	path := filepath.Join(t.TempDir(), "lever", "provision.stamp")
	script := strings.ReplaceAll(writeProvisionStampScript, ProvisionStampPath, path)
	key := ProvisionStampKey("alice")
	if out, err := exec.Command("/bin/bash", "-c", script, "lever-stamp", key).CombinedOutput(); err != nil {
		t.Fatalf("stamp script: %v: %s", err, out)
	}
	stamp, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := exec.Command("/bin/bash", "-c", bootKeyScript+"bootkey").Output()
	if err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{items: map[string]probeItem{
		"boot":  {out: strings.TrimRight(string(boot), "\n")},
		"stamp": {out: strings.TrimRight(string(stamp), "\n")},
	}}
	if !snap.Provisioned(key) {
		t.Fatalf("stamp %q not accepted for boot %q", stamp, boot)
	}
	if snap.Provisioned(ProvisionStampKey("bob")) {
		t.Fatal("a stamp for another key was accepted")
	}
	snap.items["boot"] = probeItem{out: "other-boot\n1"}
	if snap.Provisioned(key) {
		t.Fatal("a stamp from another boot was accepted")
	}
}

// TestProbeScriptsRunForReal runs both probe scripts in a local shell: the
// user one as this user, the root one as root of a throwaway user and
// network namespace whose LEVER_EGRESS chains hold the ruleset, so the
// scripts, the parser and VerifyEgress meet real iptables output. Skips
// where unprivileged namespaces are unavailable.
func TestProbeScriptsRunForReal(t *testing.T) {
	if _, err := exec.Command("unshare", "-rn", "iptables", "-S").CombinedOutput(); err != nil {
		t.Skip("iptables in an unprivileged namespace is unavailable here")
	}
	if _, err := exec.LookPath("getent"); err != nil {
		t.Skip("no getent")
	}
	ports := []int{3305}
	rules := egress.BuildRulesDNS("127.0.0.1", "", ports, false, nil)
	t.Setenv("LEVER_TEST_R4", egress.RestoreInput(rules, egress.IPv4))
	t.Setenv("LEVER_TEST_R6", egress.RestoreInput(rules, egress.IPv6))
	setup := `printf '%s' "$LEVER_TEST_R4" | iptables-restore --noflush && printf '%s' "$LEVER_TEST_R6" | ip6tables-restore --noflush &&
iptables -A OUTPUT -j LEVER_EGRESS && ip6tables -A OUTPUT -j LEVER_EGRESS && exec "$@"`
	hashed := filepath.Join(t.TempDir(), "scion")
	if err := os.WriteFile(hashed, []byte("scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := Guest{
		Host:       proc.RealRunner{},
		UserPrefix: []string{"env"},
		RootPrefix: []string{"unshare", "-rn", "/bin/sh", "-c", setup, "setup"},
	}
	snap, err := g.Probe(context.Background(), ProbeSpec{HostAlias: "localhost", HashPaths: []string{hashed}})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if u, uid, ok := snap.RunUser(); !ok || u != me.Username || uid != me.Uid {
		t.Errorf("RunUser = %q %q %v, want %q %q", u, uid, ok, me.Username, me.Uid)
	}
	if _, ok := snap.GOARCH(); !ok {
		t.Error("no arch")
	}
	sum := sha256.Sum256([]byte("scion"))
	if d, ok := snap.Digest(hashed); !ok || d != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %q %v", d, ok)
	}
	if _, ok := snap.bootKey(); !ok {
		t.Error("no boot key")
	}
	// localhost resolves to 127.0.0.1 (and maybe ::1); verify against the
	// ruleset built from whatever it resolved to.
	ah, _ := snap.item("ahosts")
	v4, v6 := ParseAhosts(ah)
	if v4 != "127.0.0.1" || v6 != "" {
		t.Skipf("localhost resolves to %q %q here; the chain was loaded for 127.0.0.1 only", v4, v6)
	}
	if got, ok := snap.VerifyEgress(ports, false, false, nil); !ok || got != "127.0.0.1" {
		t.Fatalf("VerifyEgress = %q %v on the chain it describes\nchain4:\n%s", got, ok, snap.items["chain4"].out)
	}
	if _, ok := snap.VerifyEgress([]int{3305, 9000}, false, false, nil); ok {
		t.Fatal("VerifyEgress accepted a chain missing a port")
	}
}
