---
title: "SSH and VS Code"
nav_order: 5.81
---
# SSH and VS Code: a shell in an agent

`lever attach` gives you an agent's tmux session. `lever ssh` gives you an SSH session into the
manager or a worker instead: a shell, a remote command, `sftp`, and VS Code's Remote-SSH. It runs
over scion's hub relay (conduit) through the jail, with no port forwarded from the host into the
guest. It is off by default, and it is for the operator on the host only: no web login, operator
or contact, gets it.

## Requirements

- A scion whose hub has the **conduit controls**: the `server.hub.conduit` settings
  `disable_ssh`, `deny_ports` and `disable_ssh_socket_forwarding`, which the hub hands to every
  agent it starts. The scion lever pins today does not have them. On such a scion
  `lever apply` keeps conduit off and warns, `lever ssh` refuses, and `lever doctor`'s *lever ssh*
  row says why.
- Agent images whose `sciontool` enforces that policy, that is, built from such a scion. lever
  checks every agent image in the jail; an image it cannot check (not loaded yet, on a first
  apply) keeps conduit off until the next apply.
- An `ssh` client on the host, and VS Code's Remote-SSH extension for VS Code.

## Turning it on

```yaml
ssh:
  enabled: true
  # deny_ports: [5432]   # optional: more agent-local ports no tunnel or forward may reach
```

Then `lever apply`. On a scion with the controls, apply:

1. starts the hub with lever's conduit policy in its environment (not in `settings.yaml`, which
   agents can rewrite): SSH on, port `8462` (each agent's lever gateway, which acts as the agent)
   and `ssh.deny_ports` denied to every tunnel, port proxy and SSH forward, and SSH Unix socket
   forwarding off. scion sends the same policy to every agent it starts as
   `SCION_CONDUIT_POLICY`, and `sciontool` enforces it on SSH forwards, which the hub cannot see.
   Every apply also drops any `server.hub.conduit` block from the jail's `settings.yaml` (lever
   never writes one; an agent could, to open the relay's internal listener) and restarts the hub
   when it did;
2. reads the running hub's environment back and turns `hub.conduit` on as `lever-admin` only if
   the policy is there, keeping `hub.artifacts`, `hub.chat_thread_groups` and
   `federation.project_visitors` off;
3. restarts the hub so the relay starts, reads the policy back again, and checks that the hub
   serves conduit. If any of that fails, it turns conduit off again, restarts the hub, and the
   apply fails;
4. grants lever's controller role `agent.port_access`, so `lever ssh` can mint its tunnel token.

Agents started before conduit was on do not serve SSH: restart them (`lever stop` and `lever up`
for the manager). The *lever ssh* doctor row names any running agent started before conduit or
under another policy.

With `ssh.enabled` off (or removed), the next apply pins conduit off, restarts the hub if it was
on, takes `agent.port_access` back from the controller role, and revokes any tunnel token a
`lever ssh` left behind. On a scion with the controls, the hub always starts with the policy, with
SSH off.

## Using it

```bash
lever ssh manager            # a shell in the manager (or: lever ssh, the app name works too)
lever ssh w1 -- git status   # a remote command in worker w1
```

`lever ssh` runs your `ssh` client with no ssh config of yours (`-F none`) and a ProxyCommand of
`lever ssh --proxy`, which runs `scion tunnel --stdio --ssh` inside the jail. Each connection:

- checks that ssh is on, that the jail's scion has the controls, that lever turned conduit on, and
  that the running hub carries lever's policy;
- mints its own hub token as lever's controller, valid for 12 hours, for the instance project
  only, with `agent:attach`, `agent:port_access`, `agent:read` and `project:read`. The token
  reaches the jail on the tunnel's stdin, never a command line, and is revoked when the
  connection ends. Its id is kept in `.lever-state/ssh/tokens/` until then, so an apply with ssh
  off revokes it if the process was killed;
- pins the agent's SSH host key, as the hub reports it, in `.lever-state/ssh/known_hosts` under
  `lever.<name>.<agent>`, and connects with `StrictHostKeyChecking yes`.

`lever ssh` refuses while the state directory is inside the tree.

## VS Code

```bash
lever ssh-config     # or: lever ssh-config manager w1
```

prints one `Host lever.<name>.<agent>` block per agent, with the same options, and then, as
comments, the VS Code settings these hosts need:

```json
"remote.SSH.localServerDownload": "always",
"remote.autoForwardPorts": false,
"remote.forwardOnOpen": false,
"remote.restoreForwardedPorts": false,
"remote.SSH.enableAgentForwarding": false,
"remote.SSH.enableX11Forwarding": false
```

The jail's egress keeps the agent from downloading VS Code's server, so VS Code downloads it on
your machine and copies it over (`localServerDownload`). The rest keep VS Code from opening
forwards between the agent and your machine on its own. Put the blocks at the **top** of
`~/.ssh/config`, above any `Host *` block, and exclude lever's hosts from every `Host *` block
(`Host * !lever.*`). ssh takes the first value it reads for most options, so a `Host *` block
above lever's (with `ForwardAgent yes`, say) would win over them; and it adds up `RemoteForward`,
`LocalForward`, `SendEnv` and `IdentityFile` from every block that matches, so a `RemoteForward`
in a `Host *` block below would hand the agent's end a way into your machine. Then open the host
with *Remote-SSH: Connect to Host*.

## What it exposes, and what it does not

- **Only the host operator.** The tunnel token is minted per connection from lever's assertion
  key on the host. The remote proxy refuses every `/api/v1/conduit` route for every web login,
  with ssh on or off, so neither an operator's phone nor a contact can open a tunnel or an SSH
  session. Agents cannot open user tunnels: scion admits only user principals there.
- **A shell is the agent's own user.** An SSH session runs as the agent's container user, with
  its environment, its files and its loopback, lever's gateway on `8462` included. `deny_ports`
  and the socket switch restrict *forwarding* only: they keep an SSH or VS Code port forward, a
  `scion tunnel --port` and the hub's port proxy away from the gateway and the agent's sockets,
  so nothing on your machine ends up holding the agent's capabilities. They are not a boundary
  against what you run in the shell.
- **The agent controls its end.** Treat the session like `lever attach`: a hostile agent can
  show you anything, and it chose the host key the hub reports (it can even serve the session
  itself). lever's client options turn off agent and X11 forwarding, local commands, connection
  sharing, sending your environment and every authentication but none (no key, agent or password
  is offered); interactive `lever ssh` also reads none of your ssh config and clears every port
  forward. With `lever ssh-config` your own `Host *` blocks still apply (see above): keep forwards
  out of them for lever's hosts. VS Code is different: its remote server runs in the
  agent, and VS Code trusts it (extensions run there, and it can ask the client to forward ports
  or open links). Open a hostile agent's workspace in VS Code only as you would open an untrusted
  repository.
- **Per-agent policy is set at start.** An agent enforces the policy it was started with. After
  changing `ssh.deny_ports`, restart the agents; the hub enforces the new ports on tunnels and
  the port proxy at once.
