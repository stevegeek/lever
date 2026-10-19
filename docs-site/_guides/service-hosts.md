---
title: "Service hosts"
nav_order: 5.86
parent: Tools
permalink: /service-hosts/
---
# Service hosts: MCP servers on other machines

A **service host** is another machine on your tailnet that runs some of an instance's MCP servers,
and keeps their credentials. The broker on the lever host reaches it over mutual TLS. Use one when
a tool, or the credential it needs, belongs on another machine: a mail or calendar server on the
laptop that is signed in, a database client next to the database, a token you do not want on the
lever host.

```
agent (jail, in the VM)
  │  mTLS, capability token          (unchanged: the jail reaches only the broker)
  ▼
broker (lever host)                  checks the capability, strips the token
  │  mutual TLS, both sides pinned   (tailnet, only the lever host may connect)
  ▼
lever service-host serve (service host)
  │  checks its own ceiling, adds the credentials
  ▼
stdio MCP server (a child process)  or  local streamable-HTTP MCP server
```

**What stays on the lever host:** the VM and the jail, the broker and its CA, the capability
signing key, every grant and policy decision, the LLM key and the `/llm` proxy, and the broker's
audit log. The service host never talks to the jail and never sees a capability token.

**What moves to the service host:** the MCP server processes, their credentials (environment
variables and files only the service host reads), and a second, independent check of which tools
and operations this instance may call: the *ceiling*.

Each tool lives on one host. An instance may use several service hosts; while one is down, only
its tools fail.

## Threat model

The service host is built so that losing any one machine costs as little as possible.

- **A hostile agent** reaches only the broker, as before. The jail's egress drops the tailnet
  ranges (`100.64.0.0/10` and IPv6 ULA) in every posture, and even a peer that reaches the
  service host's port is refused at the TLS handshake without the broker's client key, which
  lives in the host's `.lever-state` and never enters the VM. A call still needs a capability
  for the tool and operation, as for any broker tool.
- **A hostile tailnet or LAN peer** has no client key, so the service host refuses it during the
  handshake. It cannot pose as the service host either: the broker pins the service host's key.
  The tailnet ACL below keeps everyone but the lever host away from the port in the first place.
- **A compromised lever host** (the machine that runs the VM) holds the client key, so it can call
  the service host as the broker does. It gets **no more than the ceiling**: the service host
  refuses any tool it does not serve and any operation outside that tool's `allow` list, whatever
  the broker allowed. It never sees the credentials, which the service host adds to its own child
  processes or upstream requests. It can still call an allowed operation with any arguments, so
  keep each `allow` list to what the instance needs.
