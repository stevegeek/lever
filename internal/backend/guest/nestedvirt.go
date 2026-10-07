package guest

import (
	"context"
	"fmt"
	"strings"
)

// KVMUdevRulePath makes /dev/kvm usable by the rootless run user in the
// guest. The guest is a single-user jail, so 0666 grants nothing that is not
// already inside the boundary; a kvm group would need --group-add plumbing
// in scion.
const KVMUdevRulePath = "/etc/udev/rules.d/65-lever-kvm.rules"

// kvmDropInPath is lever's podman drop-in that passes /dev/kvm into every
// container podman creates in the guest (manager, hub, workers).
const kvmDropInPath = "$HOME/.config/containers/containers.conf.d/20-lever-kvm.conf"

const kvmOnRootScript = `set -e
if ! test -c /dev/kvm; then
  echo "lever: no /dev/kvm in the jail VM" >&2
  exit 4
fi
printf '%s\n' 'KERNEL=="kvm", MODE="0666"' > ` + KVMUdevRulePath + `
chmod 0666 /dev/kvm
`

const kvmOnUserScript = `set -e
mkdir -p "$HOME/.config/containers/containers.conf.d"
cat > "` + kvmDropInPath + `" <<'LEVER_EOF'
[containers]
devices = ["/dev/kvm"]
LEVER_EOF
`

const kvmOffRootScript = `rm -f ` + KVMUdevRulePath + "\n"

const kvmOffUserScript = `rm -f "` + kvmDropInPath + `"` + "\n"

// EnsureNestedVirt converges the guest's nested-virt setup: on, it checks
// the jail VM has /dev/kvm, makes it usable by the run user, and installs the
// podman drop-in; off, it removes the rule and the drop-in so the next
// container create has no /dev/kvm. Idempotent; run on every apply (Lima
// only — the caller gates it).
func (g Guest) EnsureNestedVirt(ctx context.Context, on bool) error {
	rootScript, userScript := kvmOffRootScript, kvmOffUserScript
	if on {
		rootScript, userScript = kvmOnRootScript, kvmOnUserScript
	}
	if res, err := g.RootRun(ctx, "bash", "-lc", rootScript); err != nil || res.Code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.Code, strings.TrimSpace(res.Stderr))
		}
		if on {
			return fmt.Errorf("nested_virt: the jail VM has no usable /dev/kvm: it was created before nested_virt, or the host nested module is off; recreate the VM: back up the manager conversation, then `lever destroy` (alias `down`; deletes the VM and the conversation) and `lever up`: %w", err)
		}
		return fmt.Errorf("nested_virt off: remove udev rule: %w", err)
	}
	if res, err := g.UserRun(ctx, "bash", "-lc", userScript); err != nil || res.Code != 0 {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", res.Code, strings.TrimSpace(res.Stderr))
		}
		return fmt.Errorf("nested_virt: podman devices drop-in: %w", err)
	}
	return nil
}
