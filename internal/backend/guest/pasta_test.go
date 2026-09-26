package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPastaScript runs pastaDropInScript under bash with HOME in a temp dir
// and, when help is non-nil, a fake pasta on PATH that prints it.
func runPastaScript(t *testing.T, help *string) (dropIn string, out string, err error) {
	t.Helper()
	home := t.TempDir()
	bin := t.TempDir()
	if help != nil {
		fake := "#!/bin/sh\ncat <<'EOF'\n" + *help + "\nEOF\n"
		if werr := os.WriteFile(filepath.Join(bin, "pasta"), []byte(fake), 0o755); werr != nil {
			t.Fatal(werr)
		}
	}
	for _, tool := range []string{"sh", "cat", "mkdir"} {
		p, lerr := exec.LookPath(tool)
		if lerr != nil {
			t.Fatalf("look up %s: %v", tool, lerr)
		}
		if serr := os.Symlink(p, filepath.Join(bin, tool)); serr != nil {
			t.Fatal(serr)
		}
	}
	bash, lerr := exec.LookPath("bash")
	if lerr != nil {
		t.Skip("bash not available")
	}
	// set -e is part of the contract: a failed mkdir or write must fail the
	// apply, not leave agents on the old drop-in.
	if !strings.HasPrefix(pastaDropInScript(), "set -e\n") {
		t.Fatal("the drop-in script must start with set -e")
	}
	cmd := exec.Command(bash, "-c", pastaDropInScript())
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin}
	b, err := cmd.CombinedOutput()
	got, _ := os.ReadFile(filepath.Join(home, ".config/containers/containers.conf.d/10-lever-pasta.conf"))
	return string(got), string(b), err
}

// Help text excerpts from the two passt versions lever meets: the OrbStack
// guest's passt 2026-01-20 and Ubuntu 24.04's passt 2024-02-20.
const (
	modernPastaHelp = "  --map-host-loopback ADDR\tTranslate ADDR to refer to host\n  --map-guest-addr ADDR\tTranslate ADDR to guest's address\n  --no-map-gw\t\tDon't map gateway address to host"
	oldPastaHelp    = "  -g, --gateway ADDR\tPass IPv4 or IPv6 address as gateway\n  --no-map-gw\t\tDon't map gateway address to host"
)

func TestPastaDropInModernPasta(t *testing.T) {
	h := modernPastaHelp
	got, out, err := runPastaScript(t, &h)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if got != pastaDropInModern {
		t.Fatalf("drop-in:\n%s\nwant:\n%s", got, pastaDropInModern)
	}
}

func TestPastaDropInOldPastaUsesGatewayMapping(t *testing.T) {
	h := oldPastaHelp
	got, out, err := runPastaScript(t, &h)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if got != pastaDropInGateway {
		t.Fatalf("drop-in:\n%s\nwant:\n%s", got, pastaDropInGateway)
	}
	// The old pasta rejects --map-host-loopback and every container would
	// fail to start.
	if strings.Contains(got, "--map-host-loopback") {
		t.Fatal("the gateway drop-in must not pass --map-host-loopback")
	}
}

// Both drop-ins must switch podman 4.9 off its slirp4netns default, and must
// make host.containers.internal reach the hub address.
func TestPastaDropInsSelectPastaAndHubAddress(t *testing.T) {
	for name, d := range map[string]string{"modern": pastaDropInModern, "gateway": pastaDropInGateway} {
		if !strings.Contains(d, `default_rootless_network_cmd = "pasta"`) {
			t.Errorf("%s drop-in does not select pasta", name)
		}
		if !strings.Contains(d, `"`+PastaHostAddr+`"`) {
			t.Errorf("%s drop-in does not name %s", name, PastaHostAddr)
		}
	}
	for name, d := range map[string]string{"modern": pastaDropInModern, "gateway": pastaDropInGateway} {
		if !strings.Contains(d, `host_containers_internal_ip = "`+PastaHostAddr+`"`) {
			t.Errorf("%s drop-in does not pin host.containers.internal to %s", name, PastaHostAddr)
		}
	}
	if !strings.Contains(pastaDropInGateway, `"--map-gw"`) {
		t.Error("the gateway drop-in needs --map-gw (without it the hub is unreachable)")
	}
	// Without -4 pasta copies the guest's IPv6 gateway, and --map-gw maps it
	// to the guest's ::1.
	if !strings.Contains(pastaDropInGateway, `"-4"`) {
		t.Error("the gateway drop-in must keep pasta IPv4-only")
	}
}

func TestPastaDropInFailsWithoutPasta(t *testing.T) {
	got, out, err := runPastaScript(t, nil)
	if err == nil {
		t.Fatalf("want a failure with no pasta installed; output %q", out)
	}
	if got != "" || !strings.Contains(out, "no pasta") {
		t.Fatalf("drop-in %q, output %q; want no drop-in and a no-pasta message", got, out)
	}
}

func TestPastaDropInFailsOnUnknownPasta(t *testing.T) {
	h := "  -a, --address ADDR\tAssign IPv4 or IPv6 address ADDR"
	got, out, err := runPastaScript(t, &h)
	if err == nil || got != "" || !strings.Contains(out, "supports neither") {
		t.Fatalf("err %v, drop-in %q, output %q; want a refusal and no drop-in", err, got, out)
	}
}
