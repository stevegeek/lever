---
title: "Remote access"
nav_order: 5.8
---
# Remote access: talking to the manager from your phone

None of lever's existing operator channels work away from the host: `lever attach`, `lever msg
send`, and `lever directive send` all shell into the jail or use a local UDS. `lever remote`
closes that gap by exposing the Scion hub's web UI — agent list, chat, a quick-message dialog, an
xterm.js terminal — to your Tailscale tailnet, through a small host-side reverse proxy that injects
credentials so the phone never has to hold one.

```
phone browser
  │  HTTPS (Tailscale TLS cert, tailnet-only reachability, identity headers)
  ▼
tailscale serve (host)
  │  http://127.0.0.1:<remote.port>
  ▼
lever remote proxy (host, loopback-only)
  │  + Authorization: Bearer <remote PAT>   (injected, every request)
  │  + Cookie: scion_sess=…                 (injected, UI shell only)
  │  – client Authorization/Cookie headers  (stripped)
  │  origin checks, audit log
  ▼
jail hub web port (guest 127.0.0.1:8080, --enable-web)
```

The proxy also runs a **local OIDC provider** on its own loopback port, which is how the browser
gets a hub session without an external identity provider and without dev-auth. See [how the
browser is logged in](#how-the-browser-is-logged-in) before you enable this.

## Accepted posture — read this before enabling it

Whoever can reach the proxy through your tailnet gets the same interactive power over the jail's
agents that you have from the phone — full chat, full attach, everything the injected PAT's scopes
allow (see [below](#the-remote-pat)). What they do **not** get is anything past the jail boundary:
the blast radius of a compromised tailnet device is the jail interior (agents, the mounted project
tree via the UI's exec-backed surfaces, LLM spend), never the host. The security invariants this
feature does not change:

- No agent gains any capability: the egress firewall, broker mTLS surfaces, and jail mounts are
  unchanged. Only a host-side inbound path for the human is added.
- The phone never holds a hub credential; the PAT stays host-side.
- Directives remain the only authenticated operator override (signing key + host UDS, unreachable
  from this path).

A message you send from your phone lands as an ordinary unauthenticated `user:` turn, like a
message typed at `lever attach` or sent with `lever msg send`. Remote access is a new transport,
not a new authority; for a provably-operator instruction use
[operator directives](/operator-directives/).

## Setup

### 1. Enable it in `lever.yaml`

```yaml
remote:
  enabled: true                                # default off — opt in
  base_url: "https://myhost.tailxxxx.ts.net"    # REQUIRED whenever enabled — see note below
  # port: 8445                                  # optional; defaults to 8445
  # login_port: 8447                            # optional; defaults to 8447 (the OIDC provider)
  # allowed_users: []                           # optional identity pinning — see below
```

Validation at load: `base_url` must be an absolute `https://` URL. `port` and `login_port` may not
equal 8443, 8444, 8446, or each other. `port` may not appear in `manager.allow_ports`: that grant
would let a jailed agent dial the proxy, set `Tailscale-User-Login` itself, and receive the injected
PAT.

**`base_url` is required whenever `enabled: true`.** The proxy matches every request's
`Origin`/`Sec-Fetch-Site` against the host `base_url` resolves to; with `base_url` unset, that host
is empty and the proxy's fail-closed default would refuse **every** request, Origin-bearing or not
— a proxy that could never serve anything. Rather than let that config "succeed" into a proxy that
403s 100% of traffic, `lever.yaml` fails to load at all: `remote.enabled: true` with no `base_url`
is rejected at config load, before `apply`/`serve`/`status`/`doctor` ever run.

#### The tailnet URL never reaches the hub

