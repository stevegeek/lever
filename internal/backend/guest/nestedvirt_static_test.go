package guest

import (
	"strings"
	"testing"
)

// udevd applies a rule's MODE to a static node at start-up only when the
// rule names it with static_node; otherwise 0660 returns at every restart.
func TestKVMUdevRuleAppliesToTheStaticNode(t *testing.T) {
	if !strings.Contains(kvmUdevRule, `OPTIONS+="static_node=kvm"`) {
		t.Fatalf("kvm udev rule %q lacks static_node=kvm", kvmUdevRule)
	}
	if !strings.Contains(kvmOnRootScript, kvmUdevRule) {
		t.Fatal("the nested_virt on script does not write the rule")
	}
}
