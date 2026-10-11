---
title: "The github tool"
nav_order: 5.851
parent: Tools
permalink: /github-tool/
---
The `github` broker tool (`lever-tool-github`) lets an agent publish a branch to GitHub without
holding any GitHub credential. Part of [tools](/tools/); see also
[giving agents MCP tools](/getting-started/mcp-tools/).

## What it does

The agent builds a git bundle inside the jail and calls the `push` operation. The tool runs on the
host, checks the bundle, and pushes it to GitHub with a short-lived token.

**The guarantee:** the jail never holds a GitHub credential. The tool can push only branches that
start with the configured prefix (default `agent/`), and it never forces a push.

By default the only operation is `push`, with the arguments `repo`, `branch` and `bundle`. You then
open the PR yourself: the `push` result includes a compare URL for this.

You can also offer four more operations with the `-ops` flag: `pr_open`, `pr_status`, `pr_merge`
and `release` (see [6. PRs, merges and releases](#6-prs-merges-and-releases-opt-in)). They are off
by default. `pr_open` and `release` publish text under the App's name, `pr_merge` changes the
default branch, and `release` starts your release workflow. Offer them only on repositories where
you want this, and grant each one only to the agents that need it.

## Requirements

The host needs git 2.46.0 or newer on `PATH`. Older git does not run fsck on the objects that
`git fetch` imports from a bundle. The tool checks the version and refuses to start with an older
git. The git that macOS supplies (Apple Git 2.39) is too old: install git from Homebrew or another
source.

## 1. Create the GitHub App

Do not use a personal PAT. A PAT acts as you, the repository admin. A leaked PAT could push a `v*`
tag and trigger the release workflow on unreviewed code, and an admin bypass defeats rulesets.

1. Create a GitHub App owned by you.
2. Give it **Contents: read and write** and no other permission. It has no Workflows permission, so
   GitHub rejects any push that changes `.github/workflows/`. Agents cannot change CI. The opt-in
   operations need more permissions (see [section 6](#6-prs-merges-and-releases-opt-in)). Add only
   those for the operations you offer. Never add Workflows or Administration.
3. Install it on one repository only.
4. Generate a private key. Save it as a regular file with mode `0600`, owned by the user that runs
   the broker, outside the instance tree (the agent can read the tree). The tool refuses to start
   otherwise.
5. Note the app id and the installation id.

The tool mints an installation token from the key, narrowed to the one repository and
`contents: write`. The token lasts 1 hour and stays in memory. The tool reuses it for later pushes
to that repository until less than 10 minutes remain, then mints a new one. Each opt-in operation
gets its own token, narrowed to the one repository and the permissions that operation needs, and
cached the same way.

## 2. Add rulesets

| Ruleset | Target | Rules | Bypass |
|---|---|---|---|
| `main` | `refs/heads/main` | Require a pull request. Block force pushes and deletion. | Repository admin |
| release tags | `refs/tags/v*` | Restrict creation, update and deletion. | Repository admin |

The App is not an admin. A leaked key cannot write to `main` and cannot create a release tag. The
release workflow also refuses a tag whose commit is not on `main`.

These rulesets stay the hard backstop when you offer the opt-in operations. `pr_merge` needs no
change to the `main` ruleset: a merge through a pull request satisfies "require a pull request".
Add required status checks to it, so GitHub itself refuses a merge before CI passes. `release` needs
the App in the bypass list of the release tags ruleset. Section 6 explains both.

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

To offer the opt-in operations as well, add them to `-ops`, list them under `operations`, and grant
each one to the agents that may use it:

```yaml
broker:
  tools:
    - name: github
      command: [/home/you/.local/bin/lever-tool-github,
                # -tree, -state, -app-id, -installation-id, -app-key as above
                -repos, owner/name,
                -ops, "push,pr_open,pr_status,pr_merge,release",
                -merge-methods, squash,
                -merge-checks, any]
      backend: 127.0.0.1:3210
      operations:
        - {name: push, params: [repo, branch, bundle]}
        - {name: pr_open, params: [repo, branch, title, body]}
        - {name: pr_status, params: [repo, number]}
        - {name: pr_merge, params: [repo, number, method]}
        - {name: release, params: [repo, tag, notes]}
      allowed_values:
        repo: [owner/name]
manager:
  obtain:
    - {tool: github, op: push}
    - {tool: github, op: pr_open}
    - {tool: github, op: pr_status}
    - {tool: github, op: pr_merge}
    - {tool: github, op: release}
```

Each operation is its own capability. An agent with only `push` and `pr_open` can open PRs but not
merge them. Grant `pr_merge` and `release` only to an agent whose job includes merging and
releasing.

The broker adds `-backend` and `-admin` itself. Every other setting is a flag in `command`:

| Flag | Required | Meaning |
|---|---|---|
| `-tree` | yes | Absolute path of the instance tree. |
| `-state` | yes | Absolute path of the state directory, outside `-tree`. It holds the per-repo mirrors and temporary files. |
| `-app-id`, `-installation-id` | yes | The GitHub App. |
| `-app-key` | yes | Absolute path of the app private key, outside `-tree`. |
| `-repos` | yes | Comma list of `owner/name` the tool may push to. |
| `-branch-prefix` | no | Required branch prefix. Default `agent/`. |
| `-max-bundle` | no | Bundle size cap. Default 256 MiB. |
| `-import-budget` | no | Bundle bytes that one caller can import in one hour. Refused pushes count. The count is in memory and starts again when the tool restarts. Default 1 GiB. `0` turns the cap off. |
| `-name` | no | Tool name. Default `github`. It must equal the `name` in the config. |
| `-ops` | no | Comma list of the operations to offer: `push`, `pr_open`, `pr_status`, `pr_merge`, `release`. Default `push`. An operation not listed does not exist. |
| `-merge-methods` | no | Comma list of the merge methods `pr_merge` may use: `squash`, `merge`, `rebase`. The first is the default. Default `squash`. |
| `-merge-checks` | no | What `pr_merge` requires of the checks on the PR head. `any`: at least one check, and all green. `none`: no check is needed, but any that exist must be green. A comma list of check names: these checks must be present and successful, and all others green. Default `any`. |
| `-release-tag` | no | Pattern that a `release` tag must match, against the whole tag (Go regular expression). Default `^v[0-9]+\.[0-9]+\.[0-9]+$`. A matching tag must also be a semantic version, with an optional leading `v`. |
| `-text-max` | no | Bytes of a PR body or of release notes. Default 8000, at most 65536. |

The tool keeps a bare mirror of each repository under `-state`. The mirror has no hooks and no
checkout, so no code from a bundle runs on the host. The mirror receives only `main` from GitHub.
Each push imports the bundle into a new temporary repository under `-state/tmp`, which borrows the
mirror objects. The tool deletes this repository after each call, also after a refusal. Thus a
refused bundle uses no disk space after the call, and one caller cannot use the objects of another
caller as prerequisites. Install the binary with `make install`.

## 4. The agent workflow

In the jail, the agent commits to a branch whose name starts with the prefix. It then writes a
bundle of the new commits into `/workspace/.lever-files/github/`:

```sh
git bundle create /workspace/.lever-files/github/<name>.bundle origin/main..<branch>
```

The agent then calls `push` with `repo`, `branch` and `bundle`. The `bundle` argument is the file
name only, for example `fix-1.bundle`. It is not a path. The name must match
`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,92}\.bundle$` (it cannot start with a dot). The bundle must contain exactly one ref, `refs/heads/<branch>`,
and no tags.

The tool never forces a push. If the agent rebases a branch that is already on GitHub, the push is
rejected. The agent must use a new branch name.

## 5. Refusals

A refusal returns to the agent as `{"ok": false, "error": "..."}`. The tool writes an audit line to
`tool-logs/github.log` for each call.

| Cause | Result |
|---|---|
| Bad bundle name, symlink, hard link, or file too big | Named refusal. Nothing is fetched. |
| Wrong ref set, tags, missing prerequisites, or an fsck failure | Named refusal. The tool deletes the temporary repository. |
| Caller over `-import-budget` | Named refusal before any git call on the bundle. |
| Pack over a size limit (see below) | Named refusal before any git call on the bundle. |
| Bad branch charset or prefix, or a protected name (`main`, `master`, `HEAD`) | Named refusal before any git call. |
| Repository not in `-repos` | Named refusal. |
| Another push to the same repository holds the mirror for more than 30 seconds | Refusal: retry shortly. |
| Non-fast-forward | GitHub rejects the push. The tool reports that the branch moved and to use a new branch name. |
| Token mint, GitHub or network error, or git timeout (5 minutes for one git call, 10 minutes for the whole call) | Redacted error text. Host paths show as `<state>`, `<tree>` or `<tmp>`. The tool does not retry. |
| git older than 2.46.0 | The tool refuses to start. The broker reports it down, and calls fail with 502. |
| App key file has the wrong mode or owner, or the key or `-state` is inside `-tree` | The tool refuses to start. The broker reports it down, and calls fail with 502. |

## 6. PRs, merges and releases (opt-in)

These operations exist only when `-ops` lists them. Each acts only on a repository in `-repos`, and
each call gets a token narrowed to that repository and to the permissions in the table below
(GitHub adds Metadata: read to every token). Each call writes an audit line (`github.pr_open`,
`github.pr_status`, `github.pr_merge`, `github.release`) with the caller, the repository, the PR
number or tag, and the result. For a title, body or notes, the line holds the length and the start
of the SHA-256 hash, never the text. The tool does not retry a call to GitHub.

| Operation | Arguments | App permissions it needs | GitHub settings it needs |
|---|---|---|---|
| `pr_open` | `repo`, `branch`, `title`, `body` | Contents: read, Pull requests: read and write | None. |
| `pr_status` | `repo`, `number` | Pull requests: read, Checks: read, Commit statuses: read | None. |
| `pr_merge` | `repo`, `number`, `method` (optional) | Contents: read and write, Pull requests: read and write, Checks: read, Commit statuses: read | The merge method allowed in the repository settings. Required status checks in the `main` ruleset are recommended. No bypass for the App. |
| `release` | `repo`, `tag`, `notes` | Contents: read and write | The App in the bypass list of the release tags ruleset. |

Add the permissions to the App, then accept the new permissions on the installation (GitHub asks
the repository owner). A token request for a permission that the App does not have fails, and the
call returns that error. Keep the Workflows permission off: agents still cannot change CI.

### `pr_open`

Opens a PR from `branch` into the repository's default branch. The tool reads the default branch
from GitHub; the agent cannot choose the base. The branch must start with the prefix and already
exist on GitHub (push it first). The title is 1 to 256 characters on one line. The body is at most
`-text-max` bytes. Both must be valid UTF-8, and the tool refuses control characters (except newline
and tab in the body) and Unicode bidirectional formatting characters. The result holds `number` and
`html_url`. If an open PR from that branch already exists, the tool opens no other: it returns that
PR with `existing: true`.

**Risk:** the title and body appear on GitHub under the App's name. On a public repository anyone
can read them. Grant `pr_open` only to agents whose text you accept in public.

### `pr_status`

Returns the PR's `state`, `merged`, `draft`, `mergeable`, `mergeable_state`, head branch and head
SHA, base branch, the combined commit status, and the check runs and commit statuses on the head SHA
(`name`, `kind`, `status`, `conclusion`). It reads at most 100 check runs and 100 statuses; when
there are more, `truncated` is true. It works on any PR in the repository and changes nothing.

### `pr_merge`

Merges a PR into the default branch. The tool refuses unless:

- the PR is open and not a draft,
- its head branch starts with the prefix and is in the same repository (no PR from a fork),
- its base is the default branch,
- GitHub reports it mergeable (when GitHub is still computing this, retry shortly),
- every check run on the head SHA completed with `success`, `neutral` or `skipped`, and every commit
  status is `success`,
- at least one check exists, unless `-merge-checks none`, and every check that `-merge-checks` names
  is present and succeeded, and
- `method` is in `-merge-methods` (the first is the default).

The tool then merges with the head SHA that it checked. If someone pushes to the branch after the
check, GitHub refuses the merge, and the agent must check again. The App cannot use an admin bypass.
The `main` ruleset still applies: if it requires an approving review, `pr_merge` succeeds only after
a person approves the PR. This keeps a person in the loop for every merge.

**Risk:** a merge changes the default branch. With Contents: write and Pull requests: write, a leaked
App key can merge any PR that meets your ruleset, so make the ruleset require the status checks you
trust (and a review, if you want one).

### `release`

Creates a GitHub release and its tag on the default branch's current head. The tool reads the head
SHA and creates the release at exactly that commit, so it can release only what is already merged.
The tag must match `-release-tag`, must not exist, and must be greater, by semantic version order,
than every existing tag that matches the pattern. The notes are at most `-text-max` bytes, with the
same character rules as a PR body. The release is named after the tag and is not a draft or a
prerelease. The result holds `html_url`. The tool makes one release at a time.

The release tags ruleset blocks the App by default. To offer `release`, add the App to that
ruleset's bypass list. Do not add it to the `main` ruleset.

**Risk:** the new tag starts your release workflow, which builds and publishes a version. The notes
appear under the App's name. With the App in the tag bypass list, a leaked key can create a `v*` tag
on any commit, not only on the default branch: keep the release workflow's check that the tag's
commit is on `main`. If your workflow creates the release itself, it must accept an existing one
(GoReleaser keeps an existing release by default). Grant `release` only to an agent whose job
includes releases.

| Refusal | Result |
|---|---|
| Repository not in `-repos`, bad number, tag or method, title or text out of bounds, or a refused character | Named refusal before any GitHub call. |
| Branch missing on GitHub, PR closed, merged, draft, from a fork, wrong base or head prefix, not mergeable | Named refusal. |
| A check pending or failed, no check, a named check missing, or more than 100 checks | Named refusal, with the check names. |
| Head moved after the check | GitHub refuses the merge. Check again. |
| Tag exists, is not greater, or an existing matching tag is not a semantic version, or more than 2000 tags | Named refusal. |
| Token mint or GitHub error, a redirect, or an answer over 4 MiB | Redacted error text. The tool does not retry. |

## Bundle limits

A small, highly compressible bundle can declare very large objects. git `index-pack` holds a delta
base and its result in memory, so the tool reads the pack in the bundle before git does. It refuses
the bundle if:

- the pack has more than 2,000,000 objects,
- one object, delta base or delta result is larger than 100 MiB (GitHub refuses such files), or
- the sum of the inflated sizes is larger than 4 GiB. For a delta, both the inflated delta data and
  the delta result count toward this sum.

git then runs with `pack.threads=1` and `core.bigFileThreshold=16m`. A bundle at these limits can
make `index-pack` use approximately 200 to 300 MiB of memory for one call. The tool sets no
operating-system memory limit on git.

## Hazard: never run git in the tree on the host

The jail mount writes files as your host user. An agent controls the git repositories inside the
tree, and `safe.directory` does not protect them. A git command that you run on the host inside
such a repository can run agent-set `core.fsmonitor`, hooks, `diff.external` or filters on the
host.

- Never `cd` into the jail source tree on the host.
- Turn off shell-prompt and IDE git integration for that path.
- Build host binaries only from a separate, reviewed clone.

The tool itself never touches these repositories. It uses its own mirror under `-state`.

The mirrors under `-state` grow with the history of `main`, because the tool sets `gc.auto=0`. Run
`git --git-dir=<mirror> gc` now and then, or delete the mirror directory. The tool creates it again
on the next push.