`base_url` never reaches the Scion hub. This is deliberate, and it is not an oversight to "fix" by
forwarding it: Scion's `--base-url` flag is not a web-only setting. The hub also adopts it as its
own **agent-facing endpoint** and injects it into every agent container as `SCION_HUB_ENDPOINT` /
`SCION_HUB_URL`. A jailed agent cannot reach a tailnet name — there is no DNS for it inside the
jail, and lever's egress policy drops the `100.64/10` range — so every agent would be handed a hub
address it can never call back on, breaking status updates, notifications, and the roughly
ten-hourly agent token refresh.

Worse, the damage is silent. Scion rewrites a **loopback** hub endpoint to the container-reachable
`host.containers.internal` form, which lever's pasta `--map-host-loopback` mapping makes work. A
non-loopback endpoint skips that rewrite entirely, so passing the tailnet URL removes the very step
that makes agent callbacks work. lever therefore starts the hub with `--enable-web` alone, leaving
its endpoint on loopback, and keeps the public origin on the host side where the proxy needs it.

The web UI does not miss it: the Scion SPA builds every URL relative to the page it was served
from, and `--base-url`'s only other consumers — the session cookie's `Secure` flag and the OAuth
redirect URI — are unreachable here, since the proxy strips the hub's session cookie from every
response and injects a PAT instead of ever running an OAuth login.

**Requires the `orbstack` backend.** Not because of how the proxy reaches the hub — it dials
*through* the jail, the rule every other lever hub call follows, and that needs no guest→host
forwarding on any backend. The Lima path is not validated.
`remote.enabled: true` on any other backend is rejected at config load with a clear error.

**How the proxy reaches the hub.** The hub binds `127.0.0.1:8080` **inside the jail**. The proxy
runs `nc 127.0.0.1 8080` in the jail and treats that child process as the connection, so the
address it dials is the guest's, not the host's. A host-side `127.0.0.1:8080` that appears to reach
a hub is an artifact of OrbStack's port forwarding: it lands on whichever machine claimed the port,
which need not be this instance's.

**Several remote-enabled instances on one host is supported.** Each proxy reaches its own jail, so
instances never contend for a shared host port. Each one binds **two** host loopback ports, though
— the proxy (`port`) and the login provider (`login_port`) — so a second instance must set both,
and `lever.yaml` can only catch a collision *within* one instance:

```yaml
# instance B's lever.yaml
remote:
  port: 8455
  login_port: 8456
```

```sh
tailscale serve --bg --https=443  http://127.0.0.1:8445   # instance A
tailscale serve --bg --https=8443 http://127.0.0.1:8455   # instance B
```

Each instance's `base_url` must name the host **and port** its own mapping serves — the proxy
matches every request's `Origin` against exactly that.

**The jail must be up, but not before the proxy.** The proxy resolves its jail on the first
request, not at startup, so starting it before the machine is running is fine. A dial while the
jail is down fails that one request with the reason — recorded in `.lever-state/remote.log` and in
the audit log's `error` field — and the next request retries; no restart is needed once `lever up`
has run. A hub that accepts the connection but never answers is bounded too, so a wedged machine
gives you a `502` with a recorded cause rather than a request that hangs forever. The transport
needs `netcat-openbsd` in the guest; lever declares it alongside `curl` in the jail's prereqs, and
the dial error names it and the `apt-get` line if it is ever absent.

### 2. Make sure the host has node

The web UI's assets are built **on your host**, at `apply` time, and staged into the guest. You
need **node >= 20 and npm** on `PATH` — nothing in the guest, and nothing permanent: the build
output is cached, so this only costs you the first apply per pin.

```sh
node --version    # must print v20 or newer
```

A `scion.version:` pin ships **no** web assets: upstream tracks only `web/dist/client/.gitkeep`
and `.gitignore`s the built output, so a binary compiled from the fetched module serves Scion's
*"Web UI Not Available"* page. The fetched Go module does carry the full npm project, so lever
builds it from the same source tree the binary was compiled from and starts the hub with
`--web-assets-dir` pointing at the result.

