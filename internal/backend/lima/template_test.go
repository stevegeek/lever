package lima

import (
	"os"
	"os/exec"
	"path/filepath"
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
	out, err := RenderTemplate("/Users/x/proj", TemplateOpts{})
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
	assertDiskLine(t, "", "disk: 24GiB")
}

func TestRenderTemplateDiskOverride(t *testing.T) {
	assertDiskLine(t, "48GiB", "disk: 48GiB")
}

// assertDiskLine renders the template with disk and checks the disk line.
func assertDiskLine(t *testing.T, disk, want string) {
	t.Helper()
	out, err := RenderTemplate("/tmp/tree", TemplateOpts{Disk: disk})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("expected %q line, got:\n%s", want, out)
	}
}

func TestRenderTemplateSizingAndNested(t *testing.T) {
	out, err := RenderTemplate("/tmp/tree", TemplateOpts{CPUs: 8, Memory: "24GiB", NestedVirt: true})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		CPUs    int    `yaml:"cpus"`
		Memory  string `yaml:"memory"`
		CPUType any    `yaml:"cpuType"`
		VMOpts  struct {
			QEMU struct {
				CPUType map[string]string `yaml:"cpuType"`
			} `yaml:"qemu"`
		} `yaml:"vmOpts"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("yaml: %v\n%s", err, out)
	}
	if doc.CPUs != 8 || doc.Memory != "24GiB" {
		t.Fatalf("cpus/memory = %d/%q, want 8/24GiB\n%s", doc.CPUs, doc.Memory, out)
	}
	if doc.CPUType != nil {
		t.Fatalf("deprecated top-level cpuType must not be rendered\n%s", out)
	}
	if runtime.GOOS == "darwin" {
		if len(doc.VMOpts.QEMU.CPUType) != 0 {
			t.Fatalf("vz host must not render qemu cpuType\n%s", out)
		}
		return
	}
	if doc.VMOpts.QEMU.CPUType["x86_64"] != "host" || doc.VMOpts.QEMU.CPUType["aarch64"] != "host" {
		t.Fatalf("vmOpts.qemu.cpuType = %v, want host for both\n%s", doc.VMOpts.QEMU.CPUType, out)
	}
}

func TestRenderTemplateUnsetSizingOmitsKeys(t *testing.T) {
	out, err := RenderTemplate("/tmp/tree", TemplateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"\ncpus:", "\nmemory:", "vmOpts:"} {
		if strings.Contains(out, k) {
			t.Fatalf("unset option rendered %q:\n%s", k, out)
		}
	}
}

func TestRenderedTemplateLimaValidates(t *testing.T) {
	limactl, err := exec.LookPath("limactl")
	if err != nil {
		t.Skip("limactl not installed")
	}
	out, err := RenderTemplate(t.TempDir(), TemplateOpts{CPUs: 8, Memory: "24GiB", NestedVirt: true})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "lever.yaml")
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(limactl, "validate", p).CombinedOutput(); err != nil {
		t.Fatalf("limactl validate: %v\n%s\n%s", err, b, out)
	}
}
