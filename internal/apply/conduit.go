package apply

import (
	"maps"

	"github.com/stevegeek/lever/internal/conduit"
	"github.com/stevegeek/lever/internal/config"
)

// AgentGatewayPort is lever-agent's plaintext gateway in every agent
// (agent.LocalGatewayAddr). Anything that reaches it acts as the agent,
// with the agent's capabilities, so no tunnel or SSH forward may.
const AgentGatewayPort = 8462

// ConduitPolicy is the conduit restriction lever starts the hub with on a
// scion that has the controls: SSH off unless the operator opted into
// `lever ssh`, lever's own listeners in the agent and ssh.deny_ports denied to
// every tunnel and forward, and no SSH socket forwarding (tmux and podman
// sockets included). It restricts forwarding only: an SSH shell is the agent's
// own user and reaches anything the agent can.
func ConduitPolicy(app *config.App) conduit.Policy {
	return conduit.Policy{
		DisableSSH:                 !app.SSHEnabled(),
		DenyPorts:                  append([]int{AgentGatewayPort}, app.SSH.DenyPorts...),
		DisableSSHSocketForwarding: true,
	}.Normalized()
}

// HubEnv is the env of every hub start: HubAuthEnv, plus ConduitPolicy's when
// the jail's scion has the conduit controls. A scion without them would ignore
// the policy, so lever never relies on it there and keeps conduit off.
func HubEnv(instanceID, keyFingerprint string, app *config.App, conduitControls bool) map[string]string {
	env := HubAuthEnv(instanceID, keyFingerprint)
	if conduitControls {
		maps.Copy(env, ConduitPolicy(app).HubEnv())
	}
	return env
}
