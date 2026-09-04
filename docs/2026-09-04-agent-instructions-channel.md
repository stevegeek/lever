# 2026-09-04 — instructions_file: what was verified, and the one check left

Branch `feat/agent-instructions-channel` (lever#30). Standing instructions now
reach scion as inline agent config on stdin (`scion start --config -`, JSON)
instead of riding argv into the 16 KiB tmux word.

## Verified

- `go test ./...` green (48 packages); twelve mutations each fail exactly one
  new test (transport, byte-identity, budget arithmetic, tree containment,
  the 512 KiB cap at all three seams).
- The argv budget was measured, not derived: scion's construction replayed
  through a real `sh -c`, argv summed as tmux sums it — 261 bytes fixed for
  the claude harness, tmux 3.6b accepts 16362 and refuses 16365, one
  apostrophe costs 3 extra bytes (scion's second quoting pass is consumed by
  the container's own `sh -c`).
- Live: the assistant's config loads under the new binary with an unchanged
  13-step plan; `lever apply` restarted the broker and remote proxy and left
  the running manager alone (a resume carries no instructions by design).

## Not yet verified — needs the first fresh create

The only end-to-end dependency no unit test can prove is that the backend
prefix (`orb -m …` / the Lima equivalent) forwards stdin to `scion start`.
The image-load path already streams a binary through the same seam in
production, so this is expected to hold, but it is an assumption until an
agent is CREATED with an `instructions_file` set. On that first bring-up
(`lever worker purge` + dispatch, or a manager `lever up --fresh`), check in
the guest:

    <project-configs>/<slug>/.scion/agents/<agent>/home/.scion/harness/inputs/instructions.md

exists with the file's text, and after the container starts the agent's
`~/.claude/CLAUDE.md` carries it inside the `SCION MANAGED` markers under
"Agent Instructions". If `instructions.md` is absent, the stdin did not reach
scion and the agent booted with the mandatory preamble only.
