package host

import (
	"context"
	"fmt"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
)

// controllerRoleName is the project role lever binds to the identity that
// mints the instance's PATs (the throwaway hub's dev user).
//
// Why it exists: scion's mint ceiling (pkg/hub/useraccesstoken.go) lets a
// token carry only scopes its issuer holds through a PROJECT-scope binding
// on the target project; hub-admin authority and resource-owner grants do
// not count. Since scion f7155ecb (#1838) the project-owner and admin roles
// no longer hold agent.attach, so a mint that asks for agent:attach fails
// with scope_violation. lever needs attach: `lever attach` and `lever up`
// run `scion attach` with the controller PAT, and the remote PAT carries it
// too. This role gives the issuer agent.attach on the instance project and
// nothing else. On an older scion, where the owner role still holds attach,
// it changes nothing. Every agent on the instance is created through the
// controller identity, so attach reaches no other user's agents.
const controllerRoleName = "lever-controller"

const controllerRoleDescription = "Managed by lever: lets the instance's controller token attach to agent sessions (lever attach, lever up) and start, stop and resume agents."

// controllerRolePermissions is the controller role's permission set.
// lifecycle adds agent.lifecycle, for a hub that has it: the mint ceiling
// counts only project-scope bindings, so the controller PAT can carry
// agent:lifecycle (controllerPATScopes) only when the issuer holds it here.
// An older hub rejects the unknown permission.
func controllerRolePermissions(lifecycle bool) []string {
	if lifecycle {
		return []string{"agent.attach", lifecyclePermission}
	}
	return []string{"agent.attach"}
}

// windowAdminHub is the transport for admin calls in the dev-auth window:
// curl in the jail against the throwaway hub with its dev token, or
// o.AdminHub in tests.
func windowAdminHub(ctx context.Context, jr proc.Runner, o patMintOpts) (hubapi.Doer, error) {
	if o.AdminHub != nil {
		return o.AdminHub, nil
	}
	tok, err := readDevToken(ctx, jr)
	if err != nil {
		return nil, err
	}
	return &hubapi.JailCurl{Runner: jr, BaseURL: throwawayHubURL, Token: func() string { return tok }}, nil
}

// ensureControllerRole makes the hub hold the controller role and binds the
// calling identity (the dev user of the throwaway hub) to it on the
// instance project. Idempotent.
func ensureControllerRole(ctx context.Context, hc *hubapi.Client, projectKey string, lifecycle bool, warn func(string, ...any)) error {
	me, err := hc.Me(ctx)
	if err != nil {
		return fmt.Errorf("resolving the token issuer: %w", err)
	}
	projectID, err := hc.ProjectID(ctx, projectKey, throwawayHubURL)
	if err != nil {
		return err
	}
	defs, err := hc.RoleDefinitions(ctx)
	if err != nil {
		return err
	}
	role, err := ensureProjectRole(ctx, hc, defs, controllerRoleName, controllerRoleDescription, controllerRolePermissions(lifecycle), warn)
	if err != nil {
		return err
	}
	if err := ensureProjectRoleBinding(ctx, hc, role.ID, me.ID, projectID); err != nil {
		return fmt.Errorf("binding the token issuer to role %s: %w", controllerRoleName, err)
	}
	return nil
}

// grantControllerRole runs ensureControllerRole in the window, before any
// PAT is minted. A failure is a warning, not an error: on a scion before
// f7155ecb the mint does not need the role, and on a later one the mint
// then fails with scope_violation, which this warning explains.
func grantControllerRole(ctx context.Context, jr proc.Runner, projectKey string, lifecycle bool, o patMintOpts) {
	hub, err := windowAdminHub(ctx, jr, o)
	if err == nil {
		err = ensureControllerRole(ctx, &hubapi.Client{T: hub}, projectKey, lifecycle, o.warn)
	}
	if err != nil {
		o.warn("bootstrap-token: could not grant the %s role (agent.attach for the token issuer); "+
			"on scion f7155ecb or later the token mint then fails with scope_violation: %v", controllerRoleName, err)
	}
}
