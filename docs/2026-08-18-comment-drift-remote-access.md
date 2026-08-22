# Engineering note: comment drift (remote-access arc, 2026-08-18)

Written at the close of the remote-access feature (`internal/remoteproxy`,
the guest login bridge, the host-side web asset build). It records one
observation from that arc, because nothing in the toolchain catches the class
of defect it describes and the next person to hit it will hit it the same way.

## The observation

The feature produced **four doc comments that asserted behaviour the code
beside them did not have**. Each was written truthfully and each became false
later, when the function beneath it changed and the prose did not.

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

Two of the four caused a **live failure** rather than a review finding: (1)
failed the first live apply of the branch, and (2) presented as a hang with
every process apparently healthy. A third, (3), then misdescribed the recovery
for a fourth live bug (the missing egress grant for the login port), so the
`lever down` + `lever up` requirement read as something the code detected and
handled. The last, (4), was introduced by a fix in this same session.

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
