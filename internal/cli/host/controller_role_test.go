package host

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/state"
)

// The token issuer gets a project role holding agent.attach on the instance
// project, so a mint that asks for agent:attach passes scion's ceiling on
// scion f7155ecb and later. A second run changes nothing.
func TestEnsureControllerRoleBindsIssuer(t *testing.T) {
	h := newFakeAdminHub()
	h.me = "dev-user"
	hc := &hubapi.Client{T: h}
	for run := 0; run < 2; run++ {
		if err := ensureControllerRole(context.Background(), hc, "lever", t.Logf); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if len(h.roles) != 1 || h.roles[0].Name != controllerRoleName || h.roles[0].ScopeType != hubapi.RoleScopeProject ||
		!slices.Equal(h.roles[0].Permissions, []string{"agent.attach"}) {
		t.Fatalf("roles = %+v; want one project role %s with agent.attach only", h.roles, controllerRoleName)
	}
	if len(h.bindings) != 1 {
		t.Fatalf("bindings = %+v; want exactly one", h.bindings)
	}
	b := h.bindings[0]
	if b.PrincipalID != "dev-user" || b.ScopeType != hubapi.RoleScopeProject || b.ScopeID != h.projectID || b.RoleDefinitionID != h.roles[0].ID {
		t.Fatalf("binding %+v; want dev-user bound to %s on project %s", b, controllerRoleName, h.projectID)
	}
}

// Without an identity the grant cannot bind anyone: an error, which the
// window turns into a warning.
func TestEnsureControllerRoleNeedsIssuer(t *testing.T) {
	h := newFakeAdminHub()
	if err := ensureControllerRole(context.Background(), &hubapi.Client{T: h}, "lever", t.Logf); err == nil {
		t.Fatal("want an error when the hub does not say who the caller is")
	}
	if len(h.bindings) != 0 {
		t.Fatalf("nothing may be bound: %+v", h.bindings)
	}
}

// The remote role carries agent.lifecycle exactly when the hub lists
// agent:lifecycle; an older hub would reject the unknown permission.
func TestRemoteRoleLifecycleFollowsHub(t *testing.T) {
	for _, has := range []bool{false, true} {
		h := newFakeAdminHub("op@github")
		h.scopes = []string{"agent:read", "agent:attach"}
		if has {
			h.scopes = append(h.scopes, lifecycleScope)
		}
		rec, err := ensureRemoteWebRole(context.Background(), &hubapi.Client{T: h}, "lever", []string{"op@github"}, time.Now(), t.Logf)
		if err != nil {
			t.Fatalf("has=%v: %v", has, err)
		}
		if got := slices.Contains(rec.Permissions, lifecyclePermission); got != has {
			t.Fatalf("has=%v: role permissions %v", has, rec.Permissions)
		}
	}
}

// After a scion upgrade to a hub with agent.lifecycle, a grant recorded
// without it must run again; the recorded variant alone is accepted.
func TestRemoteRoleReasonAfterScionUpgrade(t *testing.T) {
	rec := state.RemoteRoleRecord{
		Permissions: remoteRolePermissions(false), Bound: map[string]string{"op@github": "u1"},
		Ceilings: map[string]string{"op@github": "c1"}, CeilingPermissions: remoteRolePermissions(false),
	}
	if r := remoteRoleReason(rec, true, []string{"op@github"}, remoteRolePermissions(recordedLifecycle(rec))); r != "" {
		t.Fatalf("the recorded variant must fit: %q", r)
	}
	if r := remoteRoleReason(rec, true, []string{"op@github"}, remoteRolePermissions(true)); !strings.Contains(r, "agent.lifecycle") {
		t.Fatalf("a hub with agent.lifecycle must re-grant: %q", r)
	}
}

func TestHubHasLifecycle(t *testing.T) {
	h := newFakeAdminHub()
	h.scopes = []string{"agent:attach", lifecycleScope}
	if has, ok := hubHasLifecycle(context.Background(), &hubapi.Client{T: h}); !has || !ok {
		t.Fatalf("has=%v ok=%v", has, ok)
	}
	h.scopes = []string{"agent:attach"}
	if has, ok := hubHasLifecycle(context.Background(), &hubapi.Client{T: h}); has || !ok {
		t.Fatalf("old hub: has=%v ok=%v", has, ok)
	}
}
