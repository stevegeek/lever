# Changelog

All notable changes to lever are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Process: every merge
to `main` that changes behavior adds an entry under `## [0.12.0] - 2026-07-31`; a
version bump moves the block under the new version heading.

## [Unreleased]

A code-quality refactor across the module. No new features; the user-visible
differences below are side effects of tightening seams. Contributors should
read the Changed/Internal entries before rebasing open branches.

### Changed

- **Broker jail and admin routes are method-restricted and body-capped.**
  Every route is registered with a method pattern, so a request with the
  wrong method gets 405 instead of being decoded. Request bodies are read
  through `http.MaxBytesReader`: 64 KiB for jail and admin JSON, 4 KiB for
  the small verbs, 256 KiB for signed envelopes, 4 MiB for the tool gateway.
- **captool and the agent's MCP server cap a message at 1 MiB**
  (`mcp.MaxBodyBytes`): the HTTP transport reads bodies through
  `http.MaxBytesReader`, and `lever-agent serve-capability`'s stdio
  transport now sizes its line scanner to the same cap (it was
  `bufio.Scanner`'s 64 KiB default, so a line between 64 KiB and 1 MiB that
  used to end the session is now answered; a longer line still ends it with
  `bufio.ErrTooLong`). The MCP `initialize` reply reports
  `captool.Config.Version`; `lever-tool-db` now sets it from a build-time
  `main.Version` (the Makefile and `tools/test/lima-e2e.sh` stamp it with
  `-ldflags`), so a `make`-built tool reports the lever version and a plain
  `go build` reports `dev`.
- **`lever apply --dry-run` no longer lists a jail-up step.** The step was a
  no-op on every production path: apply brings the machine up eagerly, before
  planning, to resolve the run user. The machine still comes up; only the
  phantom step is gone.
- **`broker.api_key_file` is resolved against the config file's directory**
  when relative, like every other path in `lever.yaml`. An absolute path is
  unchanged.
- **The bridge events file is created owner-only** (0600, directory 0700).
  An existing file keeps its mode; delete it to adopt the new one.
- **The per-agent `settings.json` env block no longer carries
  `CLAUDE_CODE_CLIENT_CERT`, `CLAUDE_CODE_CLIENT_KEY` or `NODE_EXTRA_CA_CERTS`.**
  Claude reaches the broker through the loopback gateway and never presented
  the leaf itself. The image still exports the same paths as container env for
  other in-container tooling.
- **Web assets are tarred by lever itself** (`archive/tar`) instead of
  shelling out to the host `tar`, so the archive is identical on every host.
- **`LEVER_FORCE_HOST_NETWORK` is read once, when the jail transport is
  constructed**, not on every call. Set it before `lever apply`, not during.
- **`lever apply` talks to the broker admin socket with a 5 s client
  timeout** instead of an unbounded one.
- **Some CLI error strings changed.** Broker calls from `lever` and
  `lever-agent` go through one JSON-over-HTTP helper (`internal/httpjson`),
  so a failed call now reports the single `POST <url>: <status>: <body>`
  shape rather than per-call wording. Scripts that matched the old text need
  updating.
- **`lever-agent` binaries report a version.** The Makefile passes
  `-ldflags` (`LEVER_AGENT_LDFLAGS`) into every `lever-agent` build.
- **Broker `/worker/*` routes authenticate before they decode.** A request
  from a caller that is not the manager gets 403 whatever its body; only an
  authenticated caller with a malformed body gets 400 (it used to be 400
  for both).
- **Broker worker start/resume report a ticket-staging failure as
  `stage error`** (500) for every step; the old `ticket error` body for the
  mint step is gone.
- **Signed admin-op envelope rejections are audited with a specific
  reason.** `opsig.ParseEnvelope` names the failing field (`version N`,
  `instance mismatch`, `op`) where the audit line used to say `envelope
  fields`. The HTTP response is unchanged (`invalid envelope`, 400).
- **`lever apply`'s bootstrap-token step removes scion's residual dev-token
  on failure too.** The throwaway dev-auth hub's `~/.scion/dev-token` is now
  deleted from the deferred cleanup, so an aborted mint no longer leaves the
  open admin credential behind; before, only a successful mint removed it.

### Internal

- `internal/exec` is renamed to `internal/proc` so it no longer shadows `os/exec` (the `leverexec` alias is gone).

- **Integration tests are behind `-tags integration`.** The live VM, hubapi
  and `lever-tool-db` host tests no longer run under `go test ./...`; run
  `make test-integration` (or `go test -tags integration ./...`).
- **New packages:** `internal/retry` (one bounded poll loop, `retry.Until`),
  `internal/httpjson` (JSON-over-HTTP client), `internal/mcp` (JSON-RPC
  framing and `tools/call` projection shared by the agent server and
  captool), `internal/state` (host state dir), `internal/daemon` (host daemon
  bookkeeping), `internal/provision/*` (host build pipelines moved out of
  `internal/guest`), `internal/scion/layout` (scion's on-disk paths and
  settings keys), `internal/agent` (lever-agent domain logic moved out of
  `cmd/lever-agent`), and `internal/cli/host` + `internal/cli/manager` (the
  host and in-jail halves of `internal/cli`).
- **Wire contract in `internal/wire`.** Broker request/response types and
  route paths are declared once and used by the broker, the CLI, the agent
  and captool. `wire` is a leaf package; list replies are generic
  (`wire.WorkerListResponse[T]`, `wire.MsgListResponse[T]`) so
  `lever-manager` no longer links `internal/broker`.
- **`internal/config` split by concern; `Validate` is pure.** Host probes
  (binary presence, file modes, toolchain access) moved to `CheckHost`;
  `LoadNoHostChecks` exists for tests. `config` no longer imports
  `internal/backend`.
- **`broker.Config` is grouped into sub-structs** (`Identity`, `Persistence`,
  `LLM`, `Dispatch`, `Directives`). `Config.Agents` is gone; the declared
  `Workers` are the source of truth.
- **Smaller seams:** `exec.Runner` gained `RunStdin`; `internal/jail` is a
  pure transport; `backend.NewBase` holds private state with a shared
  version gate and machine status; `opsig.Sign`/`Verify` are ctx-bounded
  (the old un-bounded wrappers are deleted); `apply.Run` requires every
  `Deps` collaborator; `remoteproxy` has one hub URL in `LoginDriver` and a
  typed audit `Decision`; doctor checks are table-driven.
- The broker and proxy pid files and the remote stamp are written atomically
  (temp file + rename), so a crash mid-write leaves no torn file.
- **The remote stamp hash shape changed.** `lever remote serve` now records
  a `state.RemoteIdentity` digest instead of a digest of `config.Remote`; the
  covered fields are the same. The first `lever apply` after upgrading sees
  a mismatch and restarts the proxy once.
- Dead code reachable only from tests is deleted (including
  `agent.MCPServer.Handler`); staticcheck and gofmt are clean;
  `CONTRIBUTING.md` lists every `internal/` package.

## [0.19.0] - 2026-08-23

### Added

- **Web UI sessions now survive hub restarts.** Scion signs its session cookie
  with a random per-boot key when none is configured, so every hub restart
  signed every browser out — on a live instance, ~8 silent sign-outs in one
  day, each a blank dashboard behind the remote proxy. lever now generates a
  32-byte key on first apply, persists it at `.lever-state/session-secret`
  (0600), and passes it to every hub start as `--session-secret=`. The flag,
  not the env var, because scion's daemon persists its argv to
  `~/.scion/server-args.json` and replays it on restart — the argv is the
  durable channel.

  **On upgrade**, the next hub restart adopts the key; the apply that
  introduces it does not force one (a forced restart would drop every agent's
  hub connection to cause the very logout it prevents). That next restart is
  the last random-key logout — unless the operator pre-seeds
  `.lever-state/session-secret` with the currently-live value first, in which
  case there is none. lever never rewrites the file; deleting it is the
  rotation mechanism, and the next apply generates a fresh key.

### Fixed

- **The web UI's Sign-in button no longer dead-ends on a lever.invalid 404.**
  The button navigates to `/auth/login/<provider>`, and the remote proxy
  forwarded that, so the hub's 302 to the OIDC authorization endpoint — which
  deliberately does not resolve; the proxy performs the whole login
  server-side — reached the browser. The proxy's session-retry gate never saw
  it: it recognises the hub *rejecting* a session (401, or a redirect back to
  the login page), not a redirect pointing outward. The proxy now answers
  `/auth/login` and `/auth/login/*` navigations itself: it runs the same
  server-side login the retry gate uses, then 302s the browser back into the
  app, honouring the SPA's `returnTo` target for in-app paths.
  `/auth/callback/*` still reaches the hub. The path was unreachable while hub
  sessions never lapsed; the session-secret change above makes lapses rare
  again, but the button now works whenever it is shown.

## [0.18.1] - 2026-08-22

### Changed

- The scaffolded `lever-operator` skill now teaches the chat reply convention.
  Its Messaging section said to "answer here", which is right only when the
  message came from the session. A message delivered over a chat channel
  carries `channel` and `thread_id` in its envelope and must be answered with
  `scion message "<sender>" --channel <channel> --thread-id <thread_id>`; an
  agent that does not know this acts on the request and appears to ignore it,
  because the automatic per-turn mirror lands in a different surface and
  carries no thread. Also states the `instruction` / `mention` distinction —
  scion's protocol treats a mention as FYI — and that a chat message is still
  owner-tier data, so an off-remit request is declined *in the thread* rather
  than silently.

  **Existing instances need `lever init` to pick this up**; the scaffold is
  copied at init, not read from the binary at runtime.

  This belongs with 0.18.0 and was cut minutes too late.

## [0.18.0] - 2026-08-22

### Added

- **Native chat now works out of the box.** lever writes
  `server.message_broker.enabled: true` into the jail's `~/.scion/settings.yaml`
  when the key is absent. Scion registers the `/api/v1/chat/*` routes by default
  but wires the store behind them inside
  `if MessageBroker != nil && MessageBroker.Enabled`, so with the key unset —
  scion's own zero value — the web channel spoke never registers and native chat
  answers `503 "Chat preferences not available"` on a hub whose chat UI is
  present and inviting. The feature looked shipped and was inert.

  Absent-only, never an override: an operator who has written `enabled: false`
  has made a choice, and re-applying does not undo it. Same rule
  `setOperatorDisplayName` already follows for a name the operator set.

  **On upgrade**, the next `lever apply` restarts the hub to read the new key
  and the guest gains an attachment store at `~/.scion/attachments`. To opt out,
  set `server.message_broker.enabled: false` in the jail's settings before
  applying.

### Note for instance authors

Scion delivers a chat message to an agent with a `channel` and `thread_id` in
its envelope, and the agent is expected to answer with
`scion message "<sender>" --channel <channel> --thread-id <thread_id>`. An agent
that does not know this convention acts on the request and appears to ignore it,
because the automatic per-turn mirror lands in the Messages inbox and carries no
thread. Worth stating in your agent's instructions; lever does not scaffold it.

### Changed