- **A compromised service host** gets **nothing in the jail**. It sees the tool calls the broker
  forwards (the operation, the arguments and, for its audit log, the calling agent's name), and it
  answers them. It never gets a capability token (the broker strips it), the client key (TLS never
  sends it), the broker's CA, an agent certificate or the LLM key. Nothing on the lever host
  accepts a connection from it: the broker only dials out. What it can do is what any tool can do:
  answer with false or hostile content, which reaches the agent as a tool result, or refuse and
  delay its own tools. Treat a service host's tool output as you treat any tool's output.

The service host is a second ceiling, not a replacement for the broker's grants. The broker still
decides which agent may call which operation, with which argument constraints; the service host
only bounds what the whole instance may ever reach there.

## Pairing and auth

Both sides use a self-signed key pair and pin the other's public key by its SHA-256 fingerprint
(`sha256:` and 64 hex digits, over the certificate's SubjectPublicKeyInfo). No private key is ever
copied: only the two fingerprints move between the machines, and neither is a secret.

- The **service host's server key** is made on the service host by `lever service-host init`. Its
  fingerprint goes into the instance's `lever.yaml`, as the service host's `fingerprint`.
- The **broker's client key** is made on the lever host by `lever service-host pair <name>`, one per
  service host, in `.lever-state/service-hosts/<name>/` (`client.key` is `0600`). Its fingerprint
  goes into the service host's config, as `client_fingerprint`.

Both sides require TLS 1.3. The service host asks for a client certificate during the handshake
and refuses any whose key does not match `client_fingerprint`; the broker refuses a server whose
key does not match `fingerprint`. Host names are not checked: the pin is the identity, so a
MagicDNS name or an IP address both work, and a DNS answer cannot redirect the broker to another
machine. Certificate dates are not checked either; to replace a key, pair again.

### One-time setup

On the **service host** (install `lever` there; it is the same binary):

```bash
lever service-host init ~/lever-service-host/service-host.yaml
# writes server.key (0600) and server.crt next to the config, and prints:
#   server fingerprint: sha256:3f1c…
```

On the **lever host**, in the instance root, declare the service host with that fingerprint, then
pair:

```bash
lever service-host pair laptop
# writes .lever-state/service-hosts/laptop/client.key (0600) and client.crt, and prints:
#   client fingerprint: sha256:9a07…
```

Back on the **service host**, put that fingerprint in `client_fingerprint`, then start it:

```bash
lever service-host serve ~/lever-service-host/service-host.yaml
```

Run `lever apply` (or `lever reload`) on the lever host, then `lever doctor`: its *service host
laptop* row should be green.

`pair` keeps an existing key and prints its fingerprint again; `pair --rotate` makes a new one.
The broker reads the key again whenever its files change, so after a rotation update
`client_fingerprint` on the service host and restart `serve` there; calls fail with `503` in
between. `init` never replaces a server key: delete `server.key` and `server.crt` yourself to make
a new one, then update `fingerprint` in `lever.yaml` and run `lever apply`.

## Config

### On the service host

The service host reads only its own file. Keep it, and every file it names, writable by you alone:
`serve` refuses a config, certificate or key that another user owns (root aside) or that group or
others can write, and a key they can read. Keep the directories that hold them yours alone too: the
checks are on the files. A secret file (`env_files`, `header_files`) is read, and refused on the
same terms, each time a stdio server starts or an upstream call is made, so a rotated secret needs
no restart of `serve` (a running stdio server keeps the value it started with).

```yaml
# service-host.yaml
listen: 100.101.102.103:7443          # this machine's tailnet address
client_fingerprint: sha256:9a07…      # from `lever service-host pair` on the lever host
# cert: server.crt                    # defaults, next to this file
# key: server.key
# call_timeout: 5m

tools:
  - name: mail                         # a stdio MCP server, run as a child process
    command: [/usr/local/bin/mail-mcp, --stdio]
    env:
      MAIL_ACCOUNT: work
    env_files:                         # the value is the file's content
      MAIL_TOKEN: /Users/me/.config/mail-mcp/token
    allow: [search_messages, read_message]

  - name: notes                        # an MCP server already listening on this machine
    upstream: http://127.0.0.1:3901/mcp
    header_files:
      Authorization: /Users/me/.config/notes/bearer
    allow: ["*"]
```

| Key | Meaning |
|---|---|
| `listen` | `host:port` to listen on, with a literal IP. Use the machine's tailnet address (or loopback behind your own front). A wildcard (`0.0.0.0`, `::`, or no host) is refused unless `allow_wildcard_listen: true`. |
| `client_fingerprint` | Required. The broker's client key, from `lever service-host pair`. Any other client is refused during the handshake. |
| `cert`, `key` | The server certificate and key `init` wrote. Default `server.crt` and `server.key` next to the config; relative paths resolve there. |
| `call_timeout` | How long one call may take before the service host answers `504`. Default `5m`. |
| `tools[].name` | The tool name, the same as the tool's `name` in the instance's `broker.tools`. Served at `/mcp/<name>`. |
| `tools[].command` | A stdio MCP server: the program and its arguments. Exactly one of `command` and `upstream`. |
| `tools[].dir` | Working directory of the `command`. Default: the config's directory. |
| `tools[].env`, `tools[].env_files`, `tools[].pass_env` | The child's environment: `env` values, the content of each `env_files` file (trailing newline removed), and the named variables copied from the service host's own environment. The child starts from `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG` and `TMPDIR` only, so one tool never sees another's credentials. |
| `tools[].upstream` | A streamable-HTTP MCP server's URL (`http://` or `https://`). Every request goes to exactly this URL. |
| `tools[].headers`, `tools[].header_files` | Headers the service host adds to every request to `upstream` (for its credential); `header_files` values are file contents. |
| `tools[].allow` | Required. The ceiling: the MCP tools (operations) this instance may call. `["*"]` allows every operation the server offers. |
| `allow_wildcard_listen` | Allows a wildcard `listen`, which also listens on every other network the machine is on. |

A stdio server runs once, on the first call, and is shared by every call; the service host starts
it again on the next call after it exits. The service host initializes it itself and keeps no MCP
session of its own, so an agent never has to re-initialize after a restart of either side.
Requests the server makes back to the client (sampling, roots) are refused, and its notifications
are dropped. Each server runs in its own process group: when it exits, or `serve` stops, whatever
it started is stopped with it. After a server fails to start, calls fail at once for two seconds
instead of starting it again each time.

The service host forwards only the message fields MCP defines (for `tools/call`, the tool's
`name`, `arguments` and `_meta`), re-encoded from what it checked, and refuses with `400` a message
with a repeated key, or two keys equal under Unicode case folding (`params` and `paramſ`), as the
broker does: a server written in Go matches keys that way, and would otherwise read another call
than the one checked. A stdio server that does not read a call within `call_timeout` is stopped,
and started again on a later call.

`tools/list` answers list only the operations in `allow`. A `tools/call` for any other operation is
refused with `403` before it reaches the server. Only `initialize`, `ping`, `tools/list`,
`tools/call` and `notifications/initialized` are served; any other request gets JSON-RPC "method
not found", any other notification is accepted and dropped, and every HTTP method but `POST` gets
`405`. Any other path gets `404`.

### On the lever host

```yaml
# lever.yaml
broker:
  service_hosts:
    - name: laptop
      address: laptop.example-tailnet.ts.net:7443
      fingerprint: sha256:3f1c…        # from `lever service-host init`
  tools:
    - name: mail
      host: laptop                     # instead of command or backend
      operations:
        - {name: search_messages}
        - {name: read_message}
    - name: notes
      host: laptop
      gate: coarse
manager:
  obtain:
    - {tool: mail, op: search_messages}
    - {tool: notes, op: "*"}
```

| Key | Meaning |
|---|---|
| `broker.service_hosts[].name` | The name tools refer to (`host:`), and the directory of its client key. Lowercase letters, digits and `-`. |
| `broker.service_hosts[].address` | `host:port` of the service host's `listen`. A name or an IP; the pin, not DNS, authenticates it. |
| `broker.service_hosts[].fingerprint` | The service host's server key, from `lever service-host init`. |
| `broker.tools[].host` | The service host that runs this tool. |

A tool with `host:` is an external tool: it implies `external: true` and `allow_non_loopback: true`,
and takes `gate`, `operations` and `allowed_values` as any external tool does. Config load refuses a
`host:` tool that also sets `command`, `backend` or `allow_non_loopback`, a `host:` that names no
declared service host, and two service hosts with the same name or address. The
broker refuses to start while a service host that a tool uses is not paired, or its client key is
readable by group or others, and refuses service hosts altogether while the state directory is
inside the tree (an agent could read the client key there); `pair` refuses that too.

The broker sends each call to `https://<address>/mcp/<tool>`, presenting the client key, and sets
`X-Lever-Caller` to the calling agent so the service host's audit log can name it; the service host
uses it for nothing else.

## Tailnet ACL

Only the lever host should reach the service host's port. With Tailscale, tag both machines and
grant just that port:

```json
{
  "tagOwners": {
    "tag:lever-host": ["autogroup:admin"],
    "tag:lever-service": ["autogroup:admin"]
  },
  "grants": [
    {"src": ["tag:lever-host"], "dst": ["tag:lever-service"], "ip": ["tcp:7443"]}
  ]
}
```

Keep any broader rule from also admitting other devices to `tag:lever-service:7443`. The mutual
TLS refuses them anyway; the ACL keeps them from trying. Do not run `tailscale serve` or `funnel`
on that port: the service host terminates TLS itself.

## Failure modes

| What happens | What the agent sees | What is logged |
|---|---|---|
| The service host is down, asleep or off the tailnet | That call gets `503` (`tool <name>: backend unavailable; try again later`). Other hosts' tools keep working. The next call after it is back works: each call dials again if needed. | `broker.log`: one `gateway.backend_unavailable` line per tool per minute, with the address and the error. |
| The service host restarted | Nothing: the service host keeps no sessions. For an `upstream` tool, a restarted upstream's own `404` for an unknown session reaches the client, which opens a new session. | |
| A stdio server exited | Calls in flight get `503`; the next call starts it again. | Service host log: the exit. |
| A call ran past `call_timeout` | `504`. | Service host log. |
| An operation outside the ceiling | `403`; the server never sees the call. | Both: the broker's `allow` line, then `deny` on the service host and `service_host.refused` in `broker.log`. |
| The service host's key changed (re-`init`, or a machine pretending to be it) | `503`, as for a down host. | `broker.log`: the certificate pin mismatch. *service host* doctor row fails. |
| The broker's key changed (`pair --rotate`) and the service host was not updated | `503`. | Service host log: the refused client fingerprint. Doctor row fails and prints the fingerprint to set. |
| Not paired yet | The broker does not start; `lever apply` names the service host. | |

## Doctor

`lever doctor` adds one row per service host, *service host &lt;name&gt;*. It dials the address,
completes the mutual TLS handshake with the broker's client key (checking the pin), and reads the
service host's list of the tools it serves for this instance, with each tool's ceiling. The row
fails when the host is unreachable, not paired, the pin does not match, or the service host refuses
the client key, and when a `host:` tool in `lever.yaml` is not served there. It warns when an
operation in a tool's `operations` (gate `fine`) lies outside that tool's ceiling, since every call
to it would be refused, and when no tool uses the service host.

## Logs

`lever service-host serve` logs to its standard error, one line per decision:
`decision=allow|deny tool=<name> op=<operation> caller=<agent>`, plus refused handshakes with the
client fingerprint they offered, server starts and exits, failed calls and timeouts, and each stdio
server's own standard error, line by line. Run it under your service manager (launchd, systemd) to
keep the log. The service host itself never logs arguments, results or credentials; what a stdio
server prints on its standard error is that server's choice.
