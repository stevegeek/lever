package host

// controllerRoleName is the project role lever binds to the controller, the
// hub user that owns the instance project and mints the controller PAT.
//
// scion's mint ceiling lets a token carry only scopes its issuer holds
// through a project-scope binding on the target project; hub-admin and owner
// grants do not count, and newer scions' owner and admin roles lack
// agent.attach, so minting agent:attach fails with scope_violation. lever
// needs attach (`lever attach`, `lever up`), so this role grants the
// controller agent.attach on the instance project and nothing else. Every
// agent is created through the controller, so attach reaches no other user's
// agents. Its ID is the hub marker hub-identity records.
const controllerRoleName = "lever-controller"

const controllerRoleDescription = "Managed by lever: lets the instance's controller token attach to agent sessions (lever attach, lever up) and start, stop and resume agents."

// controllerRolePermissions: lifecycle adds agent.lifecycle, for a hub that
// has it; the controller PAT can carry agent:lifecycle only when the issuer
// holds it here. An older hub rejects the unknown permission.
func controllerRolePermissions(lifecycle bool) []string {
	if lifecycle {
		return []string{"agent.attach", lifecyclePermission}
	}
	return []string{"agent.attach"}
}
