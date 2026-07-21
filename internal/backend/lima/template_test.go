package lima

import (
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The template IS the containment surface (spec §4). This test pins the
// properties whose regression would leak: exactly one mount (the project tree,
// writable, at /lever), ALL automatic port-forwarding suppressed (a jailed
// agent must not be able to squat host-loopback ports), containerd off.
func TestTemplateContainmentProperties(t *testing.T) {
	out, err := RenderTemplate("/Users/x/proj", "")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		VMType    string `yaml:"vmType"`
		MountType string `yaml:"mountType"`
		Mounts    []struct {
			Location   string `yaml:"location"`
			MountPoint string `yaml:"mountPoint"`
			Writable   bool   `yaml:"writable"`
		} `yaml:"mounts"`
		Containerd struct {
			System bool `yaml:"system"`
			User   bool `yaml:"user"`
		} `yaml:"containerd"`
		PortForwards []struct {
			GuestIP           string `yaml:"guestIP"`
			GuestIPMustBeZero bool   `yaml:"guestIPMustBeZero"`
			GuestPortRange    []int  `yaml:"guestPortRange"`
			Ignore            bool   `yaml:"ignore"`
			Proto             string `yaml:"proto"`
		} `yaml:"portForwards"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("template is not valid YAML: %v\n%s", err, out)
	}
	if len(doc.Mounts) != 1 || doc.Mounts[0].Location != "/Users/x/proj" ||
		doc.Mounts[0].MountPoint != "/lever" || !doc.Mounts[0].Writable {
		t.Fatalf("mounts must be exactly the project tree, writable, at /lever: %+v", doc.Mounts)
	}
	if doc.Containerd.System || doc.Containerd.User {
		t.Fatalf("containerd must be disabled: %+v", doc.Containerd)
	}
	// Exactly two ignore rules are required: 0.0.0.0 (all interfaces) and
	// 127.0.0.1 (loopback). Dropping either would leave a class of guest
	// listeners auto-forwarded to the host.
	if len(doc.PortForwards) != 2 {
		t.Fatalf("expected exactly 2 portForwards ignore rules (0.0.0.0 + 127.0.0.1), got %d: %+v", len(doc.PortForwards), doc.PortForwards)
	}
	gotIPs := map[string]bool{}
	var zeroEntryMustBeZero bool
	for _, pf := range doc.PortForwards {
		if !pf.Ignore || len(pf.GuestPortRange) != 2 || pf.GuestPortRange[0] != 1 || pf.GuestPortRange[1] != 65535 {
			t.Fatalf("every portForwards entry must be a full-range ignore: %+v", pf)
		}
		// Lima defaults an omitted proto to "tcp"; without an explicit "any" here,
		// the ignore rule only suppresses TCP auto-forwarding and a guest UDP
		// listener still gets forwarded to host loopback by lima's builtin fallback
		// rule (proto: "any"), letting a jailed agent squat a free host-loopback UDP
		// port. Both ignore rules must cover ALL protocols, not just TCP.
		if pf.Proto != "any" {
			t.Fatalf("portForwards entry must set proto: \"any\" (lima defaults omitted proto to tcp, leaving UDP auto-forwarded): %+v", pf)
		}
		gotIPs[pf.GuestIP] = true
		if pf.GuestIP == "0.0.0.0" {
			zeroEntryMustBeZero = pf.GuestIPMustBeZero
		}
	}
	wantIPs := map[string]bool{"0.0.0.0": true, "127.0.0.1": true}
	if len(gotIPs) != len(wantIPs) || !gotIPs["0.0.0.0"] || !gotIPs["127.0.0.1"] {
		t.Fatalf("portForwards guestIPs must be exactly {0.0.0.0, 127.0.0.1}, got %+v", gotIPs)
	}
	if !zeroEntryMustBeZero {
		t.Fatal("the 0.0.0.0 portForwards entry must set guestIPMustBeZero: true (auto-inference is lima >=2.0 only)")
	}
	if runtime.GOOS == "darwin" {
		if doc.VMType != "vz" {
			t.Fatalf("vmType on darwin = %q, want vz", doc.VMType)
		}
		if doc.MountType != "virtiofs" {
			t.Fatalf("mountType on darwin = %q, want virtiofs", doc.MountType)
		}
	} else if doc.MountType != "" {
		t.Fatalf("mountType on non-darwin = %q, want empty (lima default)", doc.MountType)
	}
	if !strings.Contains(out, "cloud-images.ubuntu.com") {
		t.Fatal("expected Ubuntu LTS images")
	}
}

func TestRenderTemplateDiskDefault(t *testing.T) {
	out, err := RenderTemplate("/tmp/tree", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "disk: 24GiB") {
		t.Fatalf("expected default disk line, got:\n%s", out)
	}
}

func TestRenderTemplateDiskOverride(t *testing.T) {
	out, err := RenderTemplate("/tmp/tree", "48GiB")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "disk: 48GiB") {
		t.Fatalf("expected overridden disk line, got:\n%s", out)
	}
}
