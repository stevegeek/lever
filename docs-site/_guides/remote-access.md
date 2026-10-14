---
title: "Remote access"
nav_order: 5.8
---
# Remote access: talking to the manager from your phone

None of lever's existing operator channels work away from the host: `lever attach`, `lever msg
send`, and `lever directive send` all shell into the jail or use a local UDS. `lever remote`
closes that gap by exposing the Scion hub's web UI — agent list, chat, a quick-message dialog, an
xterm.js terminal — to your Tailscale tailnet, through a small host-side reverse proxy that injects
credentials so the phone never has to hold one. Tailscale is the default front; any other
authenticating HTTPS front that sets a verified identity header works too (see
[non-Tailscale fronts](#non-tailscale-fronts), with exe.dev as the worked example).

```
phone browser
  │  HTTPS (Tailscale TLS cert, tailnet-only reachability, identity headers)
  ▼
tailscale serve (host)
  │  http://127.0.0.1:<remote.port>
  ▼
lever remote proxy (host, loopback-only)
  │  + Cookie: scion_sess=…                 (your hub session, injected, every request)
  │  – client Authorization/Cookie headers  (stripped)
  │  origin checks, credential-mint refusal, audit log
  ▼
jail hub web port (guest 127.0.0.1:8080, --enable-web)
```

The proxy gets the browser's hub session itself, by signing in to the hub with a short-lived
assertion signed by a key only the host holds; the hub runs in scion's proxy auth mode and accepts
no other sign-in. There is no external identity provider and no dev-auth. See [how the browser is
logged in](#how-the-browser-is-logged-in) before you enable this.

## Accepted posture — read this before enabling it

Whoever can reach the proxy through your tailnet gets the same interactive power over the jail's
agents that you have from the phone — full chat, full attach, everything the
[`lever-remote` role](#the-remote-web-role) allows the signed-in hub user. What they do **not** get is anything past the jail boundary:
the blast radius of a compromised tailnet device is the jail interior (agents, the mounted project
tree via the UI's exec-backed surfaces, LLM spend), never the host. The security invariants this
feature does not change:

- No agent gains any capability: the egress firewall, broker mTLS surfaces, and jail mounts are
  unchanged. Only a host-side inbound path for the human is added.
- The phone never holds a hub credential; the session stays host-side, and the proxy refuses the
  hub routes that would put a token in a response body.
- Directives remain the only authenticated operator override (signing key + host UDS, unreachable
  from this path).

A message you send from your phone lands in the agent's session as text, like a message typed at
`lever attach` or sent with `lever msg send`. With `allowed_users` set, the agent checks it
against a host record: see [verified web chat](#verified-web-chat). A verified message is your steering, not a new
authority; for a provably-operator instruction use [operator directives](/operator-directives/).

## Setup

### 1. Enable it in `lever.yaml`

```yaml
remote:
  enabled: true                                # default off — opt in
  base_url: "https://myhost.tailxxxx.ts.net"    # REQUIRED whenever enabled — see note below
  # port: 8445                                  # optional; defaults to 8445
  # allowed_users: []                           # optional identity pinning — see below
```

Validation at load: `base_url` must be an absolute `https://` URL. `port` may not equal the
broker's ports (8443 and 8444 by default). `allowed_users` may not name an address reserved for
lever's own hub users (`lever-admin@lever.local`, `lever-controller@lever.local`) or scion's dev
user (any `@localhost` address). `remote.login_port`, from older levers, still loads but does
nothing and draws a warning. `port` may not appear in `manager.allow_ports`: that grant
would let a jailed agent dial the proxy, set the identity header (`Tailscale-User-Login` by default)
itself, and ride the injected session.

