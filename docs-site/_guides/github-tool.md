---
title: "The github tool"
nav_order: 5.85
permalink: /github-tool/
---
The `github` broker tool (`lever-tool-github`) lets an agent publish a branch to GitHub without
holding any GitHub credential. Part of [giving agents MCP tools](/getting-started/mcp-tools/).

## What it does

The agent builds a git bundle inside the jail and calls the `push` operation. The tool runs on the
host, checks the bundle, and pushes it to GitHub with a short-lived token.

**The guarantee:** the jail never holds a GitHub credential. The tool can push only branches that
start with the configured prefix (default `agent/`), and it never forces a push.

The only operation is `push`, with the arguments `repo`, `branch` and `bundle`. There is no
`pr_create`: PR text is outward-facing, so you open the PR yourself. The `push` result includes a
compare URL for this.

## 1. Create the GitHub App

Do not use a personal PAT. A PAT acts as you, the repository admin. A leaked PAT could push a `v*`
tag and trigger the release workflow on unreviewed code, and an admin bypass defeats rulesets.

1. Create a GitHub App owned by you.
2. Give it **Contents: read and write** and no other permission. It has no Workflows permission, so
   GitHub rejects any push that changes `.github/workflows/`. Agents cannot change CI.
3. Install it on one repository only.
4. Generate a private key. Save it as a regular file with mode `0600`, owned by the user that runs
   the broker. The tool refuses to start otherwise.
5. Note the app id and the installation id.

The tool mints a token from the key for each push. The token lasts 1 hour and stays in memory.

## 2. Add rulesets

| Ruleset | Target | Rules | Bypass |
|---|---|---|---|
| `main` | `refs/heads/main` | Require a pull request. Block force pushes and deletion. | Repository admin |
| release tags | `refs/tags/v*` | Restrict creation, update and deletion. | Repository admin |

The App is not an admin. A leaked key cannot write to `main` and cannot create a release tag. The
release workflow also refuses a tag whose commit is not on `main`.

CI runs on `agent/**` pushes. In the repo, open Settings, Actions, General, Workflow permissions and
select "Read repository contents and packages permissions". The `GITHUB_TOKEN` must be read-only.

## 3. Configure the tool

```yaml
broker:
  tools:
    - name: github
      command: [/home/you/.local/bin/lever-tool-github,
                -tree, /home/you/lever-dev/jail-src,
                -state, /home/you/.local/state/lever-dev-github,
                -app-id, "<id>", -installation-id, "<id>",
                -app-key, /home/you/lever-dev/secrets/github-app.pem,
                -repos, owner/name]
      backend: 127.0.0.1:3210
      operations:
        - {name: push, params: [repo, branch, bundle]}
      allowed_values:
        repo: [owner/name]
manager:
  obtain:
    - {tool: github, op: push}
```

The broker adds `-backend` and `-admin` itself. Every other setting is a flag in `command`:

| Flag | Required | Meaning |
|---|---|---|
| `-tree` | yes | Absolute path of the instance tree. |
| `-state` | yes | Absolute path of the state directory. It holds the per-repo mirrors and temporary files. |
| `-app-id`, `-installation-id` | yes | The GitHub App. |
| `-app-key` | yes | Absolute path of the app private key. |
| `-repos` | yes | Comma list of `owner/name` the tool may push to. |
| `-branch-prefix` | no | Required branch prefix. Default `agent/`. |
| `-max-bundle` | no | Bundle size cap. Default 256 MiB. |
| `-name` | no | Tool name. Default `github`. It must equal the `name` in the config. |

The tool keeps a bare mirror of each repository under `-state`. The mirror has no hooks and no
checkout, so no code from a bundle runs on the host. Install the binary with `make install`.

## 4. The agent workflow

In the jail, the agent commits to a branch whose name starts with the prefix. It then writes a
bundle of the new commits into `/workspace/.lever-files/github/`:

```sh
git bundle create /workspace/.lever-files/github/<name>.bundle origin/main..<branch>
```

The agent then calls `push` with `repo`, `branch` and `bundle`. The `bundle` argument is the file
name only, for example `fix-1.bundle`. It is not a path. The name must match
`^[A-Za-z0-9._-]{1,93}\.bundle$`. The bundle must contain exactly one ref, `refs/heads/<branch>`,
and no tags.

The tool never forces a push. If the agent rebases a branch that is already on GitHub, the push is
rejected. The agent must use a new branch name.

## 5. Refusals

A refusal returns to the agent as `{"ok": false, "error": "..."}`. The tool writes an audit line to
`tool-logs/github.log` for each call.

| Cause | Result |
|---|---|
| Bad bundle name, symlink, hard link, or file too big | Named refusal. Nothing is fetched. |
| Wrong ref set, tags, missing prerequisites, or an fsck failure | Named refusal. The tool removes the temporary ref and directory. |
| Bad branch charset or prefix, or a protected name (`main`, `master`, `HEAD`) | Named refusal before any git call. |
| Repository not in `-repos` | Named refusal. |
| Non-fast-forward | GitHub rejects the push. The tool reports that the branch moved and to use a new branch name. |
| Token mint, GitHub or network error, or git timeout | Redacted error text. The tool does not retry. |
| App key file has the wrong mode or owner | The tool refuses to start. The broker reports it down, and calls fail with 502. |

## Hazard: never run git in the tree on the host

The jail mount writes files as your host user. An agent controls the git repositories inside the
tree, and `safe.directory` does not protect them. A git command that you run on the host inside
such a repository can run agent-set `core.fsmonitor`, hooks, `diff.external` or filters on the
host.

- Never `cd` into the jail source tree on the host.
- Turn off shell-prompt and IDE git integration for that path.
- Build host binaries only from a separate, reviewed clone.

The tool itself never touches these repositories. It uses its own mirror under `-state`.

The mirrors under `-state` grow, because the tool sets `gc.auto=0`. Run `git --git-dir=<mirror> gc`
now and then, or delete the mirror directory. The tool creates it again on the next push.