The build runs host-side for the same reason scion itself is cross-compiled host-side: the guest
carries no toolchain, and giving it one would also mean giving the jail npm's registry egress.

Where things go, and how big they are:

| What | Where | Size |
| --- | --- | --- |
| Host build cache | `~/Library/Caches/lever/scion-web/<digest>/` (macOS), `~/.cache/lever/scion-web/<digest>/` (Linux) | **~210MB per pin** (`node_modules` is ~185MB of it) |
| Staged into the guest | `/usr/local/share/scion/web` | **~3.3MB** |

The cache is keyed by a digest of scion's web sources, so an unchanged pin is a no-op on re-apply
(milliseconds, no npm) and a changed pin builds once. A `source:` checkout is keyed the same way,
by content, so editing the SPA rebuilds it and nothing else does.

**The host cache grows by one directory per distinct scion pin, and lever never prunes it**:
several remote-enabled instances on one host share the cache, and pruning unused pins would make
instances on different pins rebuild each other's output on every apply. Deleting the cache
directory, or any `<digest>` inside it, is always safe; the next apply rebuilds what it needs.

Staging excludes `*.map` (vite sourcemaps), which is most of the build output.

`lever doctor` checks this, and `lever apply` fails by name if node is missing or broken. The most
common cause is an asdf/mise shim on `PATH` whose version is not installed (exit 126, no text); put
a real node ahead of the shim:

```sh
export PATH="$HOME/.asdf/installs/nodejs/<ver>/bin:$PATH"
```