Two corrections that landed just after the 0.17.1 cut. Neither changes
behaviour, and the 0.17.1 binaries are unaffected.

- Corrected the converge-off rationale in `internal/apply/run.go`. It asserted
  that Scion offers no way to read the running hub's argv; `daemon.SaveArgs`
  writes `server-args.json`, which outlives the daemon (`RemoveArgs` has no
  production callers). The comment now says lever infers the transition from
  guest state *by choice*, gives both reasons, and names what reading the argv
  would buy — a changed web port or assets dir, which the guest signal cannot
  see. The half-failed-apply residual is stated with its repair.
- Recorded the `EnableWeb` correction in the comment-drift note as its own
  class: it never drifted, it made a claim about Scion that no local change can
  invalidate and no local review will re-read. Added
  `TestServerStartNeverDisablesTheWebFrontend`, which guards the dangerous half
  — the flag must be OMITTED, never sent as `--enable-web=false`, since that
  moves the Hub API off the web port.

## [0.17.1] - 2026-08-22

A follow-up to 0.17.0, from two independent reviews of that release. No config
changes and no new surfaces — every entry is a defect in what 0.17.0 shipped.

### Fixed

- **A panic during login no longer wedges an operator out.** The single-flight
  that stops concurrent requests racing the same handshake now releases on
  every exit path. A panic previously left the entry published with its channel
  open, so every later request for that operator blocked until its own context
  expired — for the life of the process, since nothing removed the entry. The
  panic is deliberately not recovered: it keeps unwinding to `net/http`, which
  logs it with the stack trace that says what actually broke.
- **Audit lines are bounded.** Caller-chosen fields — the request path and the
  Tailscale login — were written whole, on the path that runs *before* any
  identity check, so a few thousand long requests could append gigabytes.
  Truncation is applied to the audited copy only; decisions are still made on
  the original, because a truncated login would collide with every other login
  sharing its first bytes.
- **`lever apply` restarts the hub when remote access is turned off.** The hub
  reads `oidc_login` once, at startup, so turning the feature off left it
  offering a login whose provider was gone until something unrelated restarted
  it. The ON path already restarted on change; the OFF path now does too.
- **A hand-started proxy can no longer satisfy apply's reuse check.** The pid
  file is written by any `lever remote serve`, while the stamp was written only
  by apply — so a proxy started against a different config, on top of an old
  stamp, looked like a match. The running proxy now stamps what it is actually
  serving and names its own pid.
- **`manager.allow_ports` refuses an entry naming the remote proxy port**,
  instead of silently granting a jailed agent a route to it.
- **A race in `lever remote serve`'s server list**, found while fixing the
  above.
- **`lever doctor` diagnoses the login path before probing `/healthz`.** The
  health probe depends on the login working, so a broken login surfaced as a
  vague healthz failure instead of the specific check that names the cause.

### Documentation

- **`ServerOpts.EnableWeb`'s doc was false**, and dangerously so: it claimed a
  headless hub stays API-only. Scion turns `--enable-web` on for every
  non-hosted `server start` that does not pass it, and with the web frontend
  off the Hub API leaves the web port for `hub.port` — which would take an
  instance down, since lever puts the broker, agents' `SCION_HUB_ENDPOINT`,
  doctor and the proxy on that port. Corrected, with the dependency written
  down for the first time.
- The converge-off path now states what it deliberately does **not** undo (the
  overlay agent template, and the staged SPA — nothing serves it once the
  restart drops `--web-assets-dir`), and the residual it cannot close: an apply
  that fails between the guest edit and the restart leaves the old hub running
  until a real change or a `lever stop` + `up`.
- `isAPIPath` reading the decoded path was checked against `net/http`'s
  ServeMux and recorded as safe rather than changed.

### Tests

Filled the gaps the review named: a template test that mocked the function
under test now runs the real script; the teardown verb, the removal of
`oidc_login` with no forwarder left, and a live-but-not-listening proxy are all
pinned.

## [0.17.0] - 2026-08-22

### Added

