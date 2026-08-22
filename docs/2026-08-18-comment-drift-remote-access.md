# Engineering note: comment drift (remote-access arc, 2026-08-18)

Written at the close of the remote-access feature (`internal/remoteproxy`,
the guest login bridge, the host-side web asset build). It records one
observation from that arc, because nothing in the toolchain catches the class
of defect it describes and the next person to hit it will hit it the same way.

## The observation

The feature produced **five doc comments that asserted behaviour the code
beside them did not have**. The first four were written truthfully and became
false later, when the function beneath them changed and the prose did not. The
fifth, found while fixing the branch's review findings, was never true.

1. **`scion.ServerStop`** — its doc said "AlreadyRunning's message set also
   covers scion's not-running wording", and invited every caller to "call it
   unconditionally during teardown". `AlreadyRunning` matches only
   "already running" and "already exists"; `scion server stop` on a stopped
   daemon says "server daemon is not running".

2. **The single-port login bridge** — the comments explained why one port
   number served both halves of the bridge. The container runtime mirrors a
   guest listener onto the host at the same number, so one number meant a
   forwarder dialling the mirror of itself.

3. **`guest.ApplyEgress`** — its doc claimed that "a genuine egress-config
   change [has] no active DROP and falls through to a full rebuild". The skip
   tests only that the chain is closed and carries a parseable alias; it never
   compares the live ruleset to the desired one, so a live closed instance
   does not pick up a new allowed port at all.

4. **`remoteController`** — its type comment said the spawn was
   fire-and-forget "so a startup failure surfaces in RemoteLog(), not as a
   synchronous apply error". That stopped being true when `Start` grew a wait
   for the proxy to bind. The fix had landed thirty lines below the comment
   describing it.

5. **`scion.ServerOpts.EnableWeb`** — its doc said "Only the remote-access
   feature turns this on; a headless hub stays API-only". scion applies
   workstation defaults to every `server start` that is not `--hosted`, and one
   of them enables the web frontend whenever the flag was *not* explicitly set
   (`cmd/server_config.go` `applyWorkstationDefaults`). So the hub serves a UI
   with remote access off exactly as with it on, and the daemon re-emits a bare
   `--enable-web` into its own `--foreground` child argv. The dangerous half is
   the correction rather than the error: lever must **not** "converge" the flag
   to false, because with the frontend off the Hub API stops being mounted on
   the web server and binds `cfg.Hub.Port` — 9810 — instead
   (`cmd/server_foreground.go`, the `!enableWeb` branch). That would move the
   Hub API off 8080, where the broker, every agent's `SCION_HUB_ENDPOINT`,
   `lever doctor` and the remote proxy all dial it. lever's whole model rested
   on a scion default that nothing in the tree wrote down.

Two of the five caused a **live failure** rather than a review finding: (1)
failed the first live apply of the branch, and (2) presented as a hang with
every process apparently healthy. A third, (3), then misdescribed the recovery
for a fourth live bug (the missing egress grant for the login port), so the
`lever down` + `lever up` requirement read as something the code detected and
handled. (4) was introduced by a fix in this same session.

(5) is a different animal, and worth naming as its own class. It did not
drift — nothing in lever changed under it. It described what the flag meant to
*lever* ("we set it only for remote access") and then asserted what *scion*
does with it, which scion never did. A comment that states an upstream
behaviour is a claim about a codebase nobody in this repo edits: no local
change will ever invalidate it, and no local review will ever have reason to
look at it again. It surfaced only because a fix was about to be built **on**
the claim — a review finding said the hub "keeps `--enable-web`" when remote
access is turned off — and the premise was checked against the pinned module
before the fix was written. Had it been implemented as described, the
"convergence" would have taken the instance down.

## Why it is worth a note

Nothing catches this. The compiler does not read comments; tests do not read
comments; review reads the diff, and in every case here the comment was **not
in the diff** — it sat above or beside the lines that changed, unchanged and
therefore invisible. Each one was found only when a human re-read the comment
against the code beneath it, usually while debugging something else.

The habit that catches it: **when you change what a function does, re-read the
whole comment on it, not the part your diff touches.** In particular re-read
any comment that makes a claim about a *sibling* — "X also covers Y", "the
caller may safely Z", "this falls through to W" — because those claims are
about code the diff does not show, and they are the ones that mislead.

## The corollary that actually protects the tree

A comment is not a durable guard, and neither is a commit message. The facts
from this arc that must survive a squash, a refactor or a rewrite live in
**tests and published docs**, and the comments point at them rather than
carrying the load alone. For example, "never pass `--base-url` to the hub" is
held by `TestRunScionServerKeepsAgentHubEndpointJailReachable` and by a section
of the remote-access guide; the comment on `ServerOpts.EnableWeb` explains the
reasoning, but deleting it would not let the mistake back in.

When a hard-won fact has no test and no doc — only prose — it is one
refactor away from being lost. Give it one of the other two.
