package lima

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// nestedParamPaths are the KVM module parameters that say whether the host
// lets a guest run its own KVM guests.
var nestedParamPaths = []string{
	"/sys/module/kvm_amd/parameters/nested",
	"/sys/module/kvm_intel/parameters/nested",
}

// readHostFile is os.ReadFile; a test seam.
var readHostFile = os.ReadFile

// hostArch is runtime.GOARCH; a test seam.
var hostArch = runtime.GOARCH

// checkHostNested refuses nested_virt on a host whose KVM module has nested
// virtualization off: the jail VM would boot without /dev/kvm and the guest
// step would fail later with a less direct message. lever reads only the
// Intel/AMD module parameters, so any other host arch is refused outright.
func checkHostNested(arch string, read func(string) ([]byte, error)) error {
	if arch != "amd64" {
		return fmt.Errorf("nested_virt: lever supports nested virtualization only on x86_64 (Intel/AMD) Linux hosts; this host is %s", arch)
	}
	for _, p := range nestedParamPaths {
		b, err := read(p)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(b)) {
		case "1", "Y", "y":
			return nil
		}
	}
	return fmt.Errorf("nested_virt: the host's KVM module has nested virtualization off (or KVM is not loaded). " +
		"On Intel: echo 'options kvm_intel nested=1' | sudo tee /etc/modprobe.d/kvm-nested.conf, then reboot or `sudo modprobe -r kvm_intel && sudo modprobe kvm_intel` with no VM running; on AMD it is on by default (check /sys/module/kvm_amd/parameters/nested)")
}
