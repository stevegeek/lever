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
	out, err := RenderTemplate("/Users/x/proj")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		VMType string `yaml:"vmType"`
		Mounts []struct {
			Location   string `yaml:"location"`
			MountPoint string `yaml:"mountPoint"`
			Writable   bool   `yaml:"writable"`
		} `yaml:"mounts"`
		Containerd struct {
			System bool `yaml:"system"`
			User   bool `yaml:"user"`
		} `yaml:"containerd"`
		PortForwards []struct {
			GuestIP        string `yaml:"guestIP"`
			GuestPortRange []int  `yaml:"guestPortRange"`
			Ignore         bool   `yaml:"ignore"`
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
	if len(doc.PortForwards) == 0 {
		t.Fatal("portForwards ignore rules missing — guest listeners would be forwarded to host loopback")
	}
	for _, pf := range doc.PortForwards {
		if !pf.Ignore || len(pf.GuestPortRange) != 2 || pf.GuestPortRange[0] != 1 || pf.GuestPortRange[1] != 65535 {
			t.Fatalf("every portForwards entry must be a full-range ignore: %+v", pf)
		}
	}
	if runtime.GOOS == "darwin" && doc.VMType != "vz" {
		t.Fatalf("vmType on darwin = %q, want vz", doc.VMType)
	}
	if !strings.Contains(out, "cloud-images.ubuntu.com") {
		t.Fatal("expected Ubuntu LTS images")
	}
}
