package host

import (
	"context"
	"errors"
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

// The remote role carries agent.lifecycle exactly when asked to.
func TestRemoteRoleLifecycleParameter(t *testing.T) {
	for _, has := range []bool{false, true} {
		h := newFakeAdminHub("op@github")
		rec, err := ensureRemoteWebRole(context.Background(), &hubapi.Client{T: h}, "lever", []string{"op@github"}, has, time.Now(), t.Logf)
		if err != nil {
			t.Fatalf("has=%v: %v", has, err)
		}
		if got := slices.Contains(rec.Permissions, lifecyclePermission); got != has {
			t.Fatalf("has=%v: role permissions %v", has, rec.Permissions)
		}
		if !ceilingPermissionsFit(rec.CeilingPermissions, rec.Permissions) {
			t.Fatalf("has=%v: ceiling %v does not hold the role %v", has, rec.CeilingPermissions, rec.Permissions)
		}
	}
}

// After a scion upgrade to one with agent.lifecycle, a grant recorded
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
		t.Fatalf("a scion with agent.lifecycle must re-grant: %q", r)
	}
}

func TestRemoteRoleLifecycleDecision(t *testing.T) {
	ctx := context.Background()
	with := state.RemoteRoleRecord{Permissions: remoteRolePermissions(true)}
	without := state.RemoteRoleRecord{Permissions: remoteRolePermissions(false)}
	yes := func(context.Context, string) (bool, error) { return true, nil }
	no := func(context.Context, string) (bool, error) { return false, nil }
	broken := func(context.Context, string) (bool, error) { return false, errors.New("scion not installed") }
	cases := []struct {
		name  string
		known scopeKnownFunc
		rec   state.RemoteRoleRecord
		want  bool
	}{
		{"scion knows it", yes, without, true},
		{"scion does not", no, with, false},
		{"cannot ask: keep the recorded variant (with)", broken, with, true},
		{"cannot ask: keep the recorded variant (without)", broken, without, false},
		{"no probe: keep the recorded variant", nil, with, true},
	}
	for _, c := range cases {
		if got := remoteRoleLifecycle(ctx, c.known, c.rec); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Doctor judges the remote role against what the jail's scion supports, or
// the recorded variant when it cannot ask.
func TestCheckRemoteWebRoleLifecycle(t *testing.T) {
	ctx := context.Background()
	ra := remoteAccess{Enabled: true, Emails: []string{"op@github"}}
	seed := func(lifecycle bool) state.State {
		st := state.ForConfig(t.TempDir())
		p := remoteRolePermissions(lifecycle)
		if err := st.SaveRemoteRoleRecord(state.RemoteRoleRecord{Permissions: p,
			Bound: map[string]string{"op@github": "u1"}, Ceilings: map[string]string{"op@github": "c1"},
			CeilingPermissions: p}); err != nil {
			t.Fatal(err)
		}
		return st
	}
	yes := func(context.Context, string) (bool, error) { return true, nil }
	if r := checkRemoteWebRole(ctx, seed(true), ra, nil); !r.ok {
		t.Fatalf("a lifecycle grant with no probe must be green: %+v", r)
	}
	if r := checkRemoteWebRole(ctx, seed(true), ra, yes); !r.ok {
		t.Fatalf("a lifecycle grant on a scion with lifecycle must be green: %+v", r)
	}
	if r := checkRemoteWebRole(ctx, seed(false), ra, yes); r.ok || !strings.Contains(r.detail, "agent.lifecycle") {
		t.Fatalf("an upgraded scion must flag the missing agent.lifecycle: %+v", r)
	}
}
