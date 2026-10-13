package host

import (
	"slices"
	"testing"
)

// The controller role grants attach and, on a hub that has it, lifecycle;
// nothing else.
func TestControllerRolePermissions(t *testing.T) {
	if got := controllerRolePermissions(false); !slices.Equal(got, []string{"agent.attach"}) {
		t.Fatalf("without lifecycle: %v", got)
	}
	if got := controllerRolePermissions(true); !slices.Equal(got, []string{"agent.attach", lifecyclePermission}) {
		t.Fatalf("with lifecycle: %v", got)
	}
}

// The remote role carries agent.lifecycle exactly when asked to, and the
// ceiling built from it always holds the role.
func TestRemoteRoleLifecycleParameter(t *testing.T) {
	for _, has := range []bool{false, true} {
		perms := remoteRolePermissions(has)
		if got := slices.Contains(perms, lifecyclePermission); got != has {
			t.Fatalf("has=%v: role permissions %v", has, perms)
		}
		ceiling := remoteCeilingPermissions([]string{"project.create", "user.read"}, perms)
		for _, p := range perms {
			if !slices.Contains(ceiling, p) {
				t.Fatalf("has=%v: ceiling %v lacks the role's %s", has, ceiling, p)
			}
		}
	}
}