- **Remote access (`lever remote`)** — talk to the manager, and attach to any
  running agent, from a phone over Tailscale. A new host-side reverse proxy
  (`internal/remoteproxy`) fronts the Scion hub's web UI: it injects a
  dedicated, narrowly-scoped remote PAT (`agent:read`, `agent:list`,
  `project:read`, `agent:attach` — no create/delete/manage/secret scope) on
  every forwarded request, strips any client-supplied `Authorization`/
  `Cookie`/`Tailscale-*` header before forwarding, and strips the hub's own
  session cookie from every response, so the client never holds a hub
  credential of its own. Origin and `Sec-Fetch-Site` checks reject cross-site
  browser requests; an optional `remote.allowed_users` pins the tailnet
  identity (`Tailscale-User-Login`, set only by `tailscale serve`) allowed to
  connect. Every request is appended to a host-side audit log
  (`.lever-state/remote-audit.jsonl`).
  - **Accepted step-1 posture:** the full hub UI is exposed to the tailnet —
    whoever reaches the proxy gets the same interactive power over the jail
    interior (agents, mounted tree, LLM spend) as the operator's phone. The
    host stays VM-protected regardless; no agent gains any new capability;
    directives remain the only *authenticated* operator override (remote chat
    arrives as an ordinary unauthenticated `user:` turn, same as `lever msg
    send`). See the new remote-access guide (`docs-site/_guides/remote-access.md`)
    for the full threat-model recap and setup.
  - New config block `remote:` (`enabled`, `port` default 8445, `base_url`,
    `allowed_users`); new CLI `lever remote serve|status`; lifecycle wired into
    `apply`/`stop` like the broker daemon. **Requires the `orbstack` backend**
    — `remote.enabled: true` on any other backend is rejected at config load,
    because the Lima path is not live-validated yet. This closes a trap:
    without the check, a `lima` + `remote.enabled` config loaded fine and
    `apply` returned 0 while the proxy child silently died into `remote.log`.
    `lever remote serve` keeps the same check at runtime too, as
    belt-and-braces defense-in-depth.
  - **`remote.base_url` configures the proxy only; it is never passed to the
    hub.** Scion's `--base-url` is not a web-only flag: the hub adopts it as
    its own agent-facing endpoint and injects it into every agent container as
    `SCION_HUB_ENDPOINT`/`SCION_HUB_URL`. A jailed agent cannot reach a tailnet
    name (no DNS for it inside the jail, and lever's egress drops `100.64/10`),
    so forwarding it handed every agent a hub address it could never call back
    on — breaking status updates, notifications, and the ~10-hourly agent token
    refresh. It also failed silently: Scion rewrites a LOOPBACK hub endpoint to
    the container-reachable `host.containers.internal` form that lever's pasta
    `--map-host-loopback` mapping serves, and a non-loopback endpoint skips
    that rewrite entirely. lever therefore starts the hub with `--enable-web`
    alone. The SPA does not need the flag — it builds every URL relative — and
    its only other consumers (the session cookie's `Secure` flag, the OAuth
    redirect URI) are unreachable here, since the proxy strips the hub's
    session cookie and injects a PAT rather than running an OAuth login.
    Regression-tested at the level that matters: the test asserts the endpoint
    AGENTS receive, not the flags lever emits — argv-shaped tests passed
    throughout the bug.
  - **A `scion.version:` pin now actually serves the UI.** Upstream tracks only
    `web/dist/client/.gitkeep` and `.gitignore`s the built output, so a binary
    compiled from a fetched module (or a `source:` checkout that was never
    `make web`-ed) has an EMPTY embedded asset filesystem: `--enable-web`
    served Scion's bare "Web UI Not Available — built without embedded web
    assets" page, and `GET /assets/main.js` 404ed. The fetched Go module does
    carry the full npm project, so when `remote.enabled`, lever now runs
    `npm ci && npm run build` **on the host** against that same source tree and
    starts the hub with `--web-assets-dir` pointing at the staged output. One
    pin, one download: the SPA is built from exactly the tree the pinned binary
    was compiled from, so the two cannot disagree.
    - Host-side for the same reason scion itself is cross-compiled host-side —
      the guest carries no toolchain, and giving it one would also hand the
      jail npm's registry egress.
    - **Cached per pin.** The build directory is keyed by a digest of scion's
      web sources (build inputs only — `node_modules`, `dist` and the
      npm-generated `public/assets`+`public/shoelace` are excluded, or a
      `source:` checkout's key would change on every build). An unchanged pin
      re-applies in milliseconds with no npm run; a `source:` edit rebuilds,
      keyed by content rather than by a pin string. Cache lives under
      `~/Library/Caches/lever/scion-web/` (macOS) or `~/.cache/lever/scion-web/`
      (Linux); staged into the guest at `/usr/local/share/scion/web`, and
      re-staging is skipped when the guest already holds that digest.
    - **Sizes, and no auto-prune.** The staged payload is ~3.3MB: vite's
      sourcemaps are stripped from it, being 71% of the build output (8.2MB of
      11.5MB at pin `e82a2a08`) and existing to debug a SPA lever does not
      develop. The host cache is ~210MB per distinct pin and lever never prunes
      it — several remote-enabled instances share that cache, so deleting "the
      pins I am not using" on every apply would have two instances on different
      pins thrashing each other's builds. Deleting the cache (or any `<digest>`
      in it) is always safe.
    - **New prerequisite: node >= 20 + npm on the host**, but only for
      `remote.enabled` instances on `version:`/`source:`. A new `lever doctor`
      check reports it, and `lever apply` fails early and by name — before
      copying any sources — rather than letting a missing toolchain surface as
      the "Web UI Not Available" page in a browser hours later. The message
      names the asdf/mise dead-shim case explicitly (it exits 126 with no
      useful text), the same trap the existing `go toolchain` check covers.
    - **`scion.binary:` is exempt**, deliberately and not as an error: lever has
      no source to build the SPA from, and an operator-built binary may already
      embed one (upstream's `make all` does). lever builds nothing and passes no
      `--web-assets-dir`, so those embedded assets keep serving — Scion treats
      any non-empty value as an override that REPLACES the embedded filesystem
      rather than falling back to it, so a flag pointing at an unstaged path
      would be worse than no flag at all. One predicate
      (`config.App.ScionWebAssets`) drives the build, the staging and the flag
      so the three cannot disagree.
  - **The browser gets a hub session from a local OIDC provider lever runs
    itself, with NO endpoint that can mint one.** The injected PAT opens the
    hub's API but cannot open its UI shell: Scion's web layer authenticates a
    browser by a `scion_sess` cookie alone and never reads `Authorization`
    (`pkg/hub/web.go` sessionAuthMiddleware), so with dev-auth off and no
    external IdP the SPA sat at 401. `lever remote serve` now also serves a
    minimal OIDC provider on its own host loopback port (`remote.login_port`,
    default 8447) and performs the login SERVER-SIDE: it GETs the hub's
    `/auth/login/oidc` with its own cookie jar without following the redirect,
    mints an authorization code **by an in-process function call**, GETs the
    hub's own callback with that code, and keeps the session cookie host-side.
    - **`/authorize` is a registered route that returns 404, permanently, and
      every hit is audited.** Scion's OIDC login validates NOTHING — no
      `id_token` is requested or parsed, no JWKS fetched, no PKCE, no nonce, no
      client secret, and the discovery document's issuer is never compared to
      the configured one — so whoever can obtain an authorization code can have
      the hub mint them a session as any identity. The whole security of this
      rests on codes being creatable only in-process, on the host, at the same
      trust level as `remote.pat`. That matters because the provider IS
      reachable from inside the jail: the hub only accepts a loopback issuer
      (it validates `issuer_url` at startup and refuses to start otherwise), so
      a logic-free forwarder in the guest carries guest `127.0.0.1:8446` to
      the provider's host port — and lever maps guest loopback into every agent netns at
      `169.254.1.2`. An agent therefore reaches discovery, `/token` and
      `/userinfo`, none of which yields anything without a code. A working
      `/authorize` would have turned that into "any jailed agent can become a
      hub user". The route exists, rather than simply being absent, so its
      absence cannot read as an oversight somebody later "finishes"; discovery
      advertises `https://lever.invalid/authorize` (Scion requires the field to
      be present but never dials it), and `lever doctor` checks both that
      discovery is served and that `/authorize` still 404s.
    - **Codes are bound and single-use**: 32 bytes from `crypto/rand`, bound to
      the login attempt's `state`, `redirect_uri` and `client_id`, consumed on
      presentation whatever the outcome, 60-second TTL.
    - **The session never widens the phone's reach.** It is attached to
      UI-shell requests only; everything under `/api/v1` keeps riding the
      narrow remote PAT, since Scion's `sessionToBearerMiddleware` passes a
      request through untouched when it carries an `Authorization` header. The
      cookie stays host-side and the hub's `Set-Cookie` is still stripped from
      every response.
    - **The identity asserted is the one the proxy already verified** — the
      Tailscale login matched against `allowed_users` — so the hub's user row
      names the operator who connected, and two operators get two sessions.
      `admin_emails` is never set, so users are created at Scion's `member`
      role. Sessions are obtained lazily, shared by concurrent requests (a page
      load opens several connections at once), and renewed transparently when
      the hub stops honoring one — a restarted hub costs one silent re-login,
      not a login page the operator cannot complete.
    - **The guest half**: a ~90-line stdlib forwarder, cross-compiled for the
      guest's architecture from source embedded in lever and installed with the
      same hash-skip as the scion binary. It carries no logic beyond
      forwarding, and refuses a non-loopback listen address. Enabling remote
      access therefore also needs a **Go toolchain on the host** — already true
      for `scion.version:`/`source:`, newly true for `binary:`.
    - **The hub restarts once, on change only.** Scion reads `oidc_login` at
      startup, so `lever apply` writes the block into the guest's
      `~/.scion/settings.yaml` and restarts the hub when that file actually
      changed; an unchanged config restarts nothing, since a restart drops
      every agent's hub connection for its duration. lever refuses to add a
      `server:` key beside an unmigrated legacy `server.yaml`, which would
      silently make Scion ignore that file entirely.
    - **The UI lands on the dashboard, not a setup wizard.** The same write
      names an operator in `server.auth.display_name`. Scion's SPA redirects
      every fresh load to `/onboarding` until `GET /api/v1/system/status`
      reports complete, and that requires `IdentitySet` — true only when
      `server.auth` names a display_name, email or username. A hub lever had
      fully provisioned therefore greeted the operator with a first-run wizard
      on every load. The value is inert: those fields reach Scion only as
      `hub.DevUserConfig`, which it reads under `if cfg.DevAuthToken != ""`,
      and lever runs `--dev-auth=false`. lever only ever ADDS it — an existing
      identity is an operator's own and is never overwritten or renamed — and
      turning remote access off removes it only while it still holds lever's
      exact value.
  - **The proxy reaches the hub THROUGH its own jail**, not over a host port.
    The hub binds `127.0.0.1:8080` INSIDE the jail; a host-side address that
    appears to reach it is an artifact of OrbStack's port forwarding, landing
    on whichever machine claimed the port — not necessarily this instance's.
    The proxy now runs `nc 127.0.0.1 8080` in its own jail and adapts that
    child process to a connection (real deadlines, HTTP keep-alive, and the
    hub's WebSocket attach streams all carried), which is the same
    everything-goes-through-the-jail rule `internal/hubapi` already followed.
    Consequence: **several remote-enabled instances can now run on one host**,
    each proxy reaching its own hub instead of contending for one forwarded
    port. Each still needs its own `remote.port` and its own `tailscale serve`
    mapping. A dial that fails reports why — jail down, `nc` missing from the
    guest, hub refusing the connection — in `remote.log` and in a new `error`
    field on the audit line, rather than as a bare `502`. A hub that accepts
    the connection but never sends headers is bounded at 45s, so a wedged
    machine yields that same diagnosable 502 instead of a request that hangs
    forever; the bound is on headers only, so streamed transcripts and
    attach streams are unaffected.
- **`netcat-openbsd` is now a declared jail prereq**, beside `curl`, in the
  guest provisioning package list. Both were already present in the base
  image, so this installs nothing new on an existing guest (the `dpkg -s`
  guard still passes and apt still never re-runs) — it names the dependency so
  a future base image cannot drop it and break a lever feature silently. `curl`
  carries every hub call from inside the jail; `netcat-openbsd` carries the
  remote proxy's hub dial.
  - `lever doctor` gains a "remote access" check when enabled: proxy pid
    alive and actually listening, the PAT present at `0600`, and an
    end-to-end `GET /healthz` **through** the proxy returning 200 — proving
    the whole chain (loopback listener, origin/identity gates, PAT
    injection, hub) actually works, not just that a process is running.
  - **Deviation: PAT expiry is not checked.** The hub's default token
    lifetime is 90 days; lever has no cheap way to read a token's remaining
    lifetime back from the hub in v1, so neither `doctor` nor `status` warns
    before it lapses. If remote access starts failing auth for no other
    reason, re-mint by deleting `.lever-state/remote.pat` and re-running
    `lever apply`.
  - **Upgrading an existing instance:** the first `apply` after flipping
    `remote.enabled: true` on an instance that was already bootstrapped
    reopens a brief, jail-loopback-only, dev-auth-on mint window to mint the
    remote PAT — the same shape as the documented controller-PAT re-mint
    repair. On a fresh bootstrap with `remote.enabled` already set, this
    happens in the same window as the controller-PAT mint; no extra window
    opens.

### Changed

- **Agent image bakes Claude Code 2.1.239** (was 2.1.226). Needs an image
  rebuild to take effect, then `lever apply` and — for a running manager —
  `lever stop && lever up`.

### Fixed

- **The proxy validates the `Host` header** (DNS-rebinding defence). The origin
  gate checked `Origin` and `Sec-Fetch-Site`, but both are only present when the
  client chooses to send them, and binding to loopback is no defence when
  loopback is what a rebind targets: a page on an attacker's own name, rebound
  to `127.0.0.1`, is same-origin to the browser, so it sends no `Origin`,
  `Sec-Fetch-Site: same-origin`, and any header it likes — including a forged
  `Tailscale-User-Login` — and reads a reply carrying the injected PAT's
  authority. The proxy now answers only to the tailnet name in `base_url`, or
  to loopback on its own port (for `lever doctor`'s health probe). Found by an
  independent review of this branch and reproduced against a live proxy.
- **Teardown stops the remote proxy** and removes its stamp. `destroy` stopped
  the broker but not the proxy, so a full teardown left two loopback listeners
  running and a later bring-up *reused* the pre-teardown process, whose cached
  jail prefix names a machine that no longer exists.
- **Turning remote access off converges even after a failed attempt.** The
  guest settings edit was gated on having found the login forwarder binary,
  which the same script removes first — so one failed edit left the
  `oidc_login` block in the guest permanently, with no verb that removed it.
- **Scion config values are read from stdout alone.** Scion's settings loader
  writes warnings to stderr, and lever folded both streams together; a warning
  prepended to `config get default_template` made lever read it as an
  operator's own template choice and silently skip the placeholder-prompt fix,
  while `apply` logged that it had applied it.
- **`lever apply` now restarts the remote proxy when its config changed.** The
  proxy reads the `remote:` block once and caches all of it in the handler it
  builds at startup — ServeHost from `base_url`, the allowed-user set by value,
  the bound ports — so a running proxy keeps enforcing the config it was born
  with. Start's reuse shortcut compared only "pid alive + port accepting", so
  editing `remote:` and re-applying reported success while changing nothing.
  Found live: enabling `allowed_users` left the old process serving, and
  identity-free requests kept returning 200. A running proxy is now reused only
  when a stamp beside `remote.pid` records the same lever version AND the same
  remote config; otherwise apply stops it and spawns a replacement. This is what
  `brokerController.Start` has always done via the broker's `/epoch` — the proxy
  has no such endpoint and must not grow one, since it fronts the hub.
- **Agents no longer launch with Scion's placeholder system prompt.** Scion's
  stock `default` template ships a `system-prompt.md` reading `# Placeholder`,
  and its claude harness declares `system_prompt_flag: "--system-prompt"` —
  which *replaces* Claude Code's built-in system prompt (the additive form is
  `--append-system-prompt`). Every agent provisioned from that template ran with
  the tool's entire behavioural contract replaced by two words. `lever apply`
  now installs an overlay template (`~/.scion/templates/lever/`) whose
  `system-prompt.md` is empty and points the project's `default_template` at
  it; Scion emits the flag only when the staged prompt is non-empty, so no flag
  is passed at all. Because the overlay is not named `default`, Scion still
  prepends the stock template as a base layer, so `agents.md`, `home/` and
  `skills/` keep tracking upstream — and the one directory Scion force-rewrites
  on every hub start is left alone.
  - **New agents only.** Scion stages an agent's system prompt once, when its
    home is provisioned, and never re-stages it. An agent that already exists
    keeps the prompt it was created with until its staged
    `.scion/harness/inputs/system-prompt.md` is emptied in place — which is safe
    to do live and costs no conversation.
  - lever claims `default_template` only while it is still Scion's own
    `default`; a template the operator chose is never overridden.

- **Agents now run Claude Code's classic renderer**, via
  `CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN=1` in the env overlay lever writes.
  Claude Code renders fullscreen by default, drawing on the terminal's
  alternate screen, which by definition has no scrollback. Every route to a
  lever agent is a PTY onto the container's tmux — `lever attach`, or Scion's
  web terminal over `lever remote` — and in both, fullscreen rendering destroys
  the scrollback the operator actually scrolls: tmux copy-mode cannot see
  alternate-screen content, and a browser terminal scrolls its own buffer,
  which holds the shell output sitting *behind* the alternate screen rather
  than the conversation. The operator cannot fix it from inside the session
  either — `/tui default` relaunches the process, and Claude Code refuses to
  relaunch a session carrying a `--system-prompt` replacement, which is exactly
  what Scion's harness passes. Takes effect on the next agent start.

## [0.16.0] - 2026-08-13

A correctness release. Every credential lever plants in the Scion Hub was
either undeliverable or corrupt on a current `scion` pin, and the two defects
cancelled each other, so nothing looked wrong until one was fixed alone.

### Upgrading

**Check your `scion` pin first.** The credential step needs `ce96122c`
(2026-08-10) or later. For `scion.version` the usable floor is `dbf52f22`
(2026-08-12): every commit from `4c045fc8` to `dbf52f22` carried both
`AGENTS.md` and `agents.md`, which the Go module proxy cannot fetch at all.
`scion.source` and `scion.binary` take the `ce96122c` floor directly. On an
older scion the credential step fails with `value must be base64-encoded`, and
lever names the pin rather than the symptom.

**Then check that the manager reads `running`** (`lever doctor`) before you run
`lever apply`. The credential step itself only rewrites two Hub rows and touches
no agent — but `apply` runs the whole plan, and `start-manager` on a record that
is not running takes the resume path, which recovers by deleting and recreating
the agent if the resume fails. That loses the conversation (lever#3). This
upgrade does not make that more likely; it is simply the wrong moment to
discover it.

**Nothing reaches a running agent until it restarts.** A container keeps the
environment it was given at create time, so an agent that is up now is
unaffected — for better and for worse. Picking up the corrected rows needs
`lever stop` + `lever up`, which is the resume path above.

**On a pre-`221d2eaf` scion this release changes nothing you can observe.**
`as_needed` was not yet filtered there, so the values were already being
delivered.

A failed credential step is not a clean rollback point in api-key mode: it
writes `LEVER_LLM_AUTH` before the placeholder, so an old pin leaves the first
value updated and the second not. Subscription instances write one value and are
unaffected. In both cases downgrading to 0.15.1 and re-applying restores the
previous state, because the injection mode is recomputed on every write.

### Fixed
- **Hub values lever plants for an agent were never delivered to it.** Both
  `SecretSet` and `EnvSet` wrote to the Hub without an injection mode, and scion
  normalises an unset mode to `as_needed` — which
  [scion#944](https://github.com/GoogleCloudPlatform/scion/pull/944)
  (`221d2eaf`, 2026-08-01) started filtering out of the projected container
  environment. The values sat in the Hub and never reached the agent. Both calls
  now pass `--always`.

  What this breaks, on any pin from `221d2eaf` on — note the first case covers
  every shipped example and any instance with a `credential_file`:

  - **`llm_auth: subscription` cannot bring an agent up either.** scion's claude
    harness declares `CLAUDE_CODE_OAUTH_TOKEN` as the required credential for
    auth type `oauth-token`, exactly as it declares `ANTHROPIC_API_KEY` for
    `api-key`, so an undelivered token fails the same pre-start gate.
  - **`llm_auth: api-key` cannot bring an agent up.** The placeholder
    `ANTHROPIC_API_KEY` that satisfies scion's start-time auth gate never
    arrives, so the harness provisioner finds no credential and the container
    aborts during pre-start (`Pre-start provisioning is required; aborting
    startup`). `lever up` reports it as `step start-manager: … failed to start
    agent via Hub`; a retry then reports `409 agent name is already in use by a
    stopped container`, because the first attempt leaves the crashed container
    behind. Reported as lever#28.
  - **`LEVER_LLM_AUTH=api-key` never reaches the agent's pre-start hook**, so
    the hook does not enter api-key mode. This half was silent: unlike the
    placeholder, `LEVER_LLM_AUTH` is not a key any harness declares as required,
    so scion's env-gather second pass never asks for it either.

  `SecretSet` now routes through `hub env set --secret --always` rather than
  `hub secret set`. Both write the same Hub secret row — scion's `--secret` flag
  redirects to the Secret API with `type=environment` — but `hub secret set`
  exposes no injection-mode flag at all, so it cannot express the requirement.
  The flags date from `5f56069e` (2026-02-11), well below lever's minimum
  supported pin.

- **Credentials were stored base64-encoded on a current scion, so the agent
  would have received a corrupt one.** lever base64-encoded every secret value
  because scion's secret API decoded it. `ce96122c`
  ([scion#1111](https://github.com/GoogleCloudPlatform/scion/pull/1111),
  2026-08-10) flipped that: the CLI now stamps `encoding=raw` on any value that
  did not come from `@file`, so the hub stores the argument verbatim. lever now
  sends plaintext.

  This is why the `--always` fix above could not ship on its own. The two
  defects were cancelling: the value in the Hub was the base64 *text* of the
  credential, and `as_needed` meant it was never delivered. Injecting it without
  fixing the encoding would have started delivering a credential that fails
  every model call — visible only as a 401 from inside the container.

  **`scion` must now be `ce96122c` or later.** An older pin rejects a plaintext
  value with `value must be base64-encoded`; lever catches that and names the
  pin floor rather than the symptom. The shipped examples move from `3142df68`
  to `e82a2a08`: `3142df68` sits in the worst window — after `as_needed`
  enforcement, before the encoding flip.

- **Secret redaction followed the argv change.** `redactArgs` matched the exact
  five-token `hub secret set KEY VALUE` shape, so the new argv would have put
  the secret value — now plaintext — into any error message scion returned. It
  masks both secret-bearing shapes, and a credential write additionally scrubs
  the literal value from the whole error, which position-based masking cannot do
  for a value that parses as a flag.

## [0.15.1] - 2026-08-11

### Fixed
- **`lever attach` could dial a hub that no longer exists.** Minting the
  controller PAT starts a THROWAWAY dev-auth hub on its own port and `hub
  link`s the project against it, which persists that port into the jail's
  project config. The register-project step's re-init would overwrite it, but
  that path is deliberately skipped when the registration is already sound — so
  re-minting a PAT on an established instance (the documented recovery for a
  pre-0.14.0 PAT) left the project pointing at a dead port.

  It stayed invisible because every lever call passes the hub endpoint
  explicitly; `attach` is the one verb that **execs** scion instead, so it alone
  fell back to that file. Bring-up, `doctor` and `up --no-attach` all passed
  while `lever up` failed at the moment it handed over the terminal.

  Both halves are fixed: `attach` now pins `SCION_HUB_ENDPOINT` alongside the
  token, so it no longer depends on jail state lever does not own; and
  register-project repairs a stale endpoint on the skip path, so the config
  itself stops being wrong. A repair that cannot run fails the bring-up rather
  than leaving the project pointing at a dead hub.

  **If you hit this on 0.15.0**, upgrading fixes it on the next `lever apply`.

## [0.15.0] - 2026-08-11

### Upgrading

Replace the host binary, then run `lever init` to re-scaffold the operator
skills — `lever doctor` reports them stale until you do.

**If you are not moving your `scion` pin, that is the whole upgrade.** The
pre-role guard is silent on a Scion older than scion#1089, because there are no
role-derived scopes to widen there. `lever doctor`'s new **agent authorization
roles** line tells you where you stand on any pin, so check it before planning a
bump.

**Three changes can fail a bring-up that used to succeed:**

1. **`manager.credential_file` must now be `0600`-tight.** A `0640`/`0660` token
   was previously accepted; it now fails at apply. `chmod 600` it.

2. **Config validation is stricter.** Two workers whose `dir`s overlap (the same
   subtree, or one an ancestor of the other), and a worker named `manager`, are
   now rejected at load. Rename or re-scope them.

3. **A controller PAT minted before 0.14.0 lacks `project:update`**, so the
   scratchpad strip fails with `HTTP 403: Insufficient permissions`. Delete
   `.lever-state/controller.pat` and run **`lever apply`** — *not* `lever up`,
   whose phase probe deliberately refuses to guess on an authentication failure
   and exits before reaching the re-mint step. Apply re-mints the PAT with the
   current scope set.

**Crossing scion#1089 needs a decision first.** Every agent record created by an
older Scion stores no role, and this release refuses to resume one rather than
let scion#1102 resolve it to `full`. lever cannot repair the record: the role is
written on the create path only, is immutable after, `scion resume` takes no
`--role` flag, and the hub exposes no route to set one. So either delete each
agent and let lever recreate it with `--role baseline` — **losing its
conversation** — or stamp the role onto the stored records yourself, with the
hub stopped:

```sh
scion server stop                       # in the jail; apply restarts it
cp ~/.scion/hub.db ~/.scion/hub.db.bak  # keep a way back
python3 - <<'EOF'
import json, os, sqlite3
db = sqlite3.connect(os.path.expanduser("~/.scion/hub.db"))
for agent_id, slug, applied in list(db.execute("select id, slug, applied_config from agents")):
    config = json.loads(applied) if applied else {}
    if config.get("agentRole"):
        continue
    config["agentRole"] = "baseline"
    db.execute("update agents set applied_config = ? where id = ?", (json.dumps(config), agent_id))
    print(f"{slug}: stored no role -> baseline")
db.commit()
EOF
```

`baseline` is what lever stamps on a fresh create, so this only ever narrows
what a record resolves to — it cannot widen one, and a record that already
stores a role is left alone. Re-run `lever apply` afterwards and confirm with
`lever doctor` that every record now carries a role.

### Security
- **A pin bump can no longer promote an existing agent to full hub authority.**
  0.14.0 made lever stamp `--role baseline` on every agent it *starts*. That
  covers creation and nothing else: Scion writes the role into the agent record
  on the create path only, it is immutable afterwards, and `scion resume` takes
  no `--role` flag. An agent record created by a Scion older than scion#1089
  therefore stores **no role at all** — and scion#1102 resolves an unset stored
  role to `full` (agent create, agent lifecycle, project-secret-read), at
  dispatch and, since scion#1101, on every token refresh.

  Bumping a pin across that boundary silently promoted every pre-existing agent,
  the manager included. `lever apply` now reads the hub's record before keeping
  an agent and **fails the bring-up** when the record stores no role while the
  installed Scion understands roles. An already-`running` agent is not exempt:
  it refreshes its own token on the same rule.

  lever cannot repair the record — the hub exposes no route to set a stored role
  — so the refusal is the whole remedy. The ways out are to delete the agent and
  let lever recreate it with `--role baseline`, **losing its conversation**, or
  to stay on a pin older than scion#1089. The guard deliberately does not fall
  into apply's delete-and-recreate recovery: that recovery is what destroys the
  session, so taking it automatically would spend the very thing the operator is
  being protected into keeping. It fails closed on anything it cannot answer,
  including an unreadable record or an unanswerable capability probe.

- **`/msg/list` no longer hands every agent the fleet's notification feed.**
  `resolveListProject` returned the instance project for every caller and the
  underlying `scion notifications` call is scoped by the hub to the authenticated
  **user** — always the host controller PAT — so the project never scoped
  anything. Any worker read the manager's and every sibling's events, and the
  manager's documented `--worker <name>` selector was inert. lever now resolves
  whose inbox the caller may read to an agent *slug*, resolves that to the hub's
  agent id, and returns only the events the hub attributes to it. An event with
  no `agentId` is dropped, and an id lever cannot resolve fails the request:
  returning the raw feed is the leak this closes.

- **Bootstrap staging followed agent-planted symlinks.** `wire.Stage` wrote
  `bootstrap.json` with `MkdirAll`/`WriteFile`/`Chmod` — all of which resolve
  symlinks — into a directory inside the agent's own read-write mount, as the
  host operator. An agent that replaced its `.lever` with a symlink chose where
  the host wrote, and chmod'd 0600: the instance root's `lever.yaml`, the
  broker's `revocation.json` (which decodes as a zero `RevocationState`, undoing
  `lever revoke`), the controller PAT. A lapsed leaf alone reaches it through the
  auto-re-enrol healer, with no operator action at all. Staging now takes the
  tree as a confinement anchor, runs through `os.Root`, and refuses a symlink at
  the staging directory or at `bootstrap.json` even when it stays inside the
  tree.

- **A message body could not smuggle flags into `scion message`.** The body was
  passed as a bare positional and cobra parses flags interspersed with
  positionals, so a body of exactly `-b` bound to `--broadcast` and delivered to
  every agent in the project — past the worker-to-worker deny, which had checked
  only the recipient. `-a` widened it to every project on the hub. Flags now
  precede a `--` terminator.

- **The `--role` capability probe no longer memoises its answer.** It cached the
  verdict for the client's lifetime, justified by `ConfigHash` bouncing the
  broker on a scion change. That does not hold for `scion.source` and
  `scion.binary`: both are *paths*, so replacing the artifact behind one leaves
  the hash byte-identical and a long-lived broker keeps its verdict. A stale "no
  `--role`" disarmed the stamp and the new pre-role guard together, silently.

- **Overlapping worker `dir`s are rejected at config load.** Validation checked
  each worker's dir in isolation, so two workers could declare the same subtree,
  or one could declare an ancestor of another, and both passed. That voids
  sibling isolation for the pair: the outer worker reads and writes the inner
  one's whole workspace, including the fresh, unspent enrolment ticket the
  broker stages there on every resume — redeem it first and it enrols as the
  inner worker's CN, taking its capability grants. Comparison is component-wise
  on cleaned paths, so `workers/x` and `workers/xy` stay legal.

- **A worker may no longer be named `manager`.** The operator-directive channel
  resolves that literal name to the manager whatever `broker.manager_identity`
  is set to, so a worker holding it was silently shadowed and a directive aimed
  at the worker was delivered to the most privileged agent. The existing
  collision checks did not cover it, because a custom `manager_identity` moves
  the manager's CN off that word.

- **`manager.credential_file` must now be `0600`-tight.** It rejected only
  world-readable modes, so a `0640` OAuth token — the longest-lived,
  highest-value secret lever handles in subscription mode — was accepted, read,
  and projected into every agent container, while `lever doctor` was already
  failing the same file. It now rejects any group or other bit, matching
  `api_key_file`, the controller PAT and the staged bootstrap.

### Added
- **`lever doctor` reports agent records that store no role.** It reports them
  on *any* pin, so the bump above can be planned rather than discovered: on a
  Scion that predates roles the line passes with an explicit warning that a
  later pin will refuse these records, and on a roles-aware Scion it fails,
  naming each record. An unreachable hub stays "not checked"; a hub that
  answered unusably (403, unknown project) is a finding, matching the
  shared-directories check.

### Fixed
- **Docs: `scion.agent_role` was missing from the configuration reference.** The
  one key that overrides the role lever stamps was documented only in the
  security model.

- **Docs: two security claims the code did not make good on.** The
  worker-isolation guide said the controller PAT was "re-minted, not blindly
  reused" and that lever re-runs the mint window when the hub rejects it — no
  such path exists. `lever apply` short-circuits on any PAT already on disk,
  never validates it, and only `lever destroy` clears it; a rejected PAT is a
  hard bring-up failure the operator resolves. The credentials guide claimed the
  admin listener's loopback bind was "enforced twice" and cited
  `resolveAdminAddr`, a function deleted in this range; the live enforcement is
  a single fail-closed check in `Broker.ServeListeners`, which now reads as
  such so nobody adding a second bind path assumes a helper still guards it.

### Changed
- **Docs: upstream closed the hub-wide scratchpad gap.** scion#1103 answered
  lever's scion#1098 with a `project_defaults.default_scratchpad` key that works
  in file/SQLite mode, so the worker-isolation guide no longer claims the
  section has no `settings.yaml` key. The key acts at project creation time
  only, so lever's per-project removal stays. Two traps found while verifying it
  are recorded there: Scion reads the top-level `project_defaults` section only
  when the same `settings.yaml` also carries a `server:` section, and the commit
  carrying the key is not fetchable through the Go module proxy.

## [0.14.0] - 2026-08-10

### Security
- **Agents are pinned to the `baseline` role automatically (scion#1089/#1090).**
  Upstream replaced per-template scope lists with named role bundles, and then
  **flipped the default for an unspecified role from `baseline` to `full`**
  (scion `2181aff6`, #1090). On any pin at or after that commit, an agent
  started without an explicit role receives agent create, agent lifecycle and
  project-secret-read — breaking lever's core invariant that no agent holds hub
  authority.

  lever now decides by asking the scion **binary** whether it accepts
  `start --role`, not by reasoning about the pin — a commit hash says nothing
  about which side of #1090 it sits on. When roles exist, lever stamps
  `baseline`; when they do not (pre-#1089), it omits the flag, which widens
  nothing because the roles system is absent there. A probe that cannot answer
  fails the start rather than guessing, since guessing wrong grants full
  authority. The probe runs once per client, not once per dispatch.

  Baseline means heartbeat and self-token-refresh, with no agent create,
  lifecycle or secret scope — worker dispatch runs host-side under the
  controller PAT, never an agent's own token. Note it is wider than lever's
  previously documented three scopes: it also grants `project:read` and
  `agent:port:forward`.

### Added
- **`scion.binary` accepts an already-built scion binary (#27).** `scion.source`
  and `scion.version` both *compile* scion on the machine hosting the jail, on
  every bring-up, so that host needs a Go toolchain, a ~1.3 GB module cache and
  egress for module fetches. `scion.binary: ./dist/scion-linux-amd64` installs a
  binary built elsewhere and needs none of them. It is mutually exclusive with
  the other two, and is checked against the guest's architecture — via the ELF
  header, before anything is written into the jail — so a workstation arch
  mix-up fails with "binary is arm64, but the guest is amd64" rather than as
  `exec format error` at manager start. `scion.binary` and `scion.source` are
  rejected when they resolve **inside the mounted tree**: whatever they name is
  installed as root as the engine every agent runs under, and the tree is the
  one place agents can write.
- **`scion.agent_role` overrides the role lever picks.** Rarely needed — the
  default above is the safe one. Naming a role on a scion with no `--role` flag
  is a hard error rather than a silent downgrade, and an unknown value is
  rejected at config load rather than as an opaque bring-up failure.

### Changed
- **Scion `91c26b34` (163 commits) verified on pure upstream, but NOT shipped**
  — the pin is unfetchable, see above. Every patch lever previously carried on
  its scion fork is upstream by content at that commit, so the fork stops being
  necessary as soon as the module can be fetched again. When it can, the
  cutover needs a full `lever stop` + `lever up`, never a hub-binary swap: the
  hub applies one-way DB migrations on first boot, and agent tokens minted at
  the old pin 403 on newly scope-gated read endpoints until they refresh.
  Snapshot the hub DB first. The examples stay pinned at `68507153`.
- **scion is no longer re-streamed into the jail when it has not changed.** The
  binary is 158 MB and was piped in on every `lever up`, in all modes. lever now
  hashes the installed binary in the guest and skips the copy only when it
  matches — the file itself, not a record of what was installed once, so a
  binary that was deleted, truncated or replaced is reinstalled and `lever up`
  stays self-healing. Every failure mode (no such file, no `sha256sum`,
  unreadable) falls through to installing.
- **The broker restarts when the `scion` config block changes.** It holds a
  long-lived client that caches what the installed scion supports, and the
  reuse check previously ignored `scion:` entirely — so that cache could outlive
  the binary it described across a pin bump.

### Fixed
- **New projects no longer carry scion's cross-agent `scratchpad` shared dir
  (scion#925).** Upstream stamps a writable `scratchpad` on every new project
  and mounts it read-write into EVERY agent of that project — a channel between
  the manager and every worker, which defeats lever's subtree isolation. On
  lever's file/SQLite hub the server-side default cannot be turned off, so
  `lever apply` now strips the directory from the hub's project record after
  registration, on both the fresh and the already-registered path, then
  re-reads the record to confirm it is gone — the hub answers `404` for "no
  such shared dir", "no such project" and "no such route" alike, so the delete
  status alone cannot prove the removal happened. A failure fails apply rather
  than starting a fleet that silently shares a directory. The request runs
  inside the jail, like every other scion call: the hub binds the jail's
  loopback, and the Lima backend suppresses guest→host port forwarding by
  design. `lever doctor` gained a matching check. The controller PAT now also
  carries `project:update`, which the shared-dirs endpoint requires. The strip
  refuses to guess when a hub lists more than one project of the same name, and
  requires the `sharedDirs` field in the response — an absent field would
  otherwise decode as an empty list and pass the verify vacuously, which is the
  exact failure the verify exists to catch.
- **`go test ./...` no longer fork-bombs the machine.** Two broker tests
  re-exec'd `os.Args[0]` — the *test binary* under `go test` — as a detached
  (`Setsid`) `broker serve`, which outlived the run, accumulated across runs and
  re-spawned. A full suite left 724 stray processes behind.

## [0.13.0] - 2026-08-04

### Fixed
- **Directive tools no longer silently vanish from Claude Code sessions
  (#24).** `directive_consume` / `directive_check` advertised an input schema
  with a top-level `anyOf` combinator (added in the 0.9.1 both-spellings fix).
  The Anthropic API rejects a tool input schema carrying a top-level
  combinator, and Claude Code then drops the tool from the session with no
  error — so validly-signed, delivered operator directives could not be
  consumed, and the model's `request`/CLI fallback surfaced a misleading
  `policy: may not obtain/delegate` denial. The schema is now a plain object
  declaring both `id` and `directive_id`; the exactly-one-spelling rule was
  already enforced caller-side by `directiveID()` (JSON-RPC -32602 before any
  broker call), so nothing is lost. NOTE: the bug lives in the baked
  `lever-agent` binary — the fix reaches a running instance only via an agent
  **image rebuild + restart**, not a host lever upgrade.
- **Agent renewal no longer re-points Claude at the mTLS broker for LLM
  traffic.** The 12-hourly leaf renewal (`RefreshLLMToken`) wrote
  `ANTHROPIC_BASE_URL` from the broker URL while boot writes the loopback
  gateway URL — a drift introduced when the `aa63f9f` gateway leaf-hot-reload
  fix moved only boot. In api-key mode this re-introduced the 24h cached-leaf
  LLM outage that fix closed, at the first renewal after boot. Renew now pins
  the same loopback gateway URL as boot.

### Changed
- **Internal refactoring pass (no behavior change).** Executed the 2026-08-01
  whole-codebase refactoring plan: deduplicated the broker security-path
  handler preambles, unified the bootstrap wire envelope and agent HTTP
  helpers into single sources, split the oversized orchestration functions
  (`handleWorkerStart`, `start-manager`, `buildApplyDeps`, `brokerctl.Serve`),
  extracted a shared backend base for orbstack/lima, introduced typed
  constants for the agent-phase / step-kind / directive-kind / config-enum
  string vocabularies, and deleted vestigial exports. Test count rose
  (838 → 905); the full suite and `go vet` stay green, race-clean on the
  concurrency-sensitive packages. One deliberate, documented micro-change rode
  along: the `handleWorkerList` certless-caller audit line now carries the
  underlying error like its siblings.

## [0.12.0] - 2026-07-31

### Added
- **Broker-side auto-re-enrol after natural leaf lapse (#22).** An agent whose
  24h mTLS client leaf aged out while renewal couldn't run (host asleep,
  instance idle) previously stayed dead until a manual `lever up`. The
  broker's mTLS handshake verification now classifies that exact failure
  cryptographically — the presented cert is our CA's signature over a
  configured CN, wrong **only** in time — and a policy-gated healer re-stages
  a fresh one-use ticket and bounces the agent (suspend+resume; `resume
  --force` from the error phase), so boot re-enrols and the conversation is
  preserved. Gated by `broker.auto_reenrol: all | manager | off` (default
  `all`); revoked identities are NEVER healed (`lever revoke` stays a real
  kill-switch); attempts are audited (`op=reenrol`) and bounded (10 min
  per-CN cooldown, 3 per burst, burst resets on success or an hour of quiet).
  A leaf that is not-yet-valid (backward clock step) is rejected but never
  classified as a lapse — no automated bounce on clock skew.
- **Opt-in declared operation params reject typo'd mint constraints (#21).**
  An op may declare `params:` (its argument names) alongside `caveat_param`;
  when declared, `/request` rejects capability constraints on any other key
  AT MINT TIME with an accurate error — previously a mistyped key minted a
  strictly over-narrowed token that failed closed only at call time
  (`constraint not satisfied`), far from the mistake. Undeclared ops keep
  the permissive behavior; params come from config only (a self-registering
  tool cannot alter them).
- **doctor/init surface stale adopted-skill baselines (#16).** An adopted
  skill's `lever-version:` frontmatter stamp is treated as the owner's
  attestation of the framework baseline it was reviewed against: doctor now
  FAILS (never auto-overwrites) when it lags the current version (or is
  absent), naming both versions; `lever init` marks the line with `!`.
  Previously an adoption pinned an instance to its adoption-era baseline —
  through security-relevant skill changes — with doctor green throughout.

### Fixed
- **apply no longer discards an error-phase manager without trying to save
  it (#3).** `start-manager` now re-stages a ticket and attempts `scion
  resume --force` (scion#895) on a `phase=error` record BEFORE the loud
  delete+fresh fallback — the 2026-07-31 conversation loss (VM reboot →
  corrupted container state → resume failure → deletion) would have been a
  silent recovery. The loud fallback and its wording are unchanged when the
  forced resume also fails — but a failed resume now RE-OBSERVES the record
  first (with the broker-unavailable retry), so apply cannot delete a session
  the broker's healer concurrently recovered.
- **apply restarts the broker when its binary or tool config is stale
  (#19).** The M2 broker-reuse shortcut kept ANY serving broker, so a
  re-apply with a changed tool set — or a lever upgrade — left the old
  broker (old tools, old routes, no healer) silently running while apply
  reported success. `/epoch` now reports the broker's binary version + a
  digest of the broker-relevant config; apply restarts on mismatch (old
  brokers report neither — always restarted).
- **`up --fresh` discards a record in ANY phase.** Now that apply preserves
  an error-phase record whose forced resume comes up dead (see #3 above),
  `--fresh` is the escape hatch for a genuinely-bricked record — but it only
  triggered for running/suspended, so `up --fresh` resumed the very record
  the user asked to discard (caught in 0.12.0 live validation).
- **Worker resume re-stages a fresh enrolment ticket** (mirroring the
  manager's 0.10-era fix) and uses `resume --force` for error-phase records:
  a worker resumed after its ticket/leaf lifetime no longer wedges into
  `phase=error` on a spent ticket (live-hit 2026-07-31), and an
  already-wedged record heals on the next dispatch instead of requiring
  `lever worker purge`.

Requires `scion.version` >= `68507153` (already the 0.11.0 pin) for
`resume --force`.

## [0.11.0] - 2026-07-31

### Changed
- **Scion pin bumped `b4c9911d` → `68507153`** (82 upstream commits,
  2026-07-22 → 2026-07-31; examples and e2e-script defaults updated). A full
  review of the range found every lever-critical surface intact: relative
  `--workspace` subtree isolation (#815), `pre-start.d` script hooks and their
  ordering, `--dev-auth=false` + UAT token flow, `--map-host-loopback`
  per-agent netns, default agent-token scopes, and the `list --format json`
  fields lever parses. Instance-visible upstream deltas to know about:
  - **scion#894**: the hub now applies a default `--cpus 2` limit to agent
    containers with no explicit resource spec (`runtime.enforce_resource_defaults`,
    default on). Set it to `false` in the jail hub settings, or give agents an
    explicit resource spec, to keep previous unlimited behavior.
  - **scion#908**: agents with no model configured now get `ANTHROPIC_MODEL`
    resolved to `opus` — set `SCION_MODEL`/a model in the harness config if you
    want a different default.
  - **scion#847**: scion's *claude-harness base image* now ships a tool deny
    list (including `SendMessage`) in its baked `settings.json`. This arrives
    only when you rebuild the `scion-claude` base image, not with this pin
    bump — when you do, verify the deny list doesn't remove tools your agents'
    workflows need.
  - **scion#895**: upstream adds `scion resume --force` to recover agents
    stuck in the error phase — not yet used by lever's recovery paths
    (candidate follow-up).

## [0.10.1] - 2026-07-22

### Fixed
- The 0.10.0 release shipped with the `lever version` constant still reading
  `0.9.2` (binaries self-report `0.9.2 (v0.10.0)` — the commit suffix is
  correct, the semver is not). The release workflow now refuses to publish a
  tag whose version does not match `internal/cli/root.go`'s `Version` constant.

## [0.10.0] - 2026-07-22

### Changed
- **The Scion fork dependency for worker subtree isolation is gone.** Scion
  merged relative `--workspace` paths resolved against the project root with
  the same containment guard
  ([scion#815](https://github.com/GoogleCloudPlatform/scion/pull/815), merge
  commit `b4c9911d`), superseding the fork-only `--workspace-subdir` flag
  (fork branch `feat/per-agent-workspace-subpath`, upstream PR #699 — closed).
  Worker dispatch now emits `scion start --workspace workers/<name>` (relative
  form). Requires `scion.version` >= `b4c9911d`; the shipped example pins are
  bumped accordingly. On an older Scion, worker dispatch regresses to the old
  whole-tree-mount enrolment failure (fails closed: the worker 403s at
  enrolment) — bump the pin, or keep building from the fork until you do.
- The e2e test scripts' default `SCION_VERSION` bumped `666333f9` → `b4c9911d`
  ([#11](https://github.com/stevegeek/lever/issues/11)).

## [0.9.2] - 2026-07-22

### Fixed
- `delegate` silently minted a **self-bound** token when no recipient was named,
  and reported success. Both mint surfaces were affected: the capability MCP
  tool (`delegate` with `to` absent, blank, or given under the sibling spelling
  `bound_to`) and the in-jail CLI (`lever-agent delegate` with no `-to`). Each
  passed an empty bind target to the broker, which defaults an empty target to
  the caller (self-obtain), while on the MCP path an unrecognised key survived
  as a bogus narrowing constraint — so an agent could believe it had handed a
  capability to another agent when it had only minted one for itself, with
  nothing saying otherwise. Both surfaces now validate `tool`, `op` and the
  recipient from the caller's own arguments before any broker call (JSON-RPC
  `-32602` on the MCP path), and both reject naming *yourself* as the
  recipient — which hands nothing off and, because `MayObtainRule` treats
  requester == recipient as a self-obtain, succeeded even for an agent holding
  no delegate grant. `request`'s `bound_to` stays optional and still defaults to
  self. Not a privilege issue — `MayObtain` remains the authoritative gate — but
  the same class of silent argument bug as 0.9.1's directive fix. (#20)

  Compatibility note: the MCP tools now reserve **both** bind-target spellings,
  so a constraint key literally named `to` or `bound_to` is rejected with
  `-32602` instead of reaching the token. No shipped tool config uses either
  name. It matters because constraint keys are tool argument names, and `to` is
  a plausible one (mail, message, transfer) — but the two readings of `to` on
  `request` are indistinguishable from the caller's arguments alone, and a loud
  refusal is recoverable where a confidently wrong mint is not. A tool that
  needs such a constraint can rename it in `lever.yaml` via
  `caveat_param: {recipient: to}`. The CLI is unaffected: its flags and its
  positional `key=value` constraints occupy separate namespaces.

  Known limitation: on the MCP path only the sibling spelling is recognised as a
  mis-named recipient. Any other near-miss (`agent`, `To`, `recipient`) is still
  accepted as a narrowing constraint — the general case is indistinguishable
  from a legitimate constraint key. Such a token fails closed at call time
  rather than granting anything extra.

## [0.9.1] - 2026-07-22

### Fixed
- An agent could fail to consume a **valid** operator directive and conclude
  none existed. The `directive_consume`/`directive_check` MCP tools declared
  their argument as `id`, but every other spelling an agent sees is
  `directive_id` (the signed statement's field, `lever directive send`'s
  output, the docs), so a model that sent `directive_id` had its argument
  dropped, posted an empty id, and got the broker's deliberately opaque 404 —
  byte-identical to "unknown id / wrong target / already consumed / expired".
  Both spellings are now accepted and both are declared in the advertised
  schema. A missing, blank, over-long, non-string, or self-contradicting id
  now fails locally with JSON-RPC `-32602` before any broker call, so a
  caller-side mistake can never again masquerade as a directive verdict. The
  opaque-404 contract is untouched for everything genuinely about directive
  state — the new error is decided without any lookup, so it is not an oracle.
  The pointer notification now spells out the call form (`directive_consume`
  with `id="<the id>"`) instead of naming only the tool. **Requires an
  agent-image rebuild** (`make lever-image`, `LEVER_IMAGE_FORCE=1`) plus a
  container recreate (`lever stop && lever up`): `lever-agent` is baked into
  the image, so upgrading the host CLI alone does not fix a running instance.

## [0.9.0] - 2026-07-22

### Added
- Configurable Lima guest disk size (`disk:`, default 24GiB) — the guest disk
  was previously a hardcoded 100GiB sparse file, which could wedge a
  constrained host even though the sparse file itself grows lazily (#14).
- `lever doctor` reports the agent image's baked Claude Code version, read
  from a `claude_code_version` image label baked in at build time — no
  container run needed. A pre-label image reports informationally rather
  than failing the check (#6).
- Per-tool supervisor logs: each supervised tool (e.g. `lever-tool-db`) now
  writes its own `.lever-state/tool-logs/<tool>.log` instead of sharing one
  file, plus new `logrotate` guidance in the operations guide since none of
  `.lever-state/`'s logs rotate on their own (#10).
- `lever worker purge NAME`: host-side deletion of a worker's scion record
  and staged bootstrap ticket so it can be redispatched fresh with a new
  task. Never touches the worker's `HostWorkspace` (its work product);
  requires `--force` (#7).
- Prebuilt release binaries via GoReleaser, wired into a GitHub Actions
  release workflow; curated release notes are preserved rather than
  overwritten (#1).

### Fixed
- A supervised tool command that doesn't resolve on the supervisor's PATH is
  now rejected loudly at config-load instead of failing opaquely at spawn
  (or silently on a later re-apply); the same check also rejects an
  absolute command path that is a directory or non-executable. `lever
  doctor` now probes every configured tool backend — both external
  (dialed) and supervised (command-resolved), previously untested for the
  supervised case (#9).
- Broker denials on `/msg/send`, `/msg/list`, and `/request` now return the
  specific policy-deny reason in the HTTP body instead of a bare
  `forbidden`, and the CLI's broker client surfaces it in the returned
  error — so a denied agent can see *why* instead of guessing. Scion-runtime
  errors (as opposed to policy denies) are deliberately left opaque (#4a).
- Worker start/resume now polls the worker's own scion record for a
  `running` phase and a live container before reporting success, instead of
  trusting scion's start/resume call, which could report success for a
  harness that then dies moments later (#7).
- `lever stop`'s scion-suspend failure is no longer silently swallowed — it's
  now printed as a warning, so a recurrence of the Lima conversation-loss gap
  (#3) is diagnosable instead of invisible.

### Changed
- Dispatching an **existing** worker with a **new** task now fails with
  HTTP 409 instead of silently resuming the worker's original,
  creation-time task (scion pins a worker's task at creation and its
  `Resume` takes no task argument, so a new task was previously discarded
  without warning). To resume an existing worker, use `lever-manager agent
  resume NAME` (the `/worker/resume` path) — `agent start` cannot resume, as
  its `--task` defaults to a non-empty prompt so it always carries a task. To
  run a *new* task, run `lever worker purge NAME` first to start it fresh (#7).
  The `lever-operator` skill now teaches this vocabulary (resume an existing
  worker, `msg send` to give a running worker new work, `worker purge` is
  operator-only), so re-scaffold instances with `lever init` after upgrading.

### Docs
- Honest security-model note on the in-jail scion Hub API residual: the
  2026-07-06 create/lifecycle vector is closed *structurally* (the real hub
  always starts `--dev-auth=false`, hardcoded, no config knob enables it —
  so there's no runtime toggle for a `lever doctor` check to guard), but
  agent `DELETE` is not yet scope-checked by scion and remains callable by
  an agent's own token — tracked as a scion-side fix, not something lever's
  controller-PAT model can close from outside. Also corrects an egress
  overclaim: `egress: closed` still admits loopback for the in-machine hub,
  so this residual is identical under `open` or `closed` (#8).

## [0.8.1] - 2026-07-21

### Fixed
- Operator directives now reach agents on instances **upgraded** to 0.8.x, not
  only freshly-created ones. The directive generation that `target_agent` binds
  to was established only in the `/enrol` handler, but an agent that restarts with
  a persisted cert (or whose cert predates 0.8.0) refreshes via `/renew` and never
  re-hits `/enrol` — so its generation stayed `0` and `lever directive send`
  failed at resolve with `agent not yet enrolled`. `/renew` now establishes the
  generation at 1 when unset, without bumping an existing one (bumping stays
  reserved for genuine re-enrolment, so a 12 h cert refresh never invalidates an
  agent's own active directives).

## [0.8.0] - 2026-07-21

### Added
- **Operator directives** — an authenticated human-operator→agent channel. Until
  now every human→agent instruction (`lever msg`, relayed email/file text) arrived
  as an unauthenticated `sender:"user:…"` string, so a well-hardened agent
  correctly refused out-of-band instructions it could not attribute to the
  operator. Directives give the operator a channel an agent can treat as
  authoritative **without** creating a prompt-injection backdoor:
  - The operator signs a structured directive with an SSH key
    (`lever directive send <agent> --instruction … | --action …`); the host-side
    broker verifies it (`ssh-keygen -Y verify` against an `allowed_signers` file,
    exact-byte verify-then-parse, duplicate-key rejection) and delivers only a
    **pointer** (a directive id) into the agent's inbox — never the content.
  - The agent, if it independently decides to act, fetches the directive with a
    `directive_consume` MCP tool over its own mTLS channel. All cryptography is
    host-side; the model never checks a signature. A directive binds to the
    caller's mTLS identity **and** enrolment generation (`{cn, generation}`), so a
    recycled worker slug can never inherit a predecessor's directive and one agent
    cannot consume another's — consume is a single-use atomic compare-and-swap and
    every miss returns an identical opaque `not found`.
  - New host CLI: `lever directive send/list/revoke/selftest`, over a **0600
    unix-domain-socket** admin channel (unreachable from the VM); every admin op is
    operator-signed. New config block `operator:` (`allowed_signers`, `signing_key`,
    `directive_expiry` default 10 min, capped at 24 h). Directive state (active set,
    replay tombstones, per-CN generations) persists across broker restarts
    (`directives.json`, atomic writes, fail-closed). A `lever doctor` check and a
    `selftest` round-trip catch `allowed_signers` misconfiguration before it is
    needed. Depends on the per-agent netns isolation shipped in 0.7.0.
  - **Scope (honest):** Phase 1 delivers authenticated, integrity-protected,
    replay-proof **delivery and verification** of an operator-signed action bound to
    the target agent. It does **not** yet enforce the bound action at tool-call time:
    on consume the broker returns the validated action and the agent triggers the
    call through its ordinary, independently grant-checked capability path, so a
    directive grants **no new capability** — its value is that the request is
    provably from the operator. Host-enforced call-time binding (and the `approval`
    kind's operator-approval gate) is deferred to a later phase with its own review.
  - Bootstrap prompts (manager + worker) gain a directive carve-out: only an action
    returned by the agent's own `directive_consume` this turn carries operator
    authority; messages claiming or quoting a "verified" directive are data.
    Manager→worker authority does not launder (directives are signed for the worker
    directly). Existing instances show a scaffold-drift warning until re-scaffolded.

## [0.7.0] - 2026-07-21

### Changed
- Agents now run in their **own per-agent network namespace** (rootless podman's
  default pasta networking) instead of a shared jail netns (`--network=host`).
  Each container's `127.0.0.1` is now private, which closes a cross-agent
  escalation: the in-container agent gateway proxy (`127.0.0.1:8462`, holds the
  agent's mTLS leaf, trusts whoever reaches its loopback) was jail-wide under
  host networking, so a co-resident worker could `POST` the manager's gateway
  and be authenticated to the broker **as the manager** with no credential.
  Hub reachability across the netns boundary is preserved by a pasta
  `--map-host-loopback` option staged in the guest `containers.conf.d`
  (Scion's auto-computed container hub endpoint, `host.containers.internal:PORT`
  for podman, resolves to it); egress containment is unaffected because pasta's
  userspace egress still re-emerges on the jail `OUTPUT` chain (`LEVER_EGRESS`),
  not a bypassing `FORWARD` path. No Scion change required. Escape hatch:
  `LEVER_FORCE_HOST_NETWORK=1` on the host restores `--network=host` for
  debugging (reopens the shared-loopback gap; not isolation-safe). **Cutover
  note:** switching a running instance requires `lever stop` + `lever up` — the
  long-running Scion server process caches the old force-host env, so recreating
  only the agent is not enough. See the new worker-isolation §4.3.

## [0.6.0] - 2026-07-17

### Changed
- Agent images are now tagged **by architecture** (`scionlocal/lever-claude:arm64`
  / `:amd64`) instead of a shared `:latest`, and a **tagless** `manager.image` (or
  worker `image:`) auto-resolves to the jail's arch at apply time. A host that
  cross-builds both arches — an arm64 laptop producing an amd64 server image — no
  longer clobbers one arch's image with the other's under `:latest`, the failure
  mode where the jail loads a wrong-arch image that dies at boot with `exec format
  error`. `make lever-image LEVER_IMAGE_ARCH=<arch>` builds `FROM scion-claude:<arch>`
  and tags the output `:<arch>`; an explicitly-tagged or digest-pinned image ref is
  left untouched (the escape hatch). Instances that pinned `…:latest` should drop
  the tag to opt into arch-resolution.

### Fixed
- The capability-minting sidecar (`lever-agent serve-capability`) now re-reads
  the rotating agent leaf per broker handshake instead of freezing the boot
  cert. It built its mTLS client once via the static `Identity.Client()`, so
  after the leaf's 24h TTL every capability mint failed the broker handshake
  (`certificate has expired`) — taking down every brokered tool (each mints a
  capability first) while the broker itself stayed healthy, recurring roughly
  daily. The 2026-07-13 gateway fix covered Claude's proxied MCP/LLM traffic but
  not this second, direct-to-broker client. A new `agent.NewReloadingClient`
  (reusing the gateway's per-handshake `clientCertSource`) closes it, and
  `Identity.Client()` is now documented short-lived-only so no future long-lived
  holder reintroduces the trap.

## [0.5.0] - 2026-07-16

### Added
- `lever version` now appends build provenance to the release string when the
  binary carries it: the commit it was built from (short) plus `-dirty` for an
  uncommitted tree (any `go build` / `make install` from a git checkout), or the
  module version for a `go install …@vX` build. A make-install binary that lags
  its source no longer hides behind the bare hardcoded version string.
- `lever init --adopt`: record owner-customized scaffolds (the operator/agent
  SKILL.md files and the whole tree-root CLAUDE.md) as an accepted baseline in
  `.lever-state/skills-adopted.json`. Doctor's "operator skills" check and
  `lever init --check` then treat the adopted content as OK, and a plain
  `lever init` leaves it alone (including not appending the CLAUDE.md block).
  Previously a customized scaffold read `skipped-modified` forever. Adoption
  is deliberately a recorded baseline rather than a mute: the scaffolds live
  inside the agent-writable tree, so the check doubles as tamper detection —
  any change PAST the adopted baseline fails doctor as "modified since
  adoption", and the baseline itself lives host-side where an agent cannot
  re-bless its own edits. Only genuine customizations qualify: framework-
  current files and stale-but-unmodified scaffolds (plain-`init` refresh
  territory) never get a record. `lever init --force` still restores the
  framework content — for CLAUDE.md, by re-ensuring the marker block in place
  — and clears the now-stale adoption record.

### Fixed
- `lever up` no longer needs a second run to clear the first-boot
  `start-manager` race on a cold VM. The scion workstation daemon registers its
  runtime broker asynchronously after its Hub API comes up, so the first
  create/resume could act before the broker was ready — failing the apply (on a
  cold VM as a hub "context deadline exceeded") so only a second `up`
  reconciled. `start-manager` now waits for the runtime broker to be registered
  and online (via `scion hub brokers`) before acting, closing the window at the
  source; the wait is fail-soft, so it can never fail the bring-up on its own.
  As a backstop, the transient-broker retry also now treats a hub "deadline
  exceeded" as the same race, and the initial observe `scion list` rides that
  bounded, ctx-checked retry instead of failing on the first blip.
- `lever destroy` now clears the persisted controller PAT
  (`.lever-state/controller.pat`). The PAT is minted against the hub DB that
  lives inside the jail, so destroying the machine leaves it stale; the next
  `lever up` reused it (ensureControllerPAT no-ops when a PAT is already
  persisted) and the new hub's fresh DB rejected it, failing the readiness
  probe with "authentication failed" until the file was removed by hand. Only
  the current-instance teardown (no `--machine`) clears it, alongside the
  broker stop and staged-ticket cleanup it already does.
- `lever apply` no longer re-imports every agent image into the jail on each
  run. The `load-image` step now first compares the jail's podman image ID
  against the host docker image ID and skips the multi-GB `docker save |
  podman load` re-stream when they match (the config digest is stable across
  save/load, so equal IDs mean the exact bytes are already present). The check
  is fail-open — any uncertainty (a not-yet-loaded or rebuilt image, an inspect
  failure) falls through to a load, so it can never wrongly skip and leave a
  stale image. This matters most under the first-boot retry loop, where any
  step failing re-runs the *entire* plan: previously each retry re-streamed
  every image; now unchanged images are near-no-ops. After a load, the step also
  prunes dangling (untagged, unreferenced) jail images — so a rebuilt image,
  whose old copy the load leaves untagged, no longer ratchets the grow-only jail
  disk up by a full image size (a no-op when nothing was superseded). Pruning
  never touches a tagged or container-referenced image.
- A tool whose broker backend carries a path (e.g. qmd's `[::1]:3101/mcp`) now
  reaches that path exactly on the tool root, instead of a trailing-slash
  variant (`/mcp/`) that a strict streamable-HTTP endpoint 404s. The trailing
  slash was an artifact of the broker's subtree mux; qmd was the only tool with
  a path-suffixed backend and so the only one that couldn't connect. Path-less
  backends (every other tool) and sub-path requests are unaffected.
- `lever doctor`'s "agent certificate" check no longer cries wolf right after
  a healing restart: an expired-leaf rejection logged before the current
  broker started (pid-file mtime) is reported as healed rather than as an
  active failure. Previously any rejection inside the 15-minute window failed
  the check even when the restart that fixed it had already happened.
- The broker's mTLS serving cert now self-rotates. It was minted once at
  startup with the 24h leaf TTL, so a broker running longer than a day served
  an expired cert and every gateway handshake failed — tools down, and the
  agents' own `/renew` calls failed with it, so their leafs decayed too (the
  only remedy was a `lever stop && lever up` power-cycle). The broker holds the
  CA key, so it re-mints its serving cert in-process via a rotating
  `GetCertificate` source when less than 6h of validity remains; agent leafs
  were already kept fresh by the 12h renew sidecar once the broker stays
  reachable.
- The agent's mTLS client leaf now hot-reloads on the live MCP/LLM path. Claude
  Code read `CLAUDE_CODE_CLIENT_CERT`/`_KEY` once at process start and cached the
  leaf for its whole lifetime, so after ~24h the boot cert expired and every
  gateway call failed with `tls: certificate has expired` — despite the renew
  sidecar rewriting a fresh leaf every 12h — until the manager was restarted. A
  new `lever-agent gateway` sidecar now runs a loopback (127.0.0.1) reverse-proxy
  that terminates plaintext from Claude and re-presents the always-current leaf
  to the broker over mTLS, re-reading `agent.{crt,key}` per TLS handshake. Claude
  no longer holds the rotating cert: boot points its MCP `--transport http` URLs
  and `ANTHROPIC_BASE_URL` at the loopback gateway. The proxy caps idle broker
  connections at 5m so a rotated leaf reaches the broker well before the 24h
  expiry, uses `FlushInterval = -1` for MCP's SSE streaming, and binds loopback
  only (it holds the agent key). Boot-time discovery and llm-token calls still go
  direct to the broker (the gateway isn't up during pre-start).

## [0.4.0] - 2026-07-10

The single-project re-architecture (P1–P4): one Scion project per instance,
the manager and all workers as agents in it, the real hub running dev-auth-off
behind a host-side controller PAT. **Worker subtree isolation depends on
Scion's `--workspace-subdir` feature, which is not yet upstreamed** (fork branch
`feat/per-agent-workspace-subpath`); dispatching workers requires building Scion
from that fork (`scion.source`) until it lands — see the Fixed entry below.

### Added
- Single-project model: the manager and all workers now share one Scion
  project (the jail mount root), with workers living as in-place subdir
  workspaces (`workers/<name>`) instead of separate per-agent projects.
  Collapses `register-manager` + N×`register-worker` into one
  `register-project` apply step and the worker list into a single
  instance-project query. (P2 of the single-project re-architecture.)
- Controller-PAT bootstrap: `lever apply` mints a scoped controller PAT
  (`agent:manage`, `agent:attach`, `project:read`) via a throwaway dev-auth
  server, persists it `0600` under `.lever-state/`, and threads it into every
  scion client (including attach). The real hub now runs with
  `--dev-auth=false` by default. (P3 of the single-project re-architecture.)

### Changed
- Renamed the `grove` concept to `worker` throughout: config keys `groves:`→`workers:` and `grove_to_grove:`→`worker_to_worker:`, the `--grove`/`-grove` CLI flags → `--worker`/`-worker`, broker routes `/grove/*`→`/worker/*`, and the `groves/<name>` workspace convention → `workers/<name>`. Prerelease clean break — no migration. (P1 of the single-project re-architecture.)
- The agent image (`image/lever-claude`) pins Claude Code explicitly (`ARG
  CLAUDE_CODE_VERSION`) instead of inheriting whatever the scion base image
  baked. Bump the ARG + rebuild + `lever apply` to upgrade; the in-container
  auto-updater remains disabled (updates by rebuild, never at runtime).

### Fixed
- Workers now mount **only their own subtree** at `/workspace`. `lever`
  dispatches each worker with `scion start --workspace-subdir workers/<name>`
  (project-relative, containment-guarded) instead of an absolute `--workspace`,
  so a worker can no longer see the manager's tree — and the dispatch-time
  enrolment failure where a worker read the manager's bootstrap and inherited a
  spent ticket is gone with it. This relies on Scion's `--workspace-subdir`
  subtree-isolation feature (fork branch `feat/per-agent-workspace-subpath`,
  not yet upstreamed); it is **not in the pinned `scion.version`**, so
  dispatching workers today requires building Scion from that fork
  (`scion.source`) until the addition lands upstream.
- `lever up` self-heals an expired agent mTLS leaf: resume now re-stages a
  fresh enrolment ticket before reconnecting, so an instance left down longer
  than the leaf's lifetime no longer needs a full `lever destroy && lever up`
  to recover. Adds a `lever doctor` check that detects the expired-leaf
  handshake failure in the broker log.
- `lever attach <worker>` and `lever msg send --to <worker>` now target the
  single instance project instead of a stale per-worker project path,
  fixing worker addressing under the single-project model. (P4 §9)

## [0.3.1] - 2026-07-06

### Changed
- Module path is now `github.com/stevegeek/lever`, matching the repository —
  `go install github.com/stevegeek/lever/cmd/lever@latest` works. (The old
  declared path `github.com/lever-to/lever` never resolved: no such repo, so
  v0.3.0 and earlier were build-from-clone only.)

## [0.3.0] - 2026-07-06

### Added
- Audit mint ledger: every capability token now carries a random 128-bit id
  inside the signed payload. The broker's `/request` allow line records the
  id, the matched policy rule (`obtain:<agent>:<tool>.<op>` /
  `delegate:<agent>-><recipient>:<tool>.<op>`), expiry, epoch, and the baked
  constraints (JSON); gateway and `/llm` lines carry the same id on allows AND
  on every post-decode deny (revoked replay included), so any use of a token
  — permitted or refused — greps back to its mint: `grep id=<id>
  .lever-state/broker.log`. Token bytes are never logged; deny-line ids are
  the token's claimed id (signature not necessarily valid). Tokens minted by
  earlier builds verify but log an empty id until they expire.
- `lever reload`: apply config changes (new grove, tool, or grant) to a running
  instance without a VM power cycle — restarts the broker on the current config
  while leaving the manager container (and its conversation) running.
- `make lever-image`: build the generic, instance-agnostic agent image
  (`scionlocal/lever-claude:latest`) in-repo — scion's stock harness plus the
  lever binaries and boot hook. Instances extend it `FROM lever-claude:latest`.
  The examples are now buildable from a clean checkout.
- `examples/assistant-demo`: a runnable mini personal-assistant instance (a
  morning-standup manager + a todo grove) that demonstrates both tool models in
  one place — a first-party capability tool (`lever-tool-todo`, reads a CSV) the
  broker supervises, and an external MCP (`weather-stub`, canned data) the broker
  only proxies — plus grove dispatch and per-agent grants. Offline, no API key.

### Fixed
- Revocation now fails closed on every acting path. Previously only a revoked
  agent's tool calls were denied (at the gateway/`/llm`), so it could still mint
  or delegate tokens, message other agents, dispatch/tear-down groves (as the
  manager), issue enrolment tickets, or renew its cert. `lever revoke <agent>`
  now denies all of these; the agent's existing cert simply expires (renew is
  refused), making revocation terminal.

### Docs
- New "Capabilities" and "Operations & recipes" guides; a "CLI" reference page;
  a security-model section on what a compromised agent can and can't do;
  disclosures on token-in-LLM-context (safe via CN-binding), the subscription
  vs api-key trade, and tree-resident boot material persisting across `--fresh`.

## [0.2.0] - 2026-07-04

### Added
- Operator skills: framework-authored `lever-operator` (manager) and
  `lever-agent` (grove) SKILL.md files teaching agents the capability flow
  (mint via `lever-capability` `request`, attach as `_capability`), messaging,
  and grove dispatch.
- `lever init [--force] [--check]`: scaffolds the skills into the instance
  tree (tree root + each declared grove dir) with hash-guarded updates
  (locally-modified files are skipped with a warning unless `--force`) and an
  idempotent marker block in the tree-root CLAUDE.md.
- `lever doctor` check "operator skills": present / current / unmodified /
  CLAUDE.md block present.
- Skill files carry a `lever-version` frontmatter stamp.

### Changed
- Version is now `0.2.0` (was `0.0.0-dev`).

## [0.1.0] - pre-changelog era

Everything before this changelog: the containment jail (OrbStack/Lima
backends, egress allowlisting), the capability broker and mTLS gateway
(enrolment, typed tokens, MCP-aware `_capability` gating, `/llm` api-key
proxy), external MCP tools, broker-routed messaging, resume-reconciliation
(`lever stop`/`up` restores the manager conversation), and `lever doctor`.
See git history for details.