**`scion.binary:` is exempt.** With a prebuilt binary lever has no source to build the SPA from and
does not try — and does not pass `--web-assets-dir`, so whatever you embedded in that binary keeps
serving. If you build your own scion and want the UI, build it with its assets embedded
(upstream's `make all`), or switch to `version:`/`source:`.

### 3. Apply

```sh
lever apply
```

This mints a narrow **remote PAT** (see [below](#the-remote-pat)) and starts the proxy as a
daemonized child, the same lifecycle pattern as the broker: `apply`/`up` start it, `lever stop`
stops it alongside the rest of the instance.

If this is the **first** time `remote.enabled` has been on for this instance — including flipping
it on later, after the instance was already bootstrapped — `apply` briefly reopens the same
jail-loopback-only, dev-auth-on mint window used for the [controller PAT
re-mint](/security-model/worker-isolation/): a throwaway hub, reachable only from inside the jail,
up just long enough to mint the token, then torn down. On a fresh bootstrap with `remote.enabled`
already set, this happens in the *same* window as the controller-PAT mint — no extra window opens.

### 4. Front it with Tailscale

```sh
tailscale serve --bg --https=443 http://127.0.0.1:8445
```

(substitute your configured `remote.port` if you set one; `lever remote status` prints the command
with the proxy port filled in; it always shows `--https=443`, so adjust the HTTPS port yourself for a
second instance). This is the **only** Tailscale-side command lever
asks you to run — lever does not manage Tailscale itself. It needs **MagicDNS and HTTPS certificates
enabled** for your tailnet (Tailscale admin console → DNS), since `tailscale serve --https` is what
gets you a real cert for the `*.ts.net` hostname `base_url` should name.

### 5. Check it

```sh
lever remote status
lever doctor
```

`status` reports proxy liveness, the serve URL (or the `tailscale serve` command to run if it
looks unset), and whether the PAT is present — never its value. `doctor` goes further: when
`remote.enabled`, it confirms the proxy process is alive **and** actually listening, that
`remote.pat` exists at `0600`, does an end-to-end `GET /healthz` **through** the proxy — proving
the loopback listener, the origin/identity gates, PAT injection, and the hub itself are all wired
correctly, not just that a process happens to be running — and checks the login path from both
ends: that the provider serves discovery and still answers 404 at `/authorize`, and that the HUB,
asked from inside the jail to start a login, redirects to lever's dead authorization endpoint. That
last one is the end-to-end proof: the hub has to fetch the provider's discovery document through
the guest forwarder before it can answer, so a 302 means the `oidc_login` block, the forwarder and
the provider are all working. (The hub caches discovery for an hour, so it proves the chain worked
at the first login since the hub started, not that it is reachable this second.) A disabled `remote:` block is always a
pass; most instances never turn this on.

## How the browser is logged in

The injected PAT opens the hub's **API**. It cannot open the hub's **UI shell**: Scion's web layer
authenticates a browser by one thing only, a `scion_sess` cookie, and never reads `Authorization`.
Without dev-auth (which lever keeps off) and without an external identity provider (which lever
deliberately does not add), a browser would sit at a 401 forever.

lever closes that with a **local OIDC provider** it runs itself, and a login it performs
**server-side**:

```
lever remote serve (host process)
  ├── the proxy            127.0.0.1:<port>        ← your browser, via tailscale serve
  └── the OIDC provider    127.0.0.1:<login_port>  ← the hub's back channel, via the jail
                                                     forwarder (guest 127.0.0.1:8446)

first UI request for an operator
  1. proxy GETs the hub's /auth/login/oidc, keeping its own cookie jar, and does NOT follow the
     redirect. It reads state / redirect_uri / client_id out of it.
  2. proxy mints an authorization code BY CALLING A FUNCTION. No HTTP, no endpoint, no redirect.
  3. proxy GETs the hub's own callback with that code, carrying the jar.
  4. the hub calls the provider's /token and /userinfo, and answers with a session cookie.
  5. the proxy keeps that cookie host-side and injects it into UI requests from then on.
```

Two things follow, and both are load-bearing.

**The session never widens what the phone can do.** The cookie is attached to UI-shell requests
only. Everything under `/api/v1` keeps riding the narrow remote PAT — Scion's own middleware passes
a request straight through when it carries an `Authorization` header — so the API surface stays
exactly the four scopes [below](#the-remote-pat), whatever the session's hub user may be allowed to
do. Nothing about this changes what the browser holds either: the cookie stays on the host, and the
hub's `Set-Cookie` is still stripped from every response.

**There is no endpoint that mints a session.** This is the part to understand before enabling
remote access, because Scion's OIDC login path validates *nothing*: it never requests or parses an
`id_token`, never fetches JWKS, uses neither PKCE nor a nonce, needs no client secret, and does not
check that the discovery document's issuer matches the one configured. Whatever the provider says
at `/userinfo`, the hub believes. So the security of this rests on exactly one property — **an
authorization code can only be created by an in-process call inside the host-side proxy**, at the
same trust level as the `remote.pat` file sitting beside it — and lever holds that property by
having **no authorization endpoint at all**:

- `/authorize` is a registered route that returns **404, unconditionally and permanently**, and
  every hit is written to the audit log. It exists as a route precisely so that its absence cannot
  read as an oversight somebody later "finishes".
- Discovery still advertises an `authorization_endpoint`, because Scion refuses to start a login
  without the field. It advertises `https://lever.invalid/authorize` — a host that cannot resolve —
  and never dials it.
- `lever doctor` checks both: that the provider serves discovery, and that `/authorize` still
  answers 404.

That matters because the provider is **reachable from inside the jail**. The hub only accepts a
loopback issuer (it validates `issuer_url` at startup and refuses to start otherwise), so a
logic-free forwarder in the guest listens on the guest's `127.0.0.1:8446` and carries bytes to the
provider's host port — and lever maps guest loopback into every agent's network namespace, so every agent can
reach that forwarder. What an agent finds there is discovery, `/token` and `/userinfo`, and none of
them yields anything without a code. An agent can even start a login against the hub and get a
state cookie; it cannot mint a code, so it can never finish. Had `/authorize` been implemented, that
same agent could have obtained a hub session for any identity it cared to assert.

**Who the hub thinks you are.** The provider asserts the Tailscale login the proxy already verified
against `allowed_users`, so the hub's user row names that identity, and two operators get two
sessions rather than sharing one. With `allowed_users` unset there is no verified identity to
assert and a placeholder is used instead — another reason to set it. lever never sets
`admin_emails`, so the hub creates these users at its ordinary `member` role.

**Turning this on restarts the hub, once.** Scion reads the `oidc_login` block at startup only, so
the first `lever apply` after enabling remote access rewrites the guest's `~/.scion/settings.yaml`
and restarts the hub so it is read. Later applies find the block already correct and restart
nothing.

**Turning it back off removes the forwarder.** Every `lever apply` with `remote.enabled: false`
stops the guest forwarder, deletes it, and drops the `oidc_login` block. That is not tidiness: the
forwarder is an unauthenticated TCP bridge from guest loopback — which every agent can reach — to a
host loopback port sitting beside lever's own 8443/8444/8445. Left running after the feature is
gone, it would keep that port bridged into the jail for whatever binds it next. No hub restart is
needed to undo the block; a stale one names a provider that no longer answers.

**Two port numbers, not one.** The hub dials a GUEST loopback port — fixed at `8446`, with no
config key — and the forwarder there carries the bytes to the provider on the host's `login_port`.
They must differ, because the container runtime **mirrors a guest listener onto the host at the
same number**: with one number for both halves, the guest forwarder's mirror took the host port and
the provider could not bind it. (Worse, it was order-dependent — a provider that bound first left
the runtime unable to mirror, and the whole thing worked by luck.) Only the host number is
configurable, so no configuration can make the two halves of one instance collide; `lever.yaml`
additionally rejects a `login_port` of `8446`, which is the host port that mirror occupies.

**The login port is granted in the jail's egress allowlist**, and only while remote access is on.
The forwarder's whole job is to reach a host port, and that allowlist is the one thing deciding
whether a jail→host dial succeeds — without the grant the dial is dropped, the hub's discovery
fetch hangs, and the browser gets a 502 with every process apparently healthy. The grant is one
port, added beside the broker's, and it disappears from the next rebuild when remote access goes
off.

> **On `egress: closed`, enabling remote access on a RUNNING instance needs `lever destroy` +
> `lever up`, not just `lever apply`.** A live closed chain is never rebuilt in place (that would
> briefly open egress under a running agent), so a newly granted port does not take effect until
> the instance is brought up again. `lever doctor` names this when it sees it.

**A Go toolchain is needed on the host** while remote access is on: the guest forwarder is
cross-compiled for the guest's architecture at apply time. A `scion.version:`/`scion.source:`
instance already needs one; `scion.binary:` mode is newly affected, and for that combination
`lever.yaml` refuses to load at all when `go` is not on PATH — the apply-time failure would
otherwise land after the bootstrap-token step had already opened a mint window against the hub.

## `allowed_users`: pinning who can connect

```yaml
remote:
  allowed_users:
    - you@github     # your Tailscale login, exactly as it appears in the admin console
```

When non-empty, the proxy requires the `Tailscale-User-Login` header — set by `tailscale serve`,
never trusted from anywhere else — to match one of these entries, and refuses the request (403)
otherwise. This is defense against other members of your tailnet, not against a compromised device
of your own: if your own phone is compromised, its Tailscale identity is exactly what an attacker
would present too. Leave it empty to allow anyone who can reach the proxy through your tailnet at
all (the default — often fine for a single-operator tailnet).

## Never use `tailscale funnel`

Publish the proxy with `tailscale serve`, which is reachable only from your tailnet. `tailscale
funnel` has the same command shape and publishes to the public internet; lever cannot refuse a
funnelled request, because it arrives like any other. The proxy injects a credential on every
forwarded request, so an unauthenticated stranger would get the same interactive power over the
jail interior as your phone. `allowed_users` is not a backstop: it is empty by default, and a
funnelled request carries no Tailscale login header. The host stays VM-protected either way; this
is exposure of the jail interior.

## The asset build runs on the host

With `remote.enabled: true`, `lever apply` builds Scion's web UI **on the host** (`npm ci &&
npm run build`), outside the jail, as the user running lever — so it executes whatever install
and build scripts that dependency tree defines, with your filesystem access. The inputs are
pinned: the source is the Scion commit `scion.version` names, fetched through the Go module proxy
and checksum-verified, and `npm ci` installs from its committed lockfile. That bounds *which*
code runs, not *what it can do*. If that is not a trade you want, `scion.binary` mode skips the
build entirely (and serves no web UI).

## Audit log

Every request the proxy handles — allowed or denied — is appended as one JSON line to
`.lever-state/remote-audit.jsonl`: timestamp, the Tailscale login if present, method, path, the
decision (`allow` / `deny-host` when the `Host` header does not match `base_url` / `deny-origin` /
`deny-user` / `deny-no-pat` / `deny-no-session`), and the
upstream status once known. The login path writes there too: `oidc-session` when a session is
obtained for an operator (`oidc-session-failed` when it is not), `oidc-discovery` / `oidc-token` /
`oidc-userinfo` for each call the hub's back channel makes (`-refused` variants when the provider
refuses one, `oidc-not-found` for an unknown path), and `deny-authorize` for anything that probes `/authorize` — nothing legitimate
ever does, so a line like that is either a misconfiguration or something in the jail looking around.
No line ever carries a cookie, a code, a token or the PAT. A request that never got an answer from the hub also carries an `error` field naming the
cause — a stopped jail, a missing `nc`, a hub refusing the connection — since a bare `502` cannot
tell those apart. The hub does not log this traffic; check this file first if doctor's healthz probe
fails or an agent received a message you do not recognise.

## The remote PAT

`apply` mints a Scion hub token scoped to exactly `agent:read`, `agent:list`, `project:read`,
`agent:attach` — enough for the full interactive surface (chat, transcript, attach) but nothing
that creates, deletes, or reconfigures an agent, and nothing that reads a project secret. It's
persisted host-side only, `.lever-state/remote.pat`, mode `0600`, and is a **different** token from
the controller PAT the broker itself uses.

**Expiry is not checked by lever.** The hub's default token lifetime is 90 days; lever has no
cheap way to read a token's remaining lifetime back from the hub, so `lever doctor` cannot warn you
before it lapses. If remote access starts
failing auth for no other visible reason, re-mint by deleting the file and re-applying:

```sh
rm .lever-state/remote.pat
lever apply
```

This is the same repair shape as the controller-PAT re-mint: a brief, jail-loopback-only dev-auth
window, agent-free, that mints a fresh token and nothing else.

## What this does NOT do

- **No lifecycle or fleet management from the phone.** Worker dispatch stays a manager action —
  asked for in chat, like any other task — not a button the phone UI exposes as an operator
  capability of its own.
- **No new authority.** A remote chat message is an ordinary unauthenticated `user:` turn (see
  the accepted posture above).
- **No credential on the phone.** The remote PAT and the UI session cookie stay on the host; the
  hub's `Set-Cookie` is stripped from every response.
- **No identity provider, and no endpoint that mints sessions.** The local OIDC provider has no
  authorization endpoint at all — see [how the browser is logged
  in](#how-the-browser-is-logged-in).

## See also

- [Config reference](/reference/config/) — the full `remote:` key table.
- [CLI reference](/reference/cli/) — `lever remote serve` / `lever remote status`.
- [Security model: worker isolation](/security-model/worker-isolation/) — the controller-PAT
  bootstrap-token window this feature reuses the shape of.
- [Operator directives](/operator-directives/) — the one channel that IS authenticated, if you need
  that instead of (or alongside) chat.