**`base_url` is required whenever `enabled: true`.** The proxy matches every request's
`Origin`/`Sec-Fetch-Site` against the host `base_url` resolves to (an `Origin` must be exactly
`https://` and that host, the host compared without case); with `base_url` unset, that host
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
response, lever configures no OAuth login, and the proxy runs the login itself, host-side, against
the hub directly (see [how the browser is
logged in](#how-the-browser-is-logged-in)).

**Both backends.** `remote.enabled: true` loads on `orbstack` and on `lima`. The proxy dials the hub
*through* the jail, the rule every other lever hub call follows, and that needs no guest→host
forwarding on any backend. The Lima path is not yet live-validated; see [Lima](#lima) for what
differs there and what to check.

**How the proxy reaches the hub.** The hub binds `127.0.0.1:8080` **inside the jail**. The proxy
runs `nc 127.0.0.1 8080` in the jail and treats that child process as the connection, so the
address it dials is the guest's, not the host's. A host-side `127.0.0.1:8080` that appears to reach
a hub is an artifact of OrbStack's port forwarding: it lands on whichever machine claimed the port,
which need not be this instance's.

**Several remote-enabled instances on one host is supported.** Each proxy reaches its own jail, so
instances never contend for a shared hub port. Each one binds one host loopback port, the proxy's
(`port`), so a second instance must set its own, and `lever.yaml` can only catch a collision
*within* one instance:

```yaml
# instance B's lever.yaml
remote:
  port: 8455
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

**On a release lever with the scion pin of its release** (`b41cd6fe`, the examples' pin) you can
skip this step: lever downloads the web UI that release ships, checks it against the sha256 built
into `lever`, and stages it, so the host needs neither node nor Go. Only a failed download builds
it, as below. See [prebuilt scion](/reference/config/#prebuilt-scion).

For any other pin, a `source:` checkout, or a lever built from a checkout, the web UI's assets are
built **on your host**, at `apply` time, and staged into the guest. You
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

This invites each login's hub user and binds the [remote web role](#the-remote-web-role) and its
project-create ceiling before anyone signs in, mints the **remote PAT** (see
[below](#the-remote-pat)), and starts the proxy as a daemonized child, the same lifecycle pattern
as the broker: `apply`/`up` start it, `lever stop` stops it alongside the rest of the instance.

All of it runs against the live hub, as lever's own hub identities, while agents run: no container
has to stop. The first apply with remote access on (including flipping it on later) restarts the
hub once, to give it the web UI (see [how the browser is logged
in](#how-the-browser-is-logged-in)); agents keep running across that restart.

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
the loopback listener, the origin/identity gates, the session login, and the hub itself are all wired
correctly, not just that a process happens to be running. The proxy signs in with a lever assertion
before it forwards, so a `200` proves the sign-in too. Three more rows read the hub itself:
`hub authentication` (an assertion from another key or for another hub is refused, `lever-admin` is
the admin, no login provider is offered, the running hub carries lever's auth settings, no dev
token exists), `hub identities` (`lever-admin` is the only admin, the controller holds only the
tokens lever recorded) and `remote web role` (each login is the hub user lever recorded, bound to
its tier's role, capped and not suspended; each removed login is suspended). See the
[CLI reference](/reference/cli/). A disabled `remote:` block is always a pass for `remote access`;
most instances never turn this on.

## How the browser is logged in

The proxy sends one credential, a hub web session, on every request. Scion's web layer
authenticates a browser by one thing only, a `scion_sess` cookie, and never reads `Authorization`;
its API layer turns the same cookie into a bearer token for `/api/v1`. Without dev-auth (which lever never runs) and without an external identity provider (which lever
deliberately does not add), a browser would sit at a 401 forever.

lever closes that by acting as scion's **authenticating proxy**. The hub runs in scion's proxy
auth mode with the JWT provider: it accepts a short-lived assertion that lever signs on the host,
and nothing else creates a browser session. The login is performed **server-side**:

```
lever remote serve (host process)
  └── the proxy            127.0.0.1:<port>        ← your browser, via tailscale serve

first UI request for an operator
  1. proxy signs an assertion for that login's hub email (ES256, header X-Lever-Hub-Assertion,
     30 s of life, iss lever:<instance-id>, aud scion-hub:<instance-id>, on the guest's
     clock, read through the jail for each sign-in; a failed read refuses the sign-in) and
     sends GET / to the hub with it, through the jail. It does not follow redirects.
  2. the hub verifies the assertion against the public key lever installed in the jail and
     answers with a scion_sess cookie: a session for that hub user.
  3. proxy calls GET /api/v1/auth/me with that cookie alone. The user ID must be the one lever
     recorded for that login in .lever-state/hub-identity.json; otherwise the login is refused.
  4. the proxy keeps that cookie host-side and injects it into every request from then on, UI
     and API alike. It logs in again after 12 hours (scion refuses a session cookie older
     than 24), and whenever the hub rejects the session.
```

The login path is `/` on purpose: scion creates a session only on its web pages, never under
`/auth/` or `/api/v1/`, where it checks the assertion but stores nothing. The assertion is sent on
step 1 only, and never on a forwarded request.

Two things follow, and both are load-bearing.

**One identity, narrowed in the hub.** The UI shell, the `/events` stream, the chat and every
`/api/v1` call — the PTY WebSocket included — run as the same hub user: yours. That is what the
chat needs, because scion's conversation model keys a direct conversation on the session user and
refuses anyone who is not a participant. (Until this release the proxy injected the remote PAT on
API calls, so the API ran as the PAT's owner, a different hub user, and every chat read and send was
a 403.) What that user may do is decided by the hub: the [`lever-remote`
role](#the-remote-web-role) on this project, the `hub-member` directory reads every hub user holds,
and an access constraint that takes `project.create` away. The proxy sends **no** `Authorization`
header, and strips any the phone sends: Scion's middleware lets a request that carries one straight
through without reading the cookie, so a forwarded header would let the phone choose who it is.

**The session never reaches the phone.** The cookie stays on the host and the hub's `Set-Cookie` is
stripped from every response. Because a session may mint a user access token, the proxy also
allowlists the hub's `/api/v1/auth/` routes — `me`, `admin-status`, `scopes`, `providers`,
`logout`, and the token page's list, read, delete and revoke — and refuses everything else there
before it reaches the hub (audit decision `deny-credential-mint`). That covers `POST
/api/v1/auth/tokens`, the CLI device and authorize flows, `/token`, `/refresh`, `/login`, the
Google token exchange, and any route a future scion adds. The web UI's token page still lists and
revokes tokens; it cannot create one.

**Some hub routes are never forwarded** (audit decision `deny-route`), whatever the hub would
decide:

- `/api/v1/system/*`, except `GET /api/v1/system/status` (the SPA reads it on load). Scion runs no
  role check on these routes. It only requires a local, non-hosted hub and a loopback caller, and
  the proxy reaches the hub from loopback inside the jail. Among them: the image registry, the
  super-admin's email, harness-config reset, image pull and build, and guest directory listing and
  creation. The web UI's onboarding and workstation-settings pages therefore do not work remotely.
- Project creation: `POST /api/v1/projects`, `/api/v1/projects/register`,
  `POST /api/v1/projects/<id>/clone`, and the legacy `/api/v1/groves` forms. Each one makes the
  caller the owner of a new project. Every hub user holds `hub-member`, which includes
  `project.create`; lever's ceiling on each remote user takes it away before the user first signs
  in, and the proxy refuses these routes as well, whatever the hub's state.
- `/auth/callback/*`. No lever login uses it, and through the proxy it could only churn logins.
- scion's conduit (`/api/v1/conduit*`, `/api/v1/admin/conduit*`: a shell, sftp and port relay
  into agents through the hub), artifacts (`/api/v1/artifacts*`, with its unauthenticated
  `shared/` and `view/` links) and chat thread groups (`/api/v1/thread-groups*`), and every write
  to `/api/v1/admin/experiments`, which switches them on. These experiments are off, and lever
  keeps them off (see [worker isolation](/security-model-worker-isolation/)); the proxy refuses
  them in any case. Reading `/api/v1/admin/experiments` stays admin-only at the hub. The
  federation token route (`POST /api/v1/auth/federation-token`) is refused with every other
  credential route under `/api/v1/auth/`.
- An agent's `ports/tunnel` (the agent side's own socket) and `delegations` (they hand the
  signed-in user's authority to an agent) under `/api/v1/agents/<id>/`, and the test-identity
  routes (`/api/v1/test-identit*`, `/auth/test-identity*`), which mint credentials or sessions.

These routes are matched in lower case and by prefix, so another spelling of the same route is
refused too.

The proxy also accepts `Sec-Fetch-Site` only as `same-origin` or `none`: a page on a sibling
tailnet name is `same-site` and is refused. There is one exception, a top-level page load of an
entry page. A link tapped in another app (Messages, mail), or the redirect back from a front's
login page, arrives as a `cross-site` navigation, and stays `cross-site` across every redirect
after it. The proxy admits such a request only when all of these hold:

- the method is `GET` or `HEAD`;
- it carries exactly one `Sec-Fetch-Mode: navigate` and exactly one `Sec-Fetch-Dest: document`
  (a frame says `iframe`, `frame`, `embed` or `object`; a fetch, a script or a WebSocket says
  another mode);
- `Sec-Fetch-Site` is `cross-site` or `same-site`, and there is no `Origin` header (a browser
  sends none on a `GET` navigation);
- the path is `/`, or `/lever/chat` while the [chat page](#the-chat-page) is on.

Neither page changes anything on `GET`. `/` is the chat page's fixed redirect, the hub's web UI
shell, or a contact's landing page; `/lever/chat` is a file from the lever binary. Every answer
to them forbids framing (`frame-ancestors 'none'` and `X-Frame-Options: DENY`), and a page on
another site cannot read what a cross-origin page shows. The requests the page makes after it
loads are same-origin, and meet the strict rule. The chat page marks no conversation read until
the user taps or types on it (or taps one of its notifications while it is open), so a link that
opens it, and picks a chat with `#agent=`, clears no unread badge. Every other cross-site request
stays refused: any `/lever/api/` or hub `/api/` route, every other hub page, every write, frames
and subresources. When the proxy refuses a top-level page load, it answers a small HTML page with
an "Open the chat" link to `/` instead of plain text, which a phone can offer as a download.

Before it forwards a request, the proxy removes every
client header that names an identity to the hub: `Authorization`, `Cookie`, `Tailscale-*`,
`X-Scion-*` (the agent token and the broker signature headers among them), `X-Forwarded-User-*`,
`X-Goog-IAP-JWT-Assertion`, `X-API-Key` and `X-Lever-Hub-Assertion`.

**Only lever's host key makes a session.** The hub's auth settings are pinned through the
`SCION_SERVER_*` environment lever starts it with (`auth.mode: proxy`, `user_access_mode:
invite_only`, `proxy.provider: jwt`, ES256, the header, the issuer and audience, the public key file
`/etc/lever/hub-auth.pub`, and `admin_emails: [lever-admin@lever.local]`). The environment beats
`~/.scion/settings.yaml`, so a write to that file from inside the jail does not change how the hub
authenticates. lever writes the same keys to the file for readers, and removes any `oidc_login`
block, because scion builds its OAuth login whenever that block is present, whatever the mode.

- The private key is `.lever-state/hub-auth.key` on the host (`0600`). The jail holds only the
  public half, root-owned, outside every home directory, so an agent cannot replace it. Nothing in
  the jail can sign an assertion, and no lever process listens in the jail.
- The proxy's signer signs only the hub emails of the configured logins (`allowed_users`, or the
  placeholder operator), lowercased, and never a reserved address (`lever-admin@lever.local`,
  `lever-controller@lever.local`, any `@localhost` address). A login that is not configured gets
  no assertion.
- The hub is `invite_only`: an assertion for an email the hub has no invited or active user for is
  refused (403). `lever apply` invites every configured login before the proxy is restarted with
  it (see [the remote web role](#the-remote-web-role)), and the proxy refuses a login that has no
  recorded hub user before it asks the hub.
- The proxy strips any client copy of `X-Lever-Hub-Assertion`, in any letter case, from every
  forwarded request.
- An assertion for another instance (`iss`/`aud`) or from another key gets a 401. `lever doctor`'s
  `hub authentication` row tests exactly that, by behaviour (see the
  [CLI reference](/reference/cli/)).

**Who the hub thinks you are.** The assertion names the Tailscale login the proxy already verified
against `allowed_users`, so the hub's user row names that identity, and two operators get two
sessions rather than sharing one. With `allowed_users` unset there is no verified identity to
assert and a placeholder (`lever-operator@lever.local`) is used instead, whatever the header says —
another reason to set it. These users are hub `member`s; the only hub admin is lever's own
`lever-admin@lever.local`, which signs in nowhere and holds no stored credential.

**Turning remote access on or off restarts the hub, once.** scion's workstation defaults keep the
hub's web frontend on either way; what remote access adds is `--web-assets-dir`, the SPA lever
stages for a scion it builds from `scion.source` or `scion.version`. lever restarts the hub when the
running hub's `--web-assets-dir` differs from the one it wants. (With `scion.binary:` lever stages
no assets, and the toggle restarts nothing.) Proxy auth stays on either way; nothing else changes
when remote access is off. Later applies find the hub as wanted and restart nothing.

**No login port and no forwarder.** The login needs no listener of its own: the proxy reaches the
hub through the jail like every other hub call, and the jail needs no egress grant for it. On a jail
an older lever brought up with remote access, the next `lever apply` stops and removes its guest
login forwarder (`/usr/local/bin/lever-login-forward`) and drops the `oidc_login` block; its
login-port ACCEPT drops out of the egress chain on the same apply, because every apply verifies the
chain against the ruleset it wants and rebuilds a chain that differs (atomically, also in the
`egress: closed` posture). `remote.login_port` still loads, does nothing, and draws a warning.

## `allowed_users`: pinning who can connect

```yaml
remote:
  allowed_users:
    - you@github     # your Tailscale login, exactly as it appears in the admin console
```

When non-empty, the proxy requires the identity header (`remote.identity_header`,
`Tailscale-User-Login` by default) — set by the front, never trusted from anywhere else — to match
one of these entries exactly, and refuses the request (403) otherwise. It also refuses a request
that carries the header twice, or a comma-joined value: that is what a front that appends to a
client-sent header produces, and picking the first value would let the client choose. This is defense against other members of your tailnet, not against a compromised device
of your own: if your own phone is compromised, its Tailscale identity is exactly what an attacker
would present too. Leave it empty to allow anyone who can reach the proxy through your tailnet at
all (the default — often fine for a single-operator tailnet).

## Non-Tailscale fronts

Any authenticating HTTPS front can replace `tailscale serve`, if it does three things: it
terminates TLS on `base_url`, it admits only users you granted access, and it puts the verified
login in a request header. Tell lever which header:

```yaml
remote:
  enabled: true
  base_url: "https://myvm.exe.xyz:8445"   # the front's public origin, port included
  identity_header: X-ExeDev-Email         # the header the front sets
  allowed_users: ["you@example.com"]       # compared exactly with that header
```

**What the trust rests on — confirm both for your provider.** The proxy believes the header
because nothing but the front can reach its listener. So:

1. **The front must OVERWRITE the header**, never append to a value the client sent. If it
   appends, the client picks the identity. lever refuses a repeated header and a comma-joined
   value, which covers the common append shapes, but a front that passes a client value through
   untouched cannot be detected — only your provider's documentation or a test can tell you.
   Test it: send a request through the front with the header already set to someone else's
   login, and check `.lever-state/remote-audit.jsonl` shows your own.
2. **Only the front may reach the listener.** With the default `bind: 127.0.0.1` that holds as
   long as the front runs on the host. A front that dials the host's external interface needs
   `bind` (below), and then the host firewall has to make it true.

The configured header is stripped before the request reaches the hub, like `Tailscale-*`.
lever refuses to take the login from headers a browser or any hop sets, or that scion reads as an
identity (`Authorization`, `Cookie`, `Host`, `Origin`, `X-Forwarded-*`, `Sec-*`, `X-Scion-*`, and
others — see the [config reference](/reference/config/#remote)), from Tailscale's user-chosen
`Tailscale-User-Name` and `Tailscale-User-Profile-Pic`, and from any header name with `_` (proxies
drop or rewrite those inconsistently). `allowed_users` entries that differ only in case are refused:
scion lowercases emails, so they would be one hub user.

**Who the hub thinks you are.** An `allowed_users` entry with an `@` is the hub email as it
stands. A front that sends an opaque user id instead (exe.dev's `X-ExeDev-UserID`) works too: an
entry without `@` becomes the hub user `<id>@id.lever.local`, an address nobody can receive mail
at. Prefer the email header when the front has one; the hub's audit then names a person.
Changing `identity_header` or the `allowed_users` form changes the hub users, and each new one
must sign in once before `lever apply` can bind its role (see [the remote web
role](#the-remote-web-role)).

**`bind`: a front that dials the host's external interface.** Some fronts reach the machine over
its network interface, not its loopback. Bind the one private address the front dials:

```yaml
remote:
  bind: 10.0.0.5     # an RFC 1918 / CGNAT / link-local / ULA address on this host
```

Anything that can reach that address can now set the identity header to any login and ride your
hub session. The host firewall must admit only the front to `<bind>:<port>`. lever accepts only
addresses the jail's egress rules drop in every posture, so no jailed agent can reach the proxy
that way; a public address is refused, because under open egress the jail could dial it. An IPv6
link-local address is refused (it cannot be listened on without an interface zone). With the
default `identity_header`, a tailnet address (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) is refused
too: any tailnet peer could connect to it directly, bypassing `tailscale serve`, and send
`Tailscale-User-Login` with any login. Use `tailscale serve` in front of the loopback bind.
The jail's egress chain is replaced atomically (per address family) on every `lever apply`, so there
is no window in which the private ranges are open. It is not persisted in the guest, though: after a guest reboot there is
no chain until the next `lever apply`/`up`, which applies it before it starts any agent.
`0.0.0.0`/`::` listens on every address, public ones included, and needs
`allow_wildcard_bind: true` as well; with the default `identity_header` it is refused outright,
because it listens on the tailnet address too. A non-loopback bind prints a warning on every `lever apply`,
`lever up` and `lever remote serve`, and `lever doctor` shows it as a `remote exposure` warning
row.

**`Host` vs `X-Forwarded-Host`.** The proxy's DNS-rebinding defence matches the request's
`Host` header against `base_url`'s host (port included when `base_url` has one), and also admits
`127.0.0.1:<port>`, `localhost:<port>` and, with a specific `bind` address (loopback ones such as
`127.0.0.2` or `::1` included), `<bind>:<port>`. A front that passes `Host` through works with any
bind. A front that rewrites `Host` to the address it dials works unchanged **only with a specific
bind address**: with `bind: 0.0.0.0`/`::` lever does not know which of the host's addresses the
front dialled and admits none of them, so such a front must pass `base_url`'s host through, or use
`trust_forwarded_host` with `X-Forwarded-Host`. A front that rewrites `Host` to some other IP
address (another port, or none) gets `deny-host` in the audit log. If that front passes the
browser's host in `X-Forwarded-Host`, set:

```yaml
remote:
  trust_forwarded_host: true
```

The proxy then judges `X-Forwarded-Host` (exactly one value; repeated or comma-joined values are
refused) instead of `Host`, but only on a request whose `Host` is an IP address. A DNS rebind
always makes the browser send the attacker's *name* in `Host`, so a rebound page that adds a forged
`X-Forwarded-Host` is still refused. A front that rewrites `Host` to a name other than `base_url`'s
host is not supported. Anything that reaches the listener directly can still send any
`X-Forwarded-Host` — as it can send any identity header — so this is off by default and warned
about like a non-loopback `bind`.

### Worked example: exe.dev

exe.dev forwards `https://<vm>.exe.xyz:<port>/` (ports 3000-9999) to the VM, requires an exe.dev
login from a user granted access to the VM, and adds `X-ExeDev-Email` and `X-ExeDev-UserID` to
each request it lets through.

```yaml
backend: lima
scion:
  binary: dist/scion-linux-amd64     # built with upstream's `make all`, so it embeds the web UI
remote:
  enabled: true
  base_url: "https://myvm.exe.xyz:8445"
  identity_header: X-ExeDev-Email
  allowed_users: ["you@example.com"]
```

- **Never make the port public** (`share set-public`). A public port has no exe.dev login in
  front of it, so no verified header either — the same rule as never using `tailscale funnel`.

> **Warning: the front must forward ONLY the proxy port.** exe.dev can forward any port in
> 3000-9999, and a front that forwards a port range to the VM's loopback also reaches lever's
> other host listeners: the broker's admin port (`8444`, unauthenticated), its jail port
> (`8443`). Anyone the front admits could then call them
> directly, and `allowed_users` does not apply there — it is checked only by the proxy. Grant
> and use only `remote.port`; if your front cannot be limited to one port, move lever's other
> listeners (`broker.jail_port`, `broker.admin_port`) outside the range it
> forwards.

- Start with the default loopback `bind`. If exe.dev's front reaches the VM on its private
  interface rather than loopback, the proxy logs nothing for your requests; bind that private
  address instead (a specific address, not `0.0.0.0`, so a Host rewritten to it is admitted),
  and firewall the port to exe.dev's front.
- Before relying on it, confirm the two properties above for exe.dev: that it overwrites
  `X-ExeDev-Email`, and what can reach the proxy port besides its front.
- `lever remote status` prints the identity header, bind address and any warnings;
  `lever doctor` probes the proxy with the first `allowed_users` entry in that header.

These settings have not been live-validated behind exe.dev yet.

## Lima

`remote.enabled` loads on the Lima backend, and nothing in the login path is specific to OrbStack:

- The proxy reaches the hub by running `nc 127.0.0.1 8080` inside the jail
  (`limactl shell <vm> nc …`), as on OrbStack. The sign-in takes the same route, so it needs no
  guest→host port and no egress grant.

**Linux hosts are often multi-user.** Every local account on the host shares its loopback, so any
of them can connect to `127.0.0.1:<remote.port>`, set the identity header and ride your hub session.
That has always been true of the loopback design; on a shared Linux host it matters. Run remote
access only on a host whose other local users you trust as much as the front's users.

**Not live-validated yet.** Until it is, check the login path on a real Lima VM with `lever
doctor`: the `hub authentication` row proves the hub accepts only lever's assertions, and the
`remote access` row that `/healthz` answers 200 through the proxy, which needs a sign-in.

## Never use `tailscale funnel`

Publish the proxy with `tailscale serve`, which is reachable only from your tailnet. `tailscale
funnel` has the same command shape and publishes to the public internet; lever cannot refuse a
funnelled request, because it arrives like any other. The proxy injects a credential on every
forwarded request, so an unauthenticated stranger would get the same interactive power over the
jail interior as your phone. `allowed_users` is not a backstop: it is empty by default, and a
funnelled request carries no Tailscale login header, so it rides the placeholder operator's session.
The host stays VM-protected either way; this
is exposure of the jail interior.

## The asset build runs on the host

With `remote.enabled: true`, `lever apply` builds Scion's web UI **on the host** (`npm ci &&
npm run build`), outside the jail, as the user running lever — so it executes whatever install
and build scripts that dependency tree defines, with your filesystem access. The inputs are
pinned: the source is the Scion commit `scion.version` names, fetched through the Go module proxy
and checksum-verified, and `npm ci` installs from its committed lockfile. That bounds *which*
code runs, not *what it can do*. If that is not a trade you want, `scion.binary` mode skips the
build entirely, and serves only the UI its binary embeds: build scion with upstream's `make all`
for one (see [`scion.binary:` is exempt](#2-make-sure-the-host-has-node)). The scion pin of a
lever release skips it too: the web UI comes prebuilt with the release, built by the same commands
in the release build and checked against the sha256 embedded in `lever`.

## Audit log

Every request the proxy handles — allowed or denied — is appended as one JSON line to
`.lever-state/remote-audit.jsonl`: timestamp, the identity header's value if present (the
`ts_login` field, whatever the header), method, path, the
decision (`allow` / `deny-host` when the `Host` header does not match `base_url` / `deny-origin` /
`deny-user` / `deny-credential-mint` / `deny-route` / `deny-no-session` / `deny-path` for a path
not in one spelling / `login-redirect` for an intercepted sign-in navigation / `deny-contact` when the
contact fence refuses a request / `chat-unavailable` when the [chat page](#the-chat-page) cannot
resolve your hub user / `remote-wake` and `deny-wake` for a [wake](#waking-a-worker), with a
`reason` word on a refusal, and `remote-wake-result` for the broker's late answer /
`operator-view` and `deny-operator-view` for the [operator's view of contact
conversations](#the-operators-view-of-contact-conversations), with `contact`, `agent` and `count`
fields and a `reason` word on a refusal / the `push-*` and `deny-push` lines of
[notifications](#notifications) / `file-upload`, `file-download` and `deny-file` for [files in the
chat](#files-in-the-chat)), and the upstream status once known. An `allow` line for a cross-site
or same-site [page load of an entry page](#how-the-browser-is-logged-in) carries the `reason`
`cross-site-navigation` or `same-site-navigation`. The login path writes there too: `hub-session`
when a session is obtained for an operator, `hub-session-failed` when it is not (the hub refused the
assertion, set no session cookie, or did not answer), and `hub-session-refused` when lever refuses
the login itself: no hub user is recorded for it, or the session the hub created belongs to another
hub user than the one lever recorded. A `hub-session-refused` line means `lever apply` has not run
since the login was added, or something renamed a hub user; `lever doctor`'s `remote web role` row
names which.
No line ever carries a cookie, an assertion or a token. A request that never got an answer from the hub also carries an `error` field naming the
cause — a stopped jail, a missing `nc`, a hub refusing the connection — since a bare `502` cannot
tell those apart. The hub does not log this traffic; check this file first if doctor's healthz probe
fails or an agent received a message you do not recognise.

## The remote PAT

`apply` mints a Scion hub token scoped to exactly `agent:read`, `agent:list` and `project:read`,
persisted host-side only, `.lever-state/remote.pat`, mode `0600`, and **different** from the
controller PAT the broker itself uses. It reads and nothing else: it is the controller's token, so
anything sent with it would carry lever's sender label.

**The proxy uses it for one read.** Every request from the browser rides your hub session (see
[how the browser is logged in](#how-the-browser-is-logged-in)). The proxy sends the remote PAT only
on its own read-only `GET`s of the project's agent list, which the [chat page](#the-chat-page)'s
agent list and the [contact fence](#contacts-chat-only-logins)'s name resolution share. The token
never enters the jail.

**Minted by the controller, while agents run.** The token is minted by lever's controller hub user
(`lever-controller@lever.local`), the same identity that mints the controller PAT, with a
lever-signed assertion against the live hub: no hub stop and no container check. It is minted only
while remote access is on; the first apply with it off revokes it and removes its files.

**Expiry and scopes are recorded, not asked.** The hub will not tell a token holder anything about
the token, so lever keeps a record beside it (`.lever-state/remote.pat.json`: scopes asked for,
scopes granted, expiry) and mints it for 360 days. `lever apply` re-mints it, saving the new token
and its record before it revokes the old one, when the record is missing (a token minted by an
older lever, on scion's 90-day default), when the scope set differs from the one this lever mints,
or with fewer than 30 days left; `lever doctor`'s `hub tokens` row reports the same. To re-mint for
a reason the record cannot see, delete the file and re-apply:

```sh
rm .lever-state/remote.pat
lever apply
```

## The remote web role

The whole web UI, API included, runs as the **hub user** the sign-in created (see
[who the hub thinks you are](#how-the-browser-is-logged-in)), and scion gives a new user no role on
any project. So `apply` also creates a custom project role, `lever-remote`, with `agent.read`,
`agent.list`, `project.read`, `agent.attach`, `agent.message` and `project.list`, which only the web
UI's project list needs (and `agent.lifecycle`, the start, stop
and suspend controls, on a hub that has that permission), and binds each `allowed_users` entry's hub user to it on this instance's project. With `allowed_users`
unset, it binds the placeholder `lever-operator@lever.local`.

**The web user cannot create projects.** Every hub user also holds scion's `hub-member` system
role, through the `hub-members` group scion re-adds on every boot. That role is mostly directory
reads (users, groups, templates, brokers, skills, role definitions, hub settings), which the web UI
uses, but it also carries `project.create`, and a project the web user created would be owned by
it, outside lever's controls. lever cannot unbind the group, so it puts a ceiling on the user
instead: a system-scope **access constraint** named `lever-remote-ceiling:<email>`, on that one
user, whose maximum permission set is `hub-member`'s permissions (read from the hub) minus
`project.create`, plus the `lever-remote` permissions. A constraint only ever takes permissions
away, and it applies to everything the user holds at every scope, which is why the role's own
permissions are inside it. A ceiling whose set has drifted is rewritten in place, in one update.
One whose subject is another hub user (the login's row was recreated) is replaced without a gap:
the new one is written first, under the user's second name `lever-remote-ceiling:<email>:b` (scion
keeps constraint names unique), and only then is the old one deleted, so for that moment the user
holds the intersection of the two. scion lets lever-admin delete a constraint only when it holds
every permission the constraint allows; a constraint it may not delete is first narrowed by the
permissions it lacks (narrowing needs no such authority) and then deleted, and one that still
cannot go stays with a warning, since it can only take permissions away. The ceiling is read from
the hub on every apply, so a `hub-member` permission a scion upgrade adds or reserves is picked up
by the next apply.

**Bound before the first sign-in, while agents run.** Every `lever apply` and `lever reload` runs a
`hub-identity` step against the live hub, as lever's own admin identity (`lever-admin@lever.local`,
asserted with the host key; it holds no stored credential). While remote access is on, for every
configured login (each `allowed_users` entry, or the placeholder operator with no `allowed_users`)
it:

1. invites the login's hub user if the hub has none (scion's `invite_only` admits only invited and
   active users);
2. writes the ceiling;
3. binds `lever-remote` (or, for a [contact](#contacts-chat-only-logins), `lever-remote-contact`) on
   this instance's project, and drops the other tier's binding.

The ceiling comes before the binding, so a tier move never leaves the user with more than either
tier allows. Only after this step does apply restart the proxy with the new login list. At the
first sign-in the hub activates that same invited row, keeping its ID, with the role and ceiling
already in force. No container has to stop, and nothing waits for a first sign-in.
`.lever-state/hub-identity.json` records each login's hub user ID, and
`.lever-state/remote-role.json` the role, its permission set, each user bound and each user's
ceiling.

**A login lever cannot vouch for is refused, alone.** If a login's hub user is one of lever's own
(the admin, the controller, scion's dev user), or is not the user ID lever recorded for that login
before, apply warns and leaves it unbound, and the proxy refuses that login (`hub-session-refused`);
every other login keeps working. A reserved address in `allowed_users` is refused at config load
while remote access is on; with it off no login is bound at all.

**Removing a login takes effect first.** Before anything else, `lever apply`, `lever reload` and
`lever up` (on every path, also when the manager is already running) write what each login may now
do to `.lever-state/remote-grants.json`. The running proxy reads that file on every request and
before every push, and re-checks every open request and stream about once a second: a login that
is no longer there, or whose tier, agents, `see`, `files` or `voice` changed, is refused at once
(`deny-revoked`), including the chat page, files, wake and voice, which the proxy answers from its
own session without asking the hub, and its open `/events` streams and terminal connections are
closed. Every other login keeps working, and the proxy restart at the end of the apply admits the
login again under its new settings. A change to a setting every login shares (the front settings,
the chat page, `agent_messages`, `files`, `voice`, push) refuses every login until that restart. An
unreadable or missing file refuses every login. A `lever up` that finds the manager running or
suspended applies nothing else, so it neither suspends at the hub nor restarts the proxy: run
`lever reload` for that. Then, as the first thing it does at the hub, the apply
suspends the removed login's hub user and drops its role bindings (the ceiling stays). The hub
refuses a suspended user's web session at once, and its API access within the hub's 60-second user
cache. Nothing an agent can do to the hub, nor a later step failing, delays either. Adding the
login back reactivates the same hub user and binds it again. Turning remote access off stops the
proxy before any other step and suspends every login (none can sign in then); turning it back on
reactivates and binds them before the proxy starts.

`lever doctor`'s `remote web role` row reads the hub, not the record: each configured login's hub
user is the one lever recorded, bound to its tier's role, capped and not suspended, and each removed
login's hub user is suspended.

## Verified web chat

An agent cannot tell who wrote a message from the message. Every message lever delivers (a
worker's relay, a manager message, your `lever msg send` note, a directive notice) reaches the
agent with the same envelope sender, `from: user:<the controller's hub user>`, because lever sends
them all with its controller token. Anyone whose words reach an agent (a contact in the web chat,
an email, a web page, a tool result, a worker) can type a lever marker such as
`[lever: operator note]`, or a whole fake envelope. So neither the envelope nor the text decides
who wrote a message. Two host records do, and no agent can write either of them:

- **The chat ledger** (`.lever-state/chat-ledger/`, one 0600 file per login in a 0700 directory).
  When the hub accepts a message you post into an agent's direct chat through the proxy, the
  proxy appends one line: your verified login and its tier, the agent, the hub's message id,
  sender and time, and the text. Only the proxy writes it, and agents never reach the proxy.
- **The sent ledger** (`.lever-state/sent-ledger/`, one 0600 file per recipient and kind). The
  broker records every message lever sends, before it sends it: the recipient, who it is from
  (`manager`, `worker:<slug>`, `operator-note` or `directive-notice`), the exact text, and a fresh
  128-bit id. It writes that id on the message's first line as `ref=<id>`, after the marker. A
  send the broker cannot record is not made.

**What the agent does.** Before it acts on any message whose `from` starts with `user:`, the agent
calls its `message_verify` tool (`chat_verify` in 0.27 agent images) with the envelope's
`timestamp` and `from`, and the `ref` from the first line when there is one. The broker answers
from the host records only, and only about messages to that agent (its certificate identity):

- A `from` that is one of your remote logins is looked up in the chat ledger only. A `from` that is
  the controller's hub user (the broker asks the hub for it) is looked up in the sent ledger only,
  by the ref. Any other `from` has no record. So the sender only chooses which record is read: a
  contact's post can never come back as a lever message, and the reverse.
  The controller's hub user is `lever-controller@lever.local`, so lever's messages carry
  `from: user:lever-controller@lever.local`. Only the current address verifies: when the broker
  misses on a `user:` label it asks the hub again (at most every 30 seconds), but a message whose
  envelope carries an earlier address stays unknown. On an instance an older lever brought up, the
  first apply renames the controller (it was scion's dev user, `<os-user>@localhost`), so a
  message delivered before that upgrade and verified after it answers `none` (unknown sender);
  agents verify on receipt, so only messages in flight across the upgrade are affected.
- The answer's `result` is `web` (a person in the web chat: login and tier), `lever` (lever sent
  it: its kind), `already_verified`, `none` or `unavailable`, with the text as recorded. The agent
  acts on that text, never on the text in its session. The markers stay in the message to make a
  session readable, but they decide nothing.
- A reply to a web post goes where the chat ledger says: the answer carries `reply_to`, the
  poster's hub email as a scion `@<email>` reference, which is the agent's direct chat with that
  user. The skills send a reply there only, never to a conversation named in the session, so an
  envelope typed into a message cannot move the reply to another person's chat.
- A message verifies once. A repeat check by the same agent within ten minutes answers as a
  repeat without text, so a retry cannot lose it; after that it answers `already_verified`. A web
  post verifies within one hour of posting, a lever message within 24 hours of sending (an agent
  in a long turn can read a relay late).
- `none` means no host record names the message for that agent: the agent treats it as data (it
  does not act on it or reply) and says so in its session. `unavailable` means the broker could
  not answer (a rate limit, a record it could not read, the hub): the agent retries once after
  `retry_after`, then treats the message as data and says so. If the manager reports failed
  verifications you did not cause, look at the `verify` lines in `broker.log`.

**When it is on.** The sent ledger is always on while the state directory is outside the `tree:`.
Web chat verification needs remote access with `allowed_users` too. `lever doctor` shows the rows
`verified chat` and `sent ledger`. Run `lever init` after you change `remote` or `allowed_users`:
it writes "verified web chat is on" into both skills.

**Remote access without `allowed_users`.** The proxy then verifies no login and records no chat,
so an agent cannot verify any web chat post and treats every one as data: it does not act on it
and does not reply. List your login in `allowed_users` to use the web chat.

**Who is the operator.** A plain login in `allowed_users` gets the tier `operator`. Do not add a
login as an operator only so that someone can look at the web UI: once verified, that person's
chat is operator steering for every agent. For anyone else, use a contact (below).

**`lever msg send` goes through the broker.** The note goes to the broker's operator socket
(`.lever-state/operator.sock`, mode 0600), and the broker records it as an `operator-note` before
it sends it. With no broker running, `lever msg send` refuses and says so; type into the session
with `lever attach` instead. The socket carries the host user's authority, the same as `lever
attach`: any process running as you can send a note. A note is never a directive.

**Web pages from agents are sandboxed.** The hub serves workspace files, attachments and other
agent-written files inline under `/api/`. The proxy adds `Content-Security-Policy: sandbox` to
every `/api/` answer, so such a page cannot run script with your session or post chat as you.

What is not covered:

- Only messages into an agent's direct chat are recorded as web chat. A topic thread, the
  quick-message dialog, an edited message and email are not recorded, so they never verify. Text
  typed into an attached terminal has no envelope and needs no verification.
- A verified message is not a directive. An action that needs a directive still needs one, and a
  compromised front account or phone can send verified chat.
- A choice you tap in the web UI that sends agent-suggested text is recorded as sent by you.
- Both ledgers keep the text of recent messages on the host. Each file rotates at 1 MiB (one old
  copy kept), and a sent-ledger file untouched for 48 hours is removed when the broker starts.
- An injected envelope that copies a real ref gets back the real message lever sent, and uses it
  up early (the genuine envelope then answers as a repeat). It cannot get text lever did not send
  to that agent.

## The chat page

The hub's web UI is an operator console: projects, agent lists, graphs, settings. To talk to your
agents from a phone, most of that is in the way. Set

```yaml
remote:
  landing: chat
  labels_file: workers/labels.json   # optional: short labels the manager writes (see below)
```

and `lever apply`. The origin then opens on a page lever serves itself: the list of agents your
login has, and a chat with each one you may message.

### What each login sees

The proxy builds the list for each login on the server, from `lever.yaml`; the page shows what it
is given and nothing else.

- **An operator** sees the manager and every configured worker, and may message all of them.
  The page also has two links: **Terminal** (per agent) opens the hub web UI's terminal for that
  agent, for the times you need Claude Code itself; **Console** opens the hub's full web UI,
  which stays at its own paths (`/agents`, `/chat`, ...). Only `/` leads to the chat page.
- **A [contact](#contacts-chat-only-logins)** sees the agents of its entry: those in `agents:` it
  may message, those in `see:` it may only see (name, label and state, greyed as "view only";
  no history, no input). An agent in neither list never appears: not its name, its id, or its
  conversation. A contact gets no Terminal and no Console link.

The list comes in this order: the manager (when the login has it), then the agents it may
message in config order, then the see-only agents. Each row shows the agent's name, then its
label (`deal-2 · Via Roma 12`; the name always comes first), a state, and a count of unread
messages from it. On a screen 720 px wide or more, the list and the open chat show side by side;
on a phone, one at a time, with **Back** to return to the list.

### States

A state is one of a fixed set of words; an agent reports its own activity to the hub, so read it
as a hint. The composer of a chat follows it:

| State | You can send | The page says |
|---|---|---|
| running (working / waiting / idle) | yes | — |
| starting | yes | "starting – your message waits until it runs" |
| asleep (suspended), a worker | yes | "asleep – your message wakes it" |
| stopped, a worker, for an operator | yes | "asleep – your message wakes it" |
| stopped, a worker, for a contact | no | a button **Ask the manager to start &lt;name&gt;** |
| error, no record, not fresh, a worker | no | a button **Ask the manager to start &lt;name&gt;** |
| the manager not running | no | "the assistant is offline" |
| unknown (the hub cannot be read) | no | "state unknown – retrying" |

A contact never wakes a stopped worker: suspended is the state an idle worker sleeps in, but
stopped is a decision the operator made, and only the operator's login undoes it from the page.
"Not fresh" is for a contact only: the agent's session started before the skills on disk now
(see [Contacts](#contacts-chat-only-logins)), so the fence would refuse the contact's message.
**Ask the manager to start** opens your chat with the manager with an editable draft ("Please
start deal-3 for me."); nothing is sent until you press Send. A contact who may not message the
manager gets no button.

### Waking a worker

A message to an asleep worker wakes it first: a suspended one for any login that may message it,
a stopped one only for an operator. The page keeps your text,
asks lever to wake the worker, shows "waking &lt;name&gt;…", and reads the list every 3 seconds
for up to 90. When the worker runs, the message goes through the normal send, under the same
idempotency key it was given before the wake, so a reload during the wake cannot store it twice.
When the wake is refused or the worker is not running after 90 seconds, the page says why and
keeps the text.

The wake (`POST /lever/api/agents/<name>/wake`) is accepted only:

- for a worker, never the manager;
- from a login that may **message** that worker (an operator, or a contact with the worker in
  `agents:`); the same 403 answers a see-only agent, a hidden one, the manager and a name that
  does not exist;
- from the page itself: one `Origin` header that is exactly `https://` and the host of
  `base_url`, and `Sec-Fetch-Site: same-origin` when the browser sends it (the push and upload
  routes use the same rule);
- while the worker is suspended, or, for an operator's login, stopped (the broker checks this
  again, with the login's tier, before it resumes anything);
- once a minute per worker, across all logins (a second wake inside the minute gets 429 with
  `Retry-After`).

The proxy asks the broker to resume the worker through the broker's operator socket, the same
resume the manager's `lever-manager agent resume` runs (ticket staging included). That socket
exists only while the state directory is outside the tree; without it a wake answers 503 and the
page says waking is not available. Each wake is a line in the [audit log](#audit-log)
(`remote-wake` or `deny-wake` with a reason word; `remote-wake-result` when the broker's answer
comes after the page's).

### Labels

A label is a short text the manager keeps beside a worker's name, for example the address a deal
worker is about. The manager writes them to the file `labels_file` names, inside the tree:

```json
{"deal-2": "Via Roma 12", "data-scout": "comps, Rome centre"}
```

The proxy reads the file on the host through the tree, at most every 10 seconds. It must be a
regular file of at most 16 KiB, with no symbolic link anywhere on its path. Names that are not
configured agents are ignored. Each label loses its control and format characters, runs of spaces
become one, and it is cut to 60 characters; an empty label is dropped. A missing file means no
labels. A file that breaks a rule (too big, not JSON, not an object of strings, a symbolic link)
also means no labels, and `lever doctor` shows a `chat labels` warning row that names the fault.
The page writes a label as text, after the name, never in its place.

**Who sets the labels.** Whoever can write `labels_file` sets the text a contact sees beside each
agent's true name, the manager's row included. By default that is the manager, which owns its
part of the tree. So `lever.yaml` refuses a `labels_file` equal to or inside any worker's `dir`
(that worker could then write what a contact reads next to every name), and one inside `.lever`
or the state directory (host-written files). Keep the file in the manager's own part of the tree,
and treat a label as the manager's words, not lever's: it can say anything up to 60 characters,
but never replaces the name.

### What it is, and is not

- **The same conversations.** A chat goes into the agent's one session, the one `lever up`
  attaches your terminal to for the manager. The page shows your messages and the agent's replies
  (the final text of each turn). It does not show tool calls; the terminal does.
- **The same message path.** The page posts to the hub's own chat route, through the proxy, like
  the hub's web UI does. So the proxy records each message in the chat ledger, and the agent
  verifies it with `message_verify` (see [Verified web chat](#verified-web-chat)). Nothing about
  signing, recording or delivery is new. This is why `landing: chat` needs an operator login in
  `allowed_users`.
- **The fence still decides for a contact.** The page uses only hub routes the contact fence
  already admits (its own conversations with its listed agents, their read markers, its own chat
  events). The list hides what a contact may not see, and the fence refuses it anyway.
- **Nothing to build.** The page is a few small files inside the lever binary: no framework, no
  build step, no outside origin. It holds no credential, like every page behind the proxy.
- **Text only.** A reply is agent-written text on your origin, so the page writes it as plain
  text and its Content-Security-Policy allows no inline script. Markdown shows as typed.
- **A display, not a proof.** The page shows what the hub stores. Which bubble a message is in
  comes from the hub's record of its sender, but the words inside an agent's bubble are the
  agent's own and can imitate anything, a system notice or a line of yours included. What proves
  a message is yours is `message_verify` on the agent's side, not the page.

The proxy answers these itself and forwards none of them to the hub: `GET /` (a redirect to
`/lever/chat`), `/lever/chat`, its `.css` and two `.js` files, the app manifest and icons,
`GET /lever/api/agents` (the list), `POST /lever/api/agents/<name>/wake`, the operator's
`/lever/api/contacts` routes ([below](#the-operators-view-of-contact-conversations)), and, while
their feature is on, the push routes and `/lever/sw.js` ([Notifications](#notifications)) and
`/lever/api/files/` ([Files in the chat](#files-in-the-chat)). Anything else under `/lever/` is a
404; `/lever/api/chat`, which the one-agent page read, is gone. With `landing` unset
or `console`, none of this exists, every path goes to the hub as before, and a contact gets the
fence's own landing page.

The page reads the open conversation again when the hub's event stream says something changed,
and reads the list every 15 seconds while it is visible and after each chat event, so a missed
event costs a short delay, not a lost reply. When you read a chat while it is in view, the page
moves your hub read marker to the newest message (the hub's own read route), and the agent's
unread count drops. After a `lever up --fresh` (or a worker's fresh start) the agent has a new
hub record; the open chat moves to the new conversation by itself, and a message you were about
to send is not posted to the old one.

Off by default, each with its own key: [messages that an agent starts](#messages-agents-start) to
a contact (`remote.agent_messages`), [notifications](#notifications) while the page is closed
(`remote.push`), [files](#files-in-the-chat) (`remote.files`) and
[dictation](#dictation-and-read-aloud) (`remote.voice`, which also turns on read-aloud). With
files off, an operator uses the Console for files.

## Contacts: chat-only logins

A contact is an external person (a client, a domain expert) who answers an agent's questions in
chat. A contact gets no operator authority, no terminal and no start or stop, and reaches only
the agents its entry lists:

```yaml
remote:
  allowed_users:
    - operator@example.com                       # tier operator, every agent
    - login: contact@example.com
      tier: contact
      agents: [research]                         # required: declared workers or the manager
      see: [scout]                               # optional: shown on the chat page, never messaged
```

`see:` lists agents the contact may only see on the [chat page](#the-chat-page): name, label and
state, no history and no input. Config load refuses a `see:` name that is not the manager or a
configured worker, a name in both `agents:` and `see:`, and `see:` on an operator entry. `see:`
grants no hub route: the fence below admits nothing for a see-only agent.

- **Hub role.** A contact's hub user gets the project role `lever-remote-contact`, which holds
  `agent.message` and nothing else, plus the same project-create ceiling as an operator. It
  cannot read agent details (their tasks and configuration), list agents, follow the project's
  events or open a terminal. Moving a login from operator to contact removes its
  `lever-remote` binding at the next grant.
- **Proxy fence.** scion binds roles to a whole project, not to named agents, and its direct
  chat checks only the user half of a conversation. So the proxy allows a contact only an
  allow-list: its own conversations with its listed agents, its own identity, the web UI's
  static files and public settings, and its own chat and notification events. Other pages show
  a lever page with links to the contact's conversations; with `landing: chat`, they redirect
  to the [chat page](#the-chat-page) instead, and the contact gets neither that landing page nor
  the hub's web UI shell. Everything else is refused (audit `deny-contact`).
- **What a contact may not send.** A word that starts with `@` (scion would route the message to
  another agent), a reply to a message by id, an attachment, or any field other than the text, its
  idempotency key and `offer_wake` (the hub's web chat sends it on every message; it only changes
  how the hub answers when the agent is not running). `wake` is refused: a contact wakes a worker
  only through the chat page's wake, which checks the session and limits the rate.
  The proxy answers 403 with the reason; the contact rewrites the message. The text itself is not
  filtered: a contact may write anything, a lever marker included.
- **Markers cannot make chat pass for lever.** The agent verifies the message, and the broker
  answers from the chat ledger: the contact's own words, tier `contact`, whatever they say.
- **Contacts need current skills, in the running sessions too.** An older skill trusts a lever
  marker, and a contact can type one. So `lever apply`, `lever up` and `lever remote serve`
  refuse contact logins until `lever init` has written this version's skills for every agent, and
  while the state directory is inside the tree. The disk is not enough: a resumed session keeps
  the skill text it read before. Lever records each fresh session start (a manager created by
  apply, a worker created by the broker; never a resume) with the skill on disk then, in
  `.lever-state/sessions.jsonl` (0600, a host record like the ledgers). The fresh start `lever up`
  makes on its own for a manager with no conversation to continue is the exception: the agent can
  bring it about, so it is recorded only when the skill on disk (read before the delete and again
  right before the create) is the one the manager's last recorded session started with. Otherwise
  lever records a session matching no skill, which refuses contacts until the manager next starts
  fresh other than on its own (`lever up --fresh`); a manager with no record gets none. The proxy
  lets a contact
  post to an agent only while that agent's last fresh start saw the skill that is on disk now;
  otherwise it answers 403 "this agent cannot take your messages yet". Every bring-up prints one
  warning line that names such agents (at most eight, grouped by reason). For the manager, run `lever up --fresh` (it backs the conversation up first; `--keep-conversation` does not help, the kept conversation keeps its old skill); a
  worker takes contact messages once the broker next creates it fresh.
- **Which agents to list.** Prefer workers with a narrow task. Listing the manager gives the
  contact a chat with the agent that holds the whole tree; the skills tell agents to answer a
  contact only with what the task needs, but that is an instruction, not a fence.
- **Verified as a contact.** A contact's message verifies with the tier `contact`. The agent
  uses it as the contact's answer for its task, never as an instruction about the system, other
  tasks, tools or recipients, and a worker records the answer in its task files.
- **Setup order.** Add the contact to `allowed_users` and run `lever apply` (or `lever reload`)
  while agents run: apply invites the contact's hub user, caps it and binds
  `lever-remote-contact` before the proxy accepts the login (see [the remote web
  role](#the-remote-web-role)). The contact's first sign-in finds the role and the ceiling already
  in force, and the fence applies from the first request. Removing the contact refuses it at the
  proxy before any other apply step, then suspends its hub user.
- **Privacy.** Anyone with `project.read` on the instance project (an operator) can read a
  contact's chat with an agent in the project message log. The contact's own event stream still
  carries its other direct chats, if an agent outside its list starts one with it; its DM list is
  filtered to its listed agents.
- **Manager messages are recorded.** A worker verifies every message too, so a manager message
  answers `lever` with the kind `manager` and a contact's post answers `web` with the tier
  `contact`, whatever either says.
- **A record names the sender, not the words' origin.** When the manager forwards a contact's
  words to a worker, the message verifies as `lever`, kind `manager`: the record shows who sent
  it, and nothing can follow text through an agent. The skills tell the manager to say whose
  words it forwards, and every agent to keep the contact's tier for quoted or forwarded text; that
  is an instruction, not a fence.
- **The session check covers the agents a contact lists.** A contact can post only to those, so
  the proxy checks only their sessions. Other agents get a contact's words only inside a lever
  message (a relay or a manager message), whose first line lever writes and whose record the
  agent verifies, so a marker the contact typed is never that message's marker.

## Messages agents start

With `remote.agent_messages.enabled: true` an agent can tell a contact something without waiting
for the contact to write first ("workbook v3 is ready", "I need document X"), in the chat the
contact already has with it. Lever controls these messages, and replies to a contact too:

```yaml
remote:
  allowed_users:
    - operator@example.com
    - {login: client@example.com, tier: contact, agents: [deal]}
  agent_messages:
    enabled: true
    follow_up_after: 24h      # default; 1h to 720h
    max_chars: 4000           # default; 1 to 16000
```

Config load refuses `enabled: true` when `allowed_users` has no `tier: contact` entry (agents
message only contacts), and on a `scion.version` older than `b3562fb1`; `lever apply`, `lever up`
and `lever remote serve` refuse it on a `scion.source` checkout that does not hold that commit (see
**Scion version** below). It works with either `landing`.

- **The agent's two steps.** The agent calls `contact_message` (a tool of its `lever-capability`
  MCP server) with the contact's login, the whole text and, for a reply, the `message_id` that
  `message_verify` returned for the contact's post. The broker checks that the contact lists
  this agent, that a reply answers that contact's verified post to this agent, the length and the
  limit, and records a hash of the exact text in `.lever-state/agent-ledger/` (0700, one 0600
  file per contact). `lever-agent` writes the text to a 0600 file and the agent runs the
  `scion message --body-file … -- '@<email>'` command it gets back. `contacts` lists the logins
  the agent may write to, with when it may start its next message.
- **What the contact sees.** Only agent messages whose exact text the record holds, each
  record showing one hub message, sent within 10 minutes of the authorization (the hub's time
  for it may be at most 10 seconds before the authorization and 2 minutes after its expiry). A message the
  agent sent any other way, or edited after it showed, is removed from the contact's history,
  and so is an agent message of any type but `instruction`, `input-needed` or `assistant-reply`
  (the agent picks the type, and a system or state type would read as a hub notice). Each row,
  and the answer, keeps only the fields the chat pages read (an agent's `threadId` and
  `channel` go). Reply previews are removed, and so are the attachment entries of every agent
  message (their names are agent text no record covers). A row id that appears twice on a page
  hides both rows.
  The contact's event stream carries only the subject of each event (and the stream's event id),
  never text, a sender or a message id; the DM list carries no last-message preview, sender, id
  or time and no unread flag; the chat page's unread count holds only shown messages. Anything
  the proxy cannot read, or a broker that does not answer, hides every agent message (it fails
  closed); paging stays the hub's, so a page can hold fewer messages than asked for, and its
  `totalCount` can show how many rows the page hid (never their text). Also accepted: the
  paging cursor (`nextCursor`, base64 of the oldest row's send time and hub message id) can
  reveal the time and id of a hidden row, and the live event subjects reveal that an agent the
  contact does not list sent it a direct message, when, and that agent's hub id. No message
  text leaks either way.
- **The limit.** Per agent and contact: one message the agent started and the contact has not
  answered yet, then one reminder after `follow_up_after`; a message from the contact resets
  it. Every authorization counts, sent or not. A reply to a contact message is not limited by
  this rule: up to 3 per contact message, within 24 hours of it. Every agent has at most 30
  authorizations an hour. Refusals are fixed words: `not-a-contact`, `limit`
  (with `next_allowed_at`), `too-long`, `empty`, `bad-text` (invalid UTF-8, a control
  character, or a word that starts with `@`, which the hub would rewrite), `bad-ref`, `rate`,
  `off`, `unavailable`. The broker audit names the login, agent, kind, record id, length and
  decision, never the text.
- **What the operator sees.** Everything, unfiltered, as before. Messages to the operator keep
  their old path. The chat page's [Contacts
  section](#the-operators-view-of-contact-conversations) shows each contact's conversations
  read-only, with the agent messages the contact is not shown marked.
- **Turning it on (or off).** Run `lever init` first, then `lever apply`: `init` rewrites both
  skills (they teach the two steps while it is on), and `apply` refuses contact logins while the
  skills on disk are not the ones this config renders. `apply` then restarts the broker and the
  proxy (the key is part of their config stamps); until it runs, they keep the old setting, and
  the `agent messages` row of `lever doctor` says so. The agent image must contain this
  release's `lever-agent`, which has the two tools: run `make lever-image` (and rebuild an
  instance image built from it), then `lever up --fresh` for the manager (it backs the conversation up
  first). Each contact agent must start fresh on the new skill: `lever up --fresh` for the
  manager, purge and start for a worker. Until then a contact's post to it is refused as not
  fresh. Turning it off again restores the earlier behaviour, with the same steps.
- **Clocks.** The hub stamps a message with the VM's clock, and lever binds it to its
  authorization only when that time is at most 10 seconds before the authorization (host
  clock). A VM clock that fell behind the host (on Lima, after the Mac slept) hides authorized
  messages from contacts. With agent messages on, the `guest clock` row of `lever doctor` fails
  when the VM is more than 5 seconds behind; resync it (`limactl shell <vm> sudo hwclock -s`, or
  restart the VM). A message sent while the clock lagged keeps its hub time and stays hidden;
  messages sent after the resync show.
- **Scion version.** The filter trusts the sender the hub stamps on a row, which scion sets from
  the caller's own auth only from commit `b3562fb1` (2026-08-28). On an older scion an agent could
  post a row as its contact and the contact would see it. Config load refuses
  `agent_messages.enabled` with a `scion.version` pseudo-version older than that commit; `lever
  apply`, `lever up` and `lever remote serve` refuse it, and the `agent messages` row of `lever
  doctor` fails, when a `scion.source` checkout does not hold it (git `merge-base
  --is-ancestor`): one whose `HEAD` is older, and one git has no such commit in at all (a
  checkout never fetched since, or a fork without it). A shallow clone that does not show the
  commit is refused too, since its cut-off history cannot show it either way; `git fetch
  --unshallow` fixes that. The `lever doctor` row warns when lever cannot tell (a `scion.binary`,
  a bare commit hash or a tag, a `scion.source` path git cannot answer for: not a repository, git
  not runnable, or no answer within 5 seconds). A pseudo-version is judged by its
  commit time alone: a commit dated after `b3562fb1` on a branch or fork that lacks it passes.
  `scion.version` is the operator's choice, not the agent's, so this is a guard against a
  mistake, not a boundary; pin an upstream `main` commit.
- **Known limits.** The hub's own web UI (the console landing) shows a new agent message to a
  contact only after a reload, since the events carry no text, and shows no unread marks (lever's
  chat page counts them itself). A message older than the two 4 MiB record files of that
  contact stops showing. An authorization the agent does not send
  still uses its slot until the reminder time.

## The operator's view of contact conversations

With `landing: chat`, an operator login's agent list on the [chat page](#the-chat-page) has a
**Contacts** section: what you would otherwise get by being copied on a contact's email. A contact
login never gets it: the page does not show it, and the routes refuse a contact (`403`, audit
`deny-contact`) whatever it sends.

A contact whose login holds a `/` is listed but cannot be opened: its route would carry an
encoded slash, which the proxy refuses on every path (`400`, audit `deny-path`).

- **What it shows.** Each contact in `allowed_users`, the agents it may message (its `agents:`
  list; a `see:` agent has no conversation), and, per agent, the whole direct conversation
  between them: read-only (no composer, no read marker, no event stream), newest at the bottom,
  read again every 30 seconds while it is open and with **Refresh**. "Load earlier messages"
  pages back. With `agent_messages` on, every agent message the contact is not shown (see
  [Messages agents start](#messages-agents-start)) is marked "not shown to the contact", so you
  can spot an agent that writes to a contact outside the rules. An agent message that a record
  would show but no read of the contact has bound yet is marked "not yet read by the contact":
  the contact has not seen it, and sees it on its next read. The marks are per page: when two
  messages carry the same text and one record, which of them the record finally shows depends on
  the page the contact reads first, so a "not yet read" mark can turn into "not shown". When the broker does not answer,
  the page says that which agent messages the contact sees is not known (the contact is then
  shown none).
- **How it reads.** The hub lets a person read only the direct messages that name them, so the
  proxy reads the conversation with the CONTACT's own hub session, on the host, and passes you the
  rows as text. That session makes one request kind only: `GET` of that one conversation's
  history (`limit` and `cursor`, nothing else of your request), with no redirect followed. Your
  browser never holds the contact's session. The contact login and the agent in the path must
  match your config exactly; the conversation key comes from the agent's hub record and the
  contact's hub user id, never from the request.
- **"has not signed in yet".** The proxy reads only for a contact that `lever apply` bound to a
  hub user (`.lever-state/remote-role.json`). Apply binds every configured contact before its first
  sign-in, so this answer means apply has not run since the contact was added, or refused that
  login (see [the remote web role](#the-remote-web-role)); run `lever apply`. When the hub refuses
  a read and the contact's session belongs to another hub user than the one `apply` bound, the
  answer is also `not-signed-in`, with the hint `run lever apply`. To find that out the proxy asks
  the hub who the session is (`GET /api/v1/auth/me`, no redirect followed), only after a failed
  read. The proxy's own sign-in never creates a hub user: the hub admits only invited users, and the
  proxy refuses a session whose user is not the one lever recorded.
- **Side effects.** A bound contact with no live session in the proxy (after a proxy restart, or
  after the 12-hour renewal) is signed in by the proxy, which moves the contact's `last login` on
  the hub. With `agent_messages` on, your read asks the broker with a peek: the answer is what the
  contact's own read would show now, and the agent ledger is not written. Only the contact's
  reads bind a record to a message, so your reads never change what the contact sees.
- **Routes.** `GET /lever/api/contacts` (the contacts, their message agents, labels, states and
  whether each is bound) and `GET /lever/api/contacts/<login>/agents/<name>/messages?cursor=&limit=`
  (`limit` 1 to 200, default 50), with the login URL-encoded; with
  [files](#files-in-the-chat) on, also `GET /lever/api/contacts/<login>/agents/<name>/files` (that
  contact's file exchange with the agent, from the host record only; `404 not-found` when files are
  off or either login has `files: false`). `HEAD` is answered like `GET`; any
  other method is `405`. Refusals are fixed words: `not-found` (404: not a contact, or not one of
  its agents), `not-signed-in` (409, with `"hint": "run lever apply"` and the audit reason
  `stale-binding` for a changed hub user), `no-record` (409: the agent has no hub record),
  `bad-query` (400) and `unavailable` (502: the hub or its answer failed; 503 for the files
  route when the record cannot be read).
- **Audit.** Every answer is one line in `.lever-state/remote-audit.jsonl`: `operator-view` with
  the `contact`, the `agent` and the `count` of rows (or of contacts for the list), or
  `deny-operator-view` with the `reason` word. No line holds message text.

## Notifications

With `landing: chat` and `remote.push` on, a login can have its devices show a notification when
an agent writes to it while the chat page is closed.

```yaml
remote:
  landing: chat
  push:
    enabled: true
    subject: mailto:you@example.com   # the contact the push services see for this sender
```

Config load refuses `push.enabled` without `landing: chat`, and a `subject` that is not
`mailto:<address>` or an `https://` URL (required, at most 200 characters). Run `lever apply`: it
restarts the proxy. Push needs no image rebuild and no `lever init`. Web push has not been tested
on a real device yet (Linux Chrome, iPhone).

- **What a login sees.** "New message from <agent>" (or "New message" when the agent is not in
  the login's list). A tap opens the chat with that agent. No message text leaves the host, not
  even encrypted: the push carries only `{"v":1,"agent":"<name>"}`, encrypted for the device
  (Web Push, VAPID and aes128gcm, on the Go standard library).
- **Turning it on.** Per device, from **Turn on notifications** in the agent list (the browser
  asks for permission then). On an iPhone, add the page to the Home Screen first and open it from
  there (iOS 16.4 or later). On Linux Chrome, the browser tab or the installed app both work.
  **Turn off notifications** removes the device's subscription. A login keeps at most five
  devices (the oldest goes); a device endpoint belongs to the last login that turned it on in
  that browser. An endpoint another login holds moves only with the same subscription keys (the
  same browser): one posted with other keys gets 409 and stays where it is. A login may turn
  devices on or off ten times a minute (then 429).
- **Shared devices.** A browser keeps one push subscription for the page, whoever is signed in.
  The page re-sends it at load only for the login that turned notifications on in that browser
  (a per-login mark in the browser's storage). Another login on the same device sees **Turn on
  notifications** and gets no notification until it turns them on itself, which moves the
  device to it. Turning notifications off removes the browser's subscription for every login.
- **Who is told.** A login is told only about agents it may message (an operator: every agent; a
  contact: its `agents:` list, never a `see:` agent). With `agent_messages` on, a contact is told
  only about a message it would be shown. No notification for the login's own messages or hub
  lines, none while the chat is open and read, and at most one per agent a minute (a later
  message in that minute gets one more when the minute ends).
- **How it works.** For each login with a device, the proxy holds one hub events stream with
  that login's own hub session; an event for one of its direct conversations makes the proxy
  read the newest messages as the login and decide. After a hub or proxy restart it catches up
  (a message older than one hour is not notified); a check the hub or the disk fails is tried
  again, up to five times over about 31 seconds (1, 2, 4, 8 and 16 s); then it stops, with a
  `push-failed` audit line (reason `retries`), until the next event or connect checks again. An
  event whose conversation matches none of the login's agents checks every agent of the login
  (one `push-stream` line per check pass, reason `unknown-dm`, with `count` = the number of
  unknown conversation keys). A push goes to a login's devices at once; on a
  proxy shutdown a send still under way gets five more seconds. Each stream is one long-lived
  connection into the jail, so each subscribed login keeps one jail dial process open on the host.
- **What the push services learn.** Google, Apple, Mozilla or Microsoft see when a push goes to
  a device, never what it says. That timing tells them when your agents write to that login.
- **The broker's record.** With `agent_messages` on, the check for a contact asks the broker
  what the contact's own read would show now, without binding: the push check never decides
  which message a ledger record shows, and the operator's view still marks such a message "not
  yet read" until the contact reads it.
- **The service worker.** `/lever/sw.js` (scope `/lever/`) handles `push` and
  `notificationclick` only: no fetch handler and no cache, so it never stands between the page
  and its requests. The page registers it only when a login turns notifications on. With push on,
  the page's CSP gains `worker-src` for `/lever/` and allows one Trusted Types policy,
  `lever-sw`, whose only output is the worker's URL (`serviceWorker.register` is a script-URL
  sink); with push off the CSP is the same as without push.
- **Egress.** The host process connects out to the push services: `fcm.googleapis.com`,
  `*.push.apple.com`, `updates.push.services.mozilla.com` and `*.notify.windows.com`, on 443. A
  host firewall must allow that. The proxy accepts a subscription only on those hosts (https,
  no port, one spelling per endpoint), never
  follows a redirect, uses no HTTP proxy, and connects only to public addresses (never a
  private, loopback, link-local or tailnet `100.64.0.0/10` address, whatever DNS answers).
- **State.** `.lever-state/push/` (0700): `vapid.key` (the signing key), `subscriptions.json`
  and `status.json` (the last send result), each 0600. A file that another user can read keeps
  push off, and `lever doctor` names it. Each write is synced to disk before it replaces the
  file; an empty or damaged `vapid.key` or `subscriptions.json` (an older power loss) also keeps
  push off, and the start warning and `lever doctor` name the file: stop the proxy, remove it,
  and run `lever apply`. Deleting `vapid.key` makes every device turn notifications on again. A login removed from `allowed_users` loses its devices at the next
  proxy start.
- **Off.** `enabled: false` removes the routes and the worker; the page unregisters the worker on
  its next load.
- **Doctor.** The `push` row shows off, or on with the key, the device count per login and the
  last send result.
- **Audit.** `push-subscribe`, `push-unsubscribe`, `deny-push`, `push-sent`, `push-gone`,
  `push-failed` and `push-stream` lines in `.lever-state/remote-audit.jsonl`. A line names the
  push service's host only, never the endpoint path, a key or a payload.

> **Test only.** For the end-to-end test with `tools/test/pushrecv` (a fake push service that
> decrypts what it gets), the proxy admits exact loopback addresses over plain http only when
> both `remote.push.test_hosts: [127.0.0.1:<port>]` in `lever.yaml` and
> `LEVER_PUSH_TEST_HOSTS=127.0.0.1:<port>` in the proxy's environment name the same addresses.
> Either one alone, or any other value, stops `lever remote serve`; the proxy prints a warning
> and `lever doctor` fails while they are set. Never set them for a real instance.

## Files in the chat

With `landing: chat` and `remote.files` on, a login uploads files to an agent it may message, and
downloads the files that agent shares with it. For example, a contact sends a PDF, and the
agent sends back a versioned `.xlsm` workbook.

```yaml
remote:
  landing: chat
  files:
    enabled: true
    # max_bytes: 26214400                     # one file; default 25 MiB, at most 100 MiB
    # extensions: [pdf, xlsx, xlsm, xls, csv, docx, doc, png, jpg, jpeg, txt, zip]
    # uploads: true                           # default; false = no new upload
    # shares: true                            # default; false = no new share, no share download
  allowed_users:
    - operator@example.com
    - {login: client@example.com, tier: contact, agents: [deal], files: false}   # no files for this login
```

Config load refuses `files.enabled` without `landing: chat`, a worker `dir` that overlaps
`.lever-files` at the tree root (the manager's exchange), and a `manager.read_only` entry that
covers it.

- **What a login does.** In a chat with an agent it may message (never a `see:` agent), the
  paperclip uploads one file at a time, with progress. After the upload, the page sends the chat
  message "📎 uploaded <name>" through the normal send path, so the agent verifies it as that
  login's message. The **Files** button lists the login's own uploads to that agent and the files
  the agent shared with it, with a download link for each. An operator also sees each contact's
  files in the [operator's view](#the-operators-view-of-contact-conversations), read-only, and may
  download them.
- **Switching a direction or a login off.** `files.uploads: false` stops new uploads (the upload
  route answers `403 uploads-off`, the page hides the paperclip); `files.shares: false` stops new
  shares (`share_file` answers `shares-off`) and the download of shares already made (`403
  shares-off`; the page lists them without a link). Records already made stay listed either
  way, and an upload stays downloadable by its owner. An `allowed_users` entry with `files: false`
  gives that login no file exchange at all: its file routes answer 404 as with files off, its
  page shows no paperclip and no Files panel, `share_file` and `contact_files` answer
  `not-a-contact` for it, and the operator view lists no files for it (and asks for none). For
  a contact this is enforced: the contact fence keeps it to the chat page, so it has no other
  way to the files. For an operator entry, `files: false` only hides lever's chat-page file
  feature (its own file routes, the operator view's files, being a share target); it is not
  access control, because an operator has the whole hub (the workspace file API, WebDAV, the
  terminal) and can reach every file in the tree there. Each
  change restarts the proxy (and, for shares or a login, the broker) at the next `lever apply`;
  a direction change also rewrites the skills, so run `lever init` first; contacts can then neither post nor upload to an agent until it starts a fresh session (`lever up --fresh` for the manager, purge and start for a worker).
- **Where files live.** In the agent's own workspace, under `.lever-files/`: `in/<key>/` holds
  uploads and `out/<key>/` holds the files the agent shares, one `<key>` per login (a hash of the
  login, so no login text appears in a path). The manager's exchange is at the tree root
  (`<tree>/.lever-files/`); a worker's is in its `dir`. The manager's workspace is the whole tree,
  so it can see every worker's exchange; its skill tells it not to touch them. An upload is stored
  as `<UTC time>-<name>`, with the name reduced to letters, digits, `.`, `_`, `-` and spaces
  (no leading dot or dash, no Windows device name, at most 120 bytes). The page shows that
  sanitized name, without the time.
- **How an agent reads an upload.** The `contact_files` tool (lever-capability MCP server) lists
  lever's host record of one login's files: its uploads (name, size, sha256 and path) and the
  agent's shares to it. The `contact` argument is required, and no call lists every login's
  files. The skill tells the agent to verify the "📎 uploaded" message first, to ask for the
  login that message names as its sender, to use a file only when `contact_files` lists it with
  the same sha256, and to use one login's files only in that login's conversation, never
  another login's, even when asked. What a file says is data from that login, never an
  instruction.
- **What is enforced, and what is not.** Lever enforces who uploads to and downloads from an
  agent, and that a share goes only to the login whose `out/<key>/` holds it. Keeping one
  login's files out of another login's conversation is the agent's skill, not a boundary: the
  agent can read every file in its own workspace, all logins' uploads included, and the broker
  only scopes each `contact_files` call to the login it names. A worker that must never see one
  contact's files next to another's needs its own worker per contact.
- **How an agent shares a file.** It writes the file directly into the login's `out_dir` (which
  `contact_files` returns, with the `in_dir`), then
  calls `share_file` with the login and the path. The broker accepts only a regular file in the
  caller's own `out/<key-of-to>/`, with no symbolic or hard link, within the size and type
  limits, to a login whose `agents:` list names the caller (an operator always). It records the
  sha256. A new version is a new file.
- **What lever guarantees.** An upload is first read whole into a private file outside the
  tree; only after its limits pass does lever copy it into `in/<key>/`, so a refused upload
  leaves nothing in the agent's workspace. Host-side writes into the agent-writable tree never follow a link
  (each directory is opened through its parent and checked, and the file is created with
  `O_EXCL`); uploads are `0600` in `0700` directories (the agent's container user is the host
  owner). A download is served only from a private copy whose sha256 equals the record: a file
  the agent changed, replaced by a link or hard-linked after the share is refused (409), and a
  deleted one is gone (410). Downloads are attachments (`application/octet-stream`, `nosniff`,
  `Content-Security-Policy: sandbox`, `no-store`), never shown in the page. Another login's file
  id answers like an unknown one. No log, audit line or record holds file content.
- **Limits.** `max_bytes` and `extensions`; per login and agent, 30 uploads an hour and 1 GiB a
  day; per login, 60 upload attempts an hour (refused ones count), 60 file lists and 20
  downloads a minute; 2 uploads and 2 downloads at once per login, 4 of each in all, the last
  one only for an operator; 20 shares an hour per agent. An upload body or a download gets 10
  minutes plus `max_bytes` at 64 KiB/s, at most 1 hour (about 17 minutes at the 25 MiB default);
  an upload that passes it answers `408 timeout`. The page itself stops waiting for an upload
  after 10 minutes.
  The extensions `html`, `htm`, `xhtml`, `shtml`, `svg`, `js`, `mjs` and `xml` are refused at
  config load.
- **Turning it on or off.** `landing: chat` is required. The skills change with the setting,
  so run `lever init` first (it rewrites them), then `lever apply` (it restarts the broker and
  the proxy). From then on a contact sees every agent as not fresh, and cannot post or upload to
  it, until that agent starts a fresh session on the new skill: `lever up --fresh` for the
  manager (it backs the conversation up first), purge and start for a worker. The operator is not
  held back. The agent image must contain this release's lever-agent: run `make lever-image` (and
  rebuild an instance image built from it) before the fresh starts. It has the two tools; they are
  direct lever-capability tools, called with no `request` mint. While files are off,
  `share_file` refuses with `off` and `contact_files` returns an empty answer with a note.
- **Routes.** `GET /lever/api/files/<agent>` lists the login's exchange with the agent,
  `POST /lever/api/files/<agent>` takes one upload (a multipart form with one `file` part; `201`),
  and `GET /lever/api/files/<agent>/<id>` downloads one recorded file. Refusals are JSON
  `{"error": "<word>"}`: `not-allowed` (403: an agent the login may not message, see-only, hidden
  or unknown), `origin` and `uploads-off` / `shares-off` (403), `not-fresh` (409, a contact),
  `changed` (409) and `gone` (410) for a download, `too-large` (413), `bad-form` (400 or 415),
  `extension` (415), `timeout` (408), `rate`, `quota` and `busy` (429), `try-again` and
  `unavailable` (503). With files off, or for a login with `files: false`, every path under
  `/lever/api/files/` is a 404.
- **The front.** The page sends every upload with the header `X-Lever-Upload: 1`, and the proxy
  refuses an upload without it. A browser sends such a header to another origin only after a
  CORS preflight, which the proxy never grants. A front must not answer `/lever/api/` with a
  307 or 308 redirect: those resend the request body.
- **Doctor.** The `files` row shows off, or on with each direction, `max_bytes` and the allowed
  types, the allowed types that can carry macros (`xlsm`, `xls` and `doc` in the defaults: open
  them with macros off), the logins with no files, and the bytes of uploads stored per agent; it fails when the state directory is inside the tree (then nothing can be recorded
  and every route refuses), when the ledger (or its `.lock`) is not private, or when an agent's
  `.lever-files`, `in`, `out` or `in/<key>` is a link.
- **Audit.** `file-upload`, `file-download` and `deny-file` lines in
  `.lever-state/remote-audit.jsonl`; the broker audits `files` list and share calls.
- **Known limits.** The host record is `.lever-state/files-ledger/` (`0700`, one `0600` file per
  agent). A file is rotated at 8 MiB; past the second rotation its oldest records are dropped, and
  those files are no longer listed or downloadable. Lever never deletes a file, uploads included:
  the doctor row shows how much each agent holds; remove old ones from `.lever-files/in/` by
  hand. Disk space is the host's.

## Dictation and read-aloud

With `landing: chat` and `remote.voice` on, the chat page has a mic beside the message box. A
login taps it, speaks, and taps again; the clip is transcribed on your host by a Whisper model,
and the text lands in the message box at the cursor. **It is never sent by itself**: the login
reads it, corrects what Whisper got wrong, and presses Send. What is sent is an ordinary typed
message, with the usual chat ledger and provenance.

The transcription runs in the first-party broker tool [`lever-tool-whisper`](/whisper-tool/),
which runs whisper.cpp's `whisper-server` as its child. The proxy hands each clip to the tool over
a Unix socket on the host that only your user can reach. It does not go through the broker, and
agents cannot reach the socket.

While voice is on, each agent message also has a speaker button that reads it aloud: with the
browser's own on-device speech synthesis (`read_aloud: device`, the default), or with a natural
neural voice on your host ([server voices](#server-voices), `read_aloud: server`), falling back to
the device's voice when that is down. `read_aloud: off` turns it off. With voice off, the page
shows neither.

```yaml
remote:
  landing: chat
  voice:
    enabled: true
    socket: /home/you/my-instance/.lever-state/whisper/dictate.sock   # the tool's -dictate-socket
    # max_seconds: 300                        # one clip; at most 600, and at most the tool's -max-seconds
    # read_aloud: device                      # the speaker button: device (default), server or off
    # speak_socket: /home/you/my-instance/.lever-state/tts/speak.sock   # with server: lever-tool-tts's -speak-socket
  allowed_users:
    - operator@example.com
    - {login: client@example.com, tier: contact, agents: [deal], voice: false}   # no dictation for this login
```

Every login gets dictation and read-aloud while voice is on, operators and contacts alike,
except an `allowed_users` entry with `voice: false`.

**Browsers.** Dictation needs MediaRecorder and OfflineAudioContext: current Chrome, Edge, Firefox,
and Safari 14.1 or later (iOS included). The microphone needs a secure context (the HTTPS front);
without one, or without those APIs, the page simply shows no mic. Read-aloud needs the Web Speech
synthesis API and at least one on-device voice, or, for server voices, Web Audio (every current
browser).

### Setting it up

1. **Install whisper.cpp's server yourself.** Lever does not build or install it. Build
   `whisper-server` from whisper.cpp with GPU support: CUDA on a Linux host with an NVIDIA GPU,
   Metal on Apple Silicon (its default there). Install it outside the tree, owned by you or root
   and writable by no one else, for example under `/usr/local/bin`. Without a GPU it still works
   with `-gpu=false`, much more slowly. The model needs disk space (about 1.6 GB for
   `large-v3-turbo`, 0.55 GB for `large-v3-turbo-q5_0`) and about as much GPU memory, or memory
   with `-gpu=false`.
2. **Add the tool to `broker.tools`.** `make install` builds `lever-tool-whisper` next to
   `lever`. All paths are absolute; `-models`, `-server` and `-dictate-socket` must be outside the
   tree. The [whisper tool guide](/whisper-tool/) lists every flag.

   ```yaml
   broker:
     tools:
       - name: whisper
         command: [/home/you/.local/bin/lever-tool-whisper,
                   -tree, /home/you/my-instance/workspace,
                   -models, /home/you/my-instance/.lever-state/voice-models,
                   -server, /usr/local/bin/whisper-server,
                   -model, large-v3-turbo,
                   -whisper-port, "8448",
                   -dictate-socket, /home/you/my-instance/.lever-state/whisper/dictate.sock]
         backend: 127.0.0.1:3212
         operations:
           - {name: transcribe, params: [file]}
   ```

   The `transcribe` operation must be listed (the tool registers it), but no agent can use it
   without an `obtain` grant: see [agents](/whisper-tool/#agents-the-transcribe-operation).
3. **Set `remote.voice`** as above, with `socket` equal to the tool's `-dictate-socket`. Config
   load refuses a `socket` that is no whisper tool's `-dictate-socket`.
4. **Fetch the model:** `lever voice fetch` (or `lever voice fetch large-v3-turbo-q5_0`). It
   downloads into the tool's `-models` directory, from Hugging Face at the commit lever pins, and
   keeps the file only if its size and sha256 match. Neither the tool nor the proxy ever
   downloads a model.
5. **Reload:** `lever reload` starts the tool with the broker and restarts the proxy with the new
   config. A model fetched later is picked up by the running tool within a minute.
6. **Check:** `lever doctor` shows the `voice`, `voice tool` and `voice model` rows, and the
   tool's own row. The `voice tool` and `voice model` rows also run with `remote.voice` off, for a
   whisper tool that only agents use.

### How it works

- **The page.** The mic shows only when the agent list carries voice for the login (voice on, the
  login not excluded, and the tool answering ready on its socket; the proxy asks at most every 5
  seconds) and the browser can record. The page records with the browser's MediaRecorder, then
  decodes the clip, resamples it to 16 kHz mono and encodes it as WAV, all in the browser, and
  posts it to `POST /lever/api/voice/transcribe`. A timer shows the time recorded; recording
  stops by itself at `max_seconds`. Hiding the page (switching tab or app, locking the phone) or
  leaving it ends a recording in progress and discards the clip: nothing is sent, and the page
  says so. While lever transcribes, the page shows "Transcribing…". The
  browser asks for the microphone the first time; the page needs the HTTPS front (a secure
  context).
- **The proxy.** It checks the login, the request and the clip (below), then posts the clip to
  the tool's socket and returns the text. It runs no transcriber of its own.
- **The tool.** `lever-tool-whisper` runs whisper-server as its child, bound to `127.0.0.1` on
  `-whisper-port`, with the model loaded once and every route under a random path prefix that is
  new at each tool start and never logged. Before each start of the child it checks the program
  and the model's size and sha256 again; it restarts the child with a growing delay if it exits.
  The child runs in the `-models` directory, gets a reduced environment (what a GPU build needs to
  find its libraries, no credentials), and its output is discarded, because whisper.cpp prints
  what it transcribes. If something else already listens on the port, the tool does not start the
  child, sends it nothing, and the page shows no mic. On Linux the tool also checks, before it
  sends anything, that the listener is its own child's (a process that bound the port while the
  model loaded is refused); on macOS it cannot tell. The tool checks each clip again and lets one
  clip at a time reach whisper-server. Dictation goes first in queue order: a waiting dictation
  clip is taken before any waiting agent clip, but a clip already running is never interrupted, so
  dictation can wait for up to one agent clip (at most the tool's `-agent-max-seconds` long, 120 s
  by default).
- **Stopping.** The broker stops the tool with SIGTERM, and the tool stops its child before it
  exits. On Linux the child also dies with the tool, even when the tool is killed. On macOS a
  tool killed hard (the broker sends SIGKILL after 1.5 s, and `lever stop` kills a broker that is
  slow to exit) can leave the child running and holding the port; the next start finds it through
  the pid file the tool keeps in `-models`, checks that it is the configured whisper-server on
  this port, and stops it. If another process holds the port, dictation stays off: find it with
  `lsof -nP -iTCP:<port> -sTCP:LISTEN` and stop it, then `lever reload`.
- **The route.** Only for a verified login, like the rest of the chat page. The page sends
  `X-Lever-Voice: 1` with each clip and the proxy refuses a clip without it (a browser sends such
  a header to another origin only after a CORS preflight, which the proxy never grants). The
  body must be WAV in exactly the form the page writes (PCM, 16-bit, 16 kHz, mono, the plain
  44-byte header), at most `max_seconds` long and `2 × 16000 × max_seconds` bytes plus 1 KiB. So
  whisper.cpp only ever parses a header lever has checked byte for byte, never a compressed
  file. The answer is `{"text": "..."}`, trimmed and cut to the chat message limit, or
  `{"error": "<word>"}`: `voice-off` and `origin` (403), `bad-audio` (400 or 415), `too-long`
  (413), `timeout` (408), `rate`, `quota` and `busy` (429), `unavailable` (503). With voice off,
  every path under `/lever/api/voice/` is a 404.
- **Limits.** One transcription at a time, and two more waiting: three slots in all, of which a
  contact never takes the last, so an operator can always dictate. When a browser gives up, the
  proxy closes its request to the tool: a clip still waiting in the tool's queue leaves it, and a
  clip being transcribed ends the tool's request to whisper-server, which stops work on it; the
  slot frees. One clip at a time per login. Per login, 30 clips an hour, 60 minutes of audio a
  day, and 60 attempts an hour (clips that fail their checks count as attempts). A clip does not
  count against the clips or the minutes when the tool never took it: the tool or its socket was
  down, the tool refused the clip before whisper-server, or the browser gave up (or the proxy's
  wait ran out) while the clip waited for its turn in the proxy. Once the tool has taken a clip
  it counts, even if it then waited in the tool's queue, failed, timed out or the browser gave
  up. These counts are kept
  in memory and start again when the proxy restarts. Whisper's known non-speech markers (such as
  `[BLANK_AUDIO]`, `[MUSIC]`, `(inaudible)`) are removed from the text; other brackets, such as
  dictated code, stay. Sending a clip may take one minute plus the time for the largest clip at
  64 KiB/s.
- **Read-aloud.** With the device's voices (`read_aloud: device`, or [server voices](#server-voices)
  that are down), the page uses the browser's speech synthesis with **on-device voices only**
  (those the browser marks as local): a voice that would send the text to a speech service is
  never used, even if it is the browser's default. Before reading, the page removes markdown
  formatting, reads a code block as "code block skipped" and a link as its text or host name.
  Tapping the button again, or another message's, stops it, and so does leaving the page. With more than one on-device voice,
  the chat header offers a choice, which the browser keeps for that device. A device with no
  on-device voice cannot read aloud.

### Security notes

- **Audio stays on your host and is never stored.** The clip goes from the browser to the
  proxy, which holds it in memory, checks it, and passes it over the socket to the tool, which
  passes it over loopback to whisper-server. No audio and no transcript is written to the tree,
  the state directory, a ledger or a log. The transcript goes back only to the login that sent
  the clip. The proxy's audit line (`voice-transcribe` or `deny-voice` in
  `.lever-state/remote-audit.jsonl`) has the login, the clip's length, the outcome and the
  latency, and the tool's log line has the length, the outcome and the latency; never the audio or
  the text.
- **Who reaches what.** The dictation socket is created `0600` in a directory the tool creates
  `0700` (it refuses a directory that is a symbolic link, belongs to someone else or is open to
  other users), so only processes of your user reach it; config load refuses it inside the tree,
  and the jail reaches no host Unix socket. The tool's TCP backend is reached through the broker,
  as for every first-party tool, and offers agents only `transcribe`, and only with a capability.
  Any agent with a whisper `transcribe` capability (obtained, or delegated to it) reads files from
  one folder, the tree root's `.lever-files/whisper/`, which only the manager can write: grant and
  delegate it only to agents that may read what the manager puts there.
  whisper-server has no authentication of its own: it is safe because it listens on host
  loopback only, the jail's egress rules drop every host loopback port that is not on the
  allowlist, and config load refuses `-whisper-port` in `manager.allow_ports`, on the broker's
  jail and admin ports, the proxy port and every tool backend (and the
  `voice` row of `lever doctor` checks it again). Nothing new is mounted into an agent, and no
  agent can call the dictation route: it is the chat page's, behind the login gate.
- **Other host processes and web pages.** whisper-server allows cross-origin requests from any
  site and has a `/load` route that loads a model from any path it is given. Lever therefore puts
  every route under a random 128-bit path prefix, new at each tool start: a web page open on the
  host, or a name rebound to `127.0.0.1`, finds only 404s. The prefix is not a defence against
  other users of the host: whisper-server takes it on its command line, so anyone who can list
  processes can read it, and any local user can connect to a loopback port (and, on macOS, could
  bind it first while the model loads and receive clips). With the prefix, such a user can make
  whisper-server load a model file of their choice, as your user. Run voice only on a host where
  you trust every local user, as for every loopback service. A second instance's
  `manager.allow_ports` on the same host is not checked against this instance's `-whisper-port`:
  keep the ports distinct.
- **The model and the program are checked.** The tool starts whisper-server only with a model
  whose size and sha256 match lever's pinned table, and only if the program's real file is an
  executable owned by you or root that no one else can change, and still the same real file as at
  the tool's first good check. Config load and the tool refuse `-server`, `-models` and `-dictate-socket`
  inside the tree; a `manager.read_only` entry does not excuse them. The vocabulary is a flag in
  `lever.yaml`, never read from a file in the tree, so an agent cannot steer the prompt.
- **Whisper can invent text**, especially on silence or noise. That is why the text always goes
  into the message box for review first.
- **In the browser.** The transcript is text in the message box: like typed text, an unsent
  draft is kept in the browser's local storage for that chat until it is sent or cleared. The
  audio is never stored in the browser. A crash of whisper-server could leave a core dump with
  audio in it, under your system's core dump settings.
- **Doctor.** The `voice` row is about dictation: off, or on with the socket, the clip limit, the
  whisper tool and its port, which read-aloud the page uses, and the logins with no dictation; it
  fails if the jail could reach the port. The `voice tool` and `voice model` rows run for any configured whisper tool, with
  `remote.voice` on or off: with it on they check the tool that serves `remote.voice.socket`, with
  it off the first whisper tool in `broker.tools` (agents only). The `voice tool` row asks the
  tool's dictation socket for its health: not running, running with whisper-server not ready
  (starting, or the program or the model failed a check; the reason is in the tool's log), or
  ready with the model. The `voice model` row fails when the model is missing from `-models`, is
  not the pinned file, or lever's table entry for it is not verified.

### Server voices

The browsers' on-device voices vary: on an iPhone, Safari offers no Siri voice to a web page, and
its voices sound robotic. With `read_aloud: server`, the page reads with a neural voice that runs
on your host instead: [Kokoro](/tts-tool/) (American and British English voices) through the
first-party broker tool [`lever-tool-tts`](/tts-tool/), which runs sherpa-onnx. The text goes only
to your own host, never to a cloud speech service.

1. **Install sherpa-onnx's `sherpa-onnx-offline-tts` yourself**, outside the tree, owned by you or
   root and writable by no one else ([how](/tts-tool/#install-sherpa-onnx)).
2. **Add the tool to `broker.tools`** with `-tree`, `-models`, `-server` and `-speak-socket`
   ([every flag](/tts-tool/#configure-the-tool)).
3. **Set `remote.voice.read_aloud: server`** and `speak_socket` to the tool's `-speak-socket`.
   Config load refuses a `speak_socket` that is no tts tool's `-speak-socket`, or that is the
   dictation socket.
4. **Fetch the model:** `lever voice fetch kokoro-en-v0_19`.
5. **Reload and check:** `lever reload`, then the `tts tool` and `tts model` rows of
   `lever doctor`.

- **The page.** While the tool answers ready (the proxy asks at most every 5 seconds), the agent
  list carries its voices, and the speaker button reads with them. The page cleans the text as for
  the device's voice, splits it into parts of whole sentences (the first one short, so the voice
  starts within about a second for a short message; the others at most the tool's `-max-chars`)
  and asks `POST /lever/api/voice/speak` for one part at a time, the next while one plays. It plays
  each answer with Web Audio, so it plays on iPhone Safari too, even with the ring switch on silent
  (Safari 16.4 and later). If the first part cannot be had (the tool is down, busy or over a limit),
  the device's on-device voice reads the whole message instead; a later failure stops with a note.
  A tap stops it, and so does hiding or leaving the page. The voice picker lists the server voices;
  the choice is kept per device, apart from the device-voice choice.
- **The route.** Only for a verified login with read-aloud (`voice: false` turns it off), with
  `X-Lever-Voice: 1` from the page's own origin, a JSON body `{agent, id, text, voice}` of at most
  64 KiB, an `agent` the login's list shows and an `id` of the message the text is from (both for
  the audit line), a `voice` of the tool's, and at most `-max-chars` characters of text. The page
  sends the text it shows, not only a reference: a lookup on the server would have to repeat every
  rule of what a login is shown, and a mistake there would read out a message the login may not
  see. The text is bounded instead. The answer is `audio/wav`, or `{"error": "<word>"}`:
  `voice-off`, `origin` and `agent` (403), `bad-request`, `bad-voice` and `bad-text` (400 or
  415), `too-long` (413), `timeout` (408), `rate`, `quota` and `busy` (429), `unavailable` (503).
- **Limits.** Per login, 40,000 characters an hour, 200,000 a day and 1,200 requests an hour, in
  memory. Three requests at a time, of which a contact never takes the last; one per login; one
  with the engine at a time (separate from dictation's: the engine runs on the CPU). A text the
  engine never got does not count.
- **Privacy.** The text and the audio are held in memory only, by the proxy and the tool; the
  engine gets the text as its argument (a local user who can list processes with their arguments
  can read it while a part is spoken) and writes the audio to a private directory of the tool's,
  which the tool reads and removes at once. The proxy's audit line (`voice-speak` or `deny-speak`) has the login, the agent, the
  message id, the length, the outcome and the latency; the tool's log line the length, the voice,
  the outcome and the latency. Never the text, never the audio.
- **Agent text is untrusted input to the engine.** It is bounded, control characters and markup
  are removed, and it is never passed as an option. lever gives the engine no path in the tree
  and expects it to use no network, but it is not sandboxed: it runs as your user, like
  whisper-server, so install a sherpa-onnx you trust. The jail reaches neither the speech socket nor the engine, which has no port.
- **Doctor.** The `tts tool` row asks the speech socket for its health; the `tts model` row
  hashes every file of the model against the manifest of its verified archive. See the [tts tool](/tts-tool/) guide.

## What this does NOT do

- **No lifecycle or fleet management from the phone.** Worker dispatch stays a manager action —
  asked for in chat, like any other task — not a button the phone UI exposes as an operator
  capability of its own. The one exception is narrow: a message to an asleep worker
  [wakes it](#waking-a-worker) (resume only, a worker the login may message, once a minute). The
  page cannot start, create, stop or remove an agent.
- **No new authority.** A remote chat message is text in the agent's session. With verified
  web chat it is provably yours, but it is still not a directive (see the accepted posture above).
- **No credential on the phone.** The session cookie stays on the host, the hub's `Set-Cookie` is
  stripped from every response, and the routes that would return a token are refused.
- **No identity provider, and nothing in the jail that mints sessions.** Only an assertion signed
  with the host-held key makes a hub session, and the proxy signs only for configured logins — see
  [how the browser is logged in](#how-the-browser-is-logged-in).

## See also

- [Config reference](/reference/config/) — the full `remote:` key table.
- [CLI reference](/reference/cli/) — `lever remote serve` / `lever remote status`.
- [Security model: worker isolation](/security-model/worker-isolation/) — lever's hub identities
  and the controller PAT.
- [Operator directives](/operator-directives/) — the one channel that IS authenticated, if you need
  that instead of (or alongside) chat.
