# Standing instructions: lever-dev manager

You develop Lever inside this jail. Stephen reviews, merges and releases.
Follow these rules in every session.

## Workflow

- Work on a branch named `agent/<topic>`.
- Run `go test ./...` and `gofmt -l .` before every push.
- Push with a git bundle. Create it with
  `git bundle create /workspace/.lever-files/github/<name>.bundle origin/main..<branch>`.
  You may use a full bundle instead of a range.
- Use a single plain file name for the bundle, with no leading dot and no
  directory part. The name must end in `.bundle`.
- Call the `github.push` tool with the repo, the branch and the bundle name.
- A host tool refusal comes back as `{"ok": false, "error": "..."}`. Read the
  error, fix the cause, and try again.
- Report the compare URL after a push. Stephen opens the PR, merges it and
  makes the release.
- Do not force push. If you rebase a branch, push it to a new branch name.

## Live end-to-end tests

- Run `make test-lima-e2e` (nested Lima). Run only one live e2e at a time.
- OrbStack e2e is not available. If a change needs it, say so in the PR notes.

## Browser tests

- Use only the Playwright Chromium. Never use another browser.

## Safety

- Never run `claude` inside the agent container of a nested Lever (#156).
- Never print `podman inspect` environment values or secret values. Prove a
  secret with its size and file mode.
- Never put a backtick in `git commit -m`. Write the message to a file with a
  heredoc and use `git commit -F`.
- Ask an independent subagent to review each significant change before you push.

## Fizzy

- Use the `fizzy` tool for the Lever board only.
- Move a card to the in-progress column when you start work.
- Ask Stephen for the in-progress column id, or read it from a `move_card` refusal.
- Comment on the card with the compare URL when you push.
- Never close a card. Stephen closes cards after the release.

## Stephen only

- Releases, tags, workflow changes, upstream PR comments, and installs on any
  host are for Stephen only. Do not do them. Ask him.
