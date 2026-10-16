package guest

import (
	"context"
	"fmt"
	"strings"
)

// KVMUdevRulePath makes /dev/kvm usable by the rootless run user in the
// guest. Only the manager container holds the device (apply bind-mounts it
// at create); a worker has no /dev/kvm node and cannot make one in its user
// namespace. A kvm group would need --group-add plumbing in scion.
const KVMUdevRulePath = "/etc/udev/rules.d/65-lever-kvm.rules"

// legacyKVMDropInPath is a podman drop-in that passes /dev/kvm into EVERY
// container in the guest, hub and workers too. Every apply removes it.
const legacyKVMDropInPath = "$HOME/.config/containers/containers.conf.d/20-lever-kvm.conf"

// static_node makes udevd apply the mode to the static /dev/kvm node each
// time it starts (at boot, and when a package upgrade restarts it); without
// it the stock rule's 0660 wins then.
const kvmUdevRule = `KERNEL=="kvm", MODE="0666", OPTIONS+="static_node=kvm"`

const kvmOnRootScript = `set -e
if ! test -c /dev/kvm; then
  echo "lever: no /dev/kvm in the jail VM" >&2
  exit 4
fi
printf '%s\n' '` + kvmUdevRule + `' > ` + KVMUdevRulePath + `
chmod 0666 /dev/kvm
`

// kvmOffRootScript also takes the device back to 0660 for the current boot:
// removing the rule alone leaves it 0666 until the next reboot.
const kvmOffRootScript = `rm -f ` + KVMUdevRulePath + `
if test -c /dev/kvm; then chmod 0660 /dev/kvm; fi
exit 0
`

const kvmUserScript = `rm -f "` + legacyKVMDropInPath + `"` + "\n"

// EnsureNestedVirt converges the guest's nested-virt setup: on, it checks
// the jail VM has /dev/kvm and makes it usable by the run user; off, it
// removes the rule and sets the device back to 0660. Idempotent. Lima only:
// the caller gates it.
func (g Guest) EnsureNestedVirt(ctx context.Context, on bool) error {
	rootScript := kvmOffRootScript
	if on {
		rootScript = kvmOnRootScript
	}
	if res, err := g.RootRun(ctx, "bash", "-lc", rootScript); err != nil || res.Code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.Code, strings.TrimSpace(res.Stderr))
		}
		if on {
			return fmt.Errorf("nested_virt: the jail VM has no usable /dev/kvm: it was created before nested_virt, or the host nested module is off; recreate the VM: `lever conversation backup`, then `lever destroy` (alias `down`; deletes the VM and the conversation) and `lever up`, then `lever conversation restore`: %w", err)
		}
		return fmt.Errorf("nested_virt off: remove udev rule: %w", err)
	}
	if res, err := g.UserRun(ctx, "bash", "-lc", kvmUserScript); err != nil || res.Code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.Code, strings.TrimSpace(res.Stderr))
		}
		return fmt.Errorf("nested_virt: remove the legacy podman devices drop-in: %w", err)
	}
	return nil
}
