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

You can also offer more operations with the `-ops` flag: `pr_open`, `pr_status`, `pr_merge`
and `release` (see [6. PRs, merges and releases](#6-prs-merges-and-releases-opt-in)), the
read-only `ci_runs` and `ci_log`, which let an agent see why CI failed (see
[7. CI runs and logs](#7-ci-runs-and-logs-opt-in)), and `issue_list`, `issue_read`,
`issue_comment` and `issue_close` for triaging the repository's issues (see
[8. Issues](#8-issues-opt-in)). They are off
by default. `pr_open` publishes text under the App's name, `pr_merge` changes the default branch,
and `release` creates a tag that starts your release workflow. Offer them only on repositories
where you want this, and grant each one only to the agents that need it. The riskier settings are
off unless you turn them on: `pr_merge` needs a person's approval unless you say otherwise.

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
| release tag creation | `refs/tags/v*` | Restrict creations. | Repository admin |
| release tag protection | `refs/tags/v*` | Restrict updates. Restrict deletions. | None |

The App is not an admin. A leaked key cannot write to `main` and cannot create a release tag. The
release workflow also refuses a tag whose commit is not on `main`.

Keep the tag rules in two rulesets. A bypass applies to every rule in its ruleset. If one ruleset
restricted creation, update and deletion, an actor that you allow to create tags could also move
or delete an existing release tag. With two rulesets, the creation ruleset can have a bypass list
(the App, if you offer `release`), and the protection ruleset has no bypass at all: nobody, not even
an admin, can move or delete a published `v*` tag.

These rulesets stay the hard backstop when you offer the opt-in operations. If you offer
`pr_merge`, also set these rules in the `main` ruleset:

- Require a pull request, with at least one required approval.
- Dismiss stale pull request approvals when new commits are pushed.
- Require approval of the most recent reviewable push.
- Require status checks to pass, with the checks you trust (pin each check's source app).

A merge through a pull request satisfies "require a pull request". The required approval and the
required status checks are the real enforcement: GitHub itself refuses a merge without them, even
if the tool's own checks were misconfigured. `release` needs the App in the bypass list of the
release tag creation ruleset only. Section 6 explains both.

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
                -ops, "push,pr_open,pr_status,pr_merge,release,ci_runs,ci_log,issue_list,issue_read,issue_comment,issue_close",
                -merge-methods, squash,
                -merge-checks, "test,lint"]  # required with pr_merge: names, any or none
                # pr_merge requires a person's approval by default
                # (-merge-require-review). Do not add -merge-require-review=false
                # unless you accept the risk in section 6.
      backend: 127.0.0.1:3210
      operations:
        - {name: push, params: [repo, branch, bundle]}
        - {name: pr_open, params: [repo, branch, title, body]}
        - {name: pr_status, params: [repo, number]}
        - {name: pr_merge, params: [repo, number, method]}
        - {name: release, params: [repo, tag]}
        - {name: ci_runs, params: [repo, branch, limit]}
        - {name: ci_log, params: [repo, job_id, lines]}
        - {name: issue_list, params: [repo, state, limit]}
        - {name: issue_read, params: [repo, number]}
        - {name: issue_comment, params: [repo, number, body]}
        - {name: issue_close, params: [repo, number, reason, body]}
      allowed_values:
        repo: [owner/name]
manager:
  obtain:
    - {tool: github, op: push}
    - {tool: github, op: pr_open}
    - {tool: github, op: pr_status}
    - {tool: github, op: pr_merge}
    - {tool: github, op: release}
    - {tool: github, op: ci_runs}
    - {tool: github, op: ci_log}
    - {tool: github, op: issue_list}
    - {tool: github, op: issue_read}
    - {tool: github, op: issue_comment}
    - {tool: github, op: issue_close}
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
| `-ops` | no | Comma list of the operations to offer: `push`, `pr_open`, `pr_status`, `pr_merge`, `release`, `ci_runs`, `ci_log`, `issue_list`, `issue_read`, `issue_comment`, `issue_close`. Default `push`. An operation not listed does not exist. |
| `-merge-methods` | no | Comma list of the merge methods `pr_merge` may use: `squash`, `merge`, `rebase`. The first is the default. Default `squash`. |
| `-merge-checks` | with `pr_merge` | What `pr_merge` requires of the checks on the PR head. A comma list of check names: these checks must be present and successful, and all others green. `any`: at least one successful check, and all green. `none`: no check is needed, but any that exist must be green. No default: when `-ops` offers `pr_merge`, the tool refuses to start without it. |
| `-merge-require-review` | no | `pr_merge` requires an approving review by a person on the PR's head SHA, and no outstanding request for changes. Default `true`. Turn it off only with `-merge-require-review=false`; the tool then logs a warning when it starts. |
| `-app-slug` | no | The GitHub App's slug. Reviews by this account never count as an approval. Reviews by any bot never count either, so this is a second guard. |
| `-release-tag` | no | Pattern that a `release` tag must match, against the whole tag (Go regular expression). Default `^v[0-9]+\.[0-9]+\.[0-9]+$`. A matching tag must also be a semantic version, with an optional leading `v`. |
| `-text-max` | no | Bytes of a PR body. Default 8000, at most 65536. |

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
| git older than 2.46.0 | The tool refuses to start. The broker reports it down, and calls fail with 503. |
| App key file has the wrong mode or owner, or the key or `-state` is inside `-tree` | The tool refuses to start. The broker reports it down, and calls fail with 503. |

## 6. PRs, merges and releases (opt-in)

These operations exist only when `-ops` lists them. Each acts only on a repository in `-repos`, and
each call gets a token narrowed to that repository and to the permissions in the table below
(GitHub adds Metadata: read to every token). Each call writes an audit line (`github.pr_open`,
`github.pr_status`, `github.pr_merge`, `github.release`) with the caller, the repository, the PR
number or tag, and the result. For a title or body, the line holds the length and the start of the
SHA-256 hash, never the text. The tool does not retry a call to GitHub.

| Operation | Arguments | App permissions it needs | GitHub settings it needs |
|---|---|---|---|
| `pr_open` | `repo`, `branch`, `title`, `body` | Contents: read, Pull requests: read and write | None. |
| `pr_status` | `repo`, `number` | Pull requests: read, Checks: read, Commit statuses: read | None. |
| `pr_merge` | `repo`, `number`, `method` (optional) | Contents: read and write, Pull requests: read and write, Checks: read, Commit statuses: read | The merge method allowed in the repository settings. Required approvals and status checks in the `main` ruleset (section 2). No bypass for the App. |
| `release` | `repo`, `tag` | Contents: read and write | The App in the bypass list of the release tag creation ruleset. |

Add the permissions to the App, then accept the new permissions on the installation (GitHub asks
the repository owner). A token request for a permission that the App does not have fails, and the
call returns that error. Keep the Workflows permission off: agents still cannot change CI.

### `pr_open`

Opens a PR from `branch` into the repository's default branch. The tool reads the default branch
from GitHub; the agent cannot choose the base. The branch must start with the prefix and already
exist on GitHub (push it first). The title is 1 to 256 characters on one line. The body is at most
`-text-max` bytes. Both must be valid UTF-8, and the tool refuses control characters (except newline
and tab in the body) and Unicode format characters such as bidirectional controls and zero-width
spaces (the zero-width joiner of emoji is allowed). The tool also refuses text that would act on
GitHub beyond the PR, each with a named error:

- a team mention such as `@org/team` (it notifies the whole team),
- an issue-closing keyword that references an issue: `close`, `closes`, `closed`, `fix`, `fixes`,
  `fixed`, `resolve`, `resolves` or `resolved`, in any case, followed by `#N`, `owner/repo#N` or an
  issue URL (the merge would close that issue; write `refs #N` instead),
- more than 5 different users `@`mentioned in the title and body together.

The tool checks only the title and body. GitHub also closes an issue from a closing keyword in a
commit message once that commit reaches the default branch, and the agent writes the commit
messages. Read them in review, or merge with `squash` and a repository setting that uses the PR
title and description as the squash commit message.

The result holds `number` and `html_url`. If an open PR from that branch already exists, the tool opens no other: it returns that
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
- every check that `-merge-checks` names is present and succeeded; with `any`, at least one check
  succeeded (skipped and neutral checks alone are not enough),
- a person approved the head SHA, and nobody's latest review requests changes (the review gate,
  below), and
- `method` is in `-merge-methods` (the first is the default).

The tool then merges with the head SHA that it checked. If someone pushes to the branch after the
check, GitHub refuses the merge, and the agent must check again. The App cannot use an admin bypass.
Never put the App in the bypass list of the `main` ruleset: then its merges would skip the review
and the required checks.

After the merge, the tool reads the PR again. If its base is no longer the default branch (someone
retargeted the PR between the check and the merge), the result has `retargeted: true`, the `base`
it merged into and a `warning`, and the tool writes a warning audit line. Check that branch.

GitHub refuses to merge a PR that changes `.github/workflows/` with the App's token, because the
App has no Workflows permission. Merge such a PR yourself.

**Which PRs:** `pr_merge` can merge any PR in the repository whose head is a prefix branch of the
same repository. That includes a PR from another agent's branch, or a PR that a person opened from a
branch with the prefix. The tool does not check who opened the PR or who pushed the branch. Keep the
prefix for agent work only, and grant `pr_merge` only to an agent that may merge all of it.

#### The review gate

CI runs the agent's own code. An agent that can open and merge PRs could make CI pass, for example
by deleting the tests, and then merge its own change. So by default (`-merge-require-review`)
`pr_merge` needs a person's approval of exactly the code it merges:

- The tool reads every review on the PR (100 a page, at most 1000; more is a refusal).
- For each reviewer, the latest review that approves, requests changes or was dismissed decides. A
  comment changes nothing.
- If any reviewer's latest such review requests changes, the tool refuses. This includes a reviewer
  from outside the repository; a person with write access can dismiss that review.
- At least one reviewer's latest such review must be an approval whose commit is the PR's current
  head SHA, by a person: a GitHub user, not a bot and not the App (`-app-slug`), whom GitHub lists
  as the repository's owner, a member of its organization or a collaborator. On a public repository
  anyone can submit an approving review; the tool ignores those, as GitHub does. An approval of an
  earlier commit does not count, so a push after the approval needs a new one.

An organization member counts even without write access to this repository. If that is too wide,
rely on the `main` ruleset's required approval, which counts only reviewers with write access.

The result and the audit line name the approvers. Set the same rule in the `main` ruleset as well
(section 2): required approval, dismiss stale approvals when new commits are pushed, and require
approval of the most recent reviewable push. Then GitHub enforces it too.

**Turning it off:** `-merge-require-review=false` removes the gate, and the tool logs a warning when
it starts. Then nothing in the tool stops an agent with `pr_open` and `pr_merge` from merging code
that no person has read, with CI made green by the same code. Do this only if the `main` ruleset
requires an approval, or if you accept that the agent decides alone what reaches the default branch.

#### Checks

`-merge-checks` has no default: when you offer `pr_merge`, you must choose. A list of check names
is the safest choice. With `any`, the tool merges as soon as every check that has reported is green
and at least one succeeded: a slower check that has not started yet does not stop it. The required
status checks in the `main` ruleset are the real enforcement, because GitHub waits for them.

The tool knows a check only by its name. It does not check which app or user created it. Anyone who
can post a check run or commit status on the head can satisfy a check name. CI runs the agent's
code, so keep the `GITHUB_TOKEN` read-only (section 2): then that code cannot post a status. For a
stronger guarantee, add the checks to the `main` ruleset as required status checks with their
source app pinned. GitHub then refuses the merge unless the right app reported them.

**Risk:** a merge changes the default branch. With Contents: write and Pull requests: write, a leaked
App key can merge any PR that meets your ruleset (the tool's own gates do not bind a leaked key), so
make the ruleset require the status checks you trust and an approval.

### `release`

Creates a tag on the default branch's current head, and nothing else: it creates no GitHub release.
The tool reads the head SHA and creates the tag at exactly that commit, so it can tag only what is
already merged. If the tag appears between the check and the creation, GitHub refuses the tag. The
tag must match `-release-tag`, must not exist, and must be greater, by semantic version order, than
every existing tag that matches the pattern. The tool makes one release at a time. The result holds
`tag`, `target_sha`, `html_url` (the tag's page) and `commit_url`.

The new tag starts the repository's release workflow (`on: push: tags`), and that workflow
(GoReleaser, for example) builds and creates the GitHub release with its notes. A tag that a GitHub
App token creates does trigger workflows, unlike one created with the workflow's own `GITHUB_TOKEN`.
This is per GitHub's documented behaviour, not tested live.

The release tag creation ruleset blocks the App by default. To offer `release`, add the App to that
ruleset's bypass list. Do not add it to the release tag protection ruleset or to the `main` ruleset.

**Risk:** the new tag starts your release workflow, which builds and publishes a version. With the
App in the tag creation bypass list, a leaked key can create a `v*` tag on any commit, not only on
the default branch: keep the release workflow's check that the tag's commit is on `main`. It still
cannot move or delete an existing tag (the protection ruleset has no bypass). Grant `release` only
to an agent whose job includes releases.

| Refusal | Result |
|---|---|
| Repository not in `-repos`, bad number, tag or method, title or text out of bounds, a refused character, a team mention, an issue-closing keyword or more than 5 mentioned users | Named refusal before any GitHub call. |
| Branch missing on GitHub, PR closed, merged, draft, from a fork, wrong base or head prefix, not mergeable | Named refusal. |
| A check pending or failed, no successful check, a named check missing, or more than 100 checks | Named refusal, with the check names. |
| No approval by a person on the head SHA, a reviewer's latest review requests changes, or more than 1000 reviews | Named refusal, with the reviewers' names. |
| Head moved after the check | GitHub refuses the merge. Check again. |
| PR retargeted between the check and the merge | Merged. The result has `retargeted: true` and a `warning`, and a warning audit line is written. |
| Tag exists, is not greater, or an existing matching tag is not a semantic version, or more than 2000 tags | Named refusal. |
| Token mint or GitHub error, a redirect, or an answer over 4 MiB | Redacted error text. The tool does not retry. |

## 7. CI runs and logs (opt-in)

`ci_runs` and `ci_log` read GitHub Actions and change nothing. They exist only when `-ops` lists
them, act only on a repository in `-repos`, and each call gets a token narrowed to that repository
and **Actions: read**. Add that permission to the App (and accept it on the installation); the
tool still has no Workflows permission, so agents can read CI but not change it. Each call writes
an audit line (`github.ci_runs`, `github.ci_log`) with the caller, the repository, the branch or
job id and how much it returned, never the log text.

| Operation | Arguments | App permissions it needs | GitHub settings it needs |
|---|---|---|---|
| `ci_runs` | `repo`, `branch`, `limit` (optional, 1-20, default 5) | Actions: read | None. |
| `ci_log` | `repo`, `job_id`, `lines` (optional, 1-400, default 150) | Actions: read | None. |

### `ci_runs`

Returns the newest workflow runs on `branch`, any branch of the repository (`main` too: reading is
not pushing): each run's `id`, `name`, `event`, `status`, `conclusion`, `head_sha`, `run_attempt`,
`html_url` and `created_at`. For the three newest runs that are not green (failed, cancelled, or
still running), it also lists their jobs, failed ones first: `id`, `name`, `conclusion` and the
names of the failed steps. It lists one page of 50 jobs a run; `jobs_truncated` says when a run has
more. A job's `id` is what `ci_log` takes.

### `ci_log`

Returns the end of one job's log: `tail`, its last `lines` lines, and `error_lines`, the first 60
lines in the whole log that look like a failure (GitHub's `##[error]` lines, Go's `--- FAIL`,
`FAIL` and `panic:`, `file.go:N:` compiler and vet messages, `error`/`fatal` at a line's start,
`exit code N`). Lines are plain text: GitHub's timestamps, terminal escape sequences and control
characters are removed, as are Unicode format characters (bidirectional controls, zero-width
characters) that could disguise text; each line is cut at 500 characters, and the token and known
credential shapes are masked. A very long line keeps its start and the read goes on after it. The
tool reads at most 64 MiB of a log, within the call's two-minute limit (`truncated` says when it
stopped early).

GitHub answers a log request with a redirect to a signed download URL. The tool follows it only
over https to GitHub's log hosts (`*.actions.githubusercontent.com`, `*.blob.core.windows.net`) and
sends no credential there; any other redirect is refused. A log GitHub no longer keeps (90 days by
default) is reported as not found or expired.

**Risk:** a log is whatever the workflow's commands printed, and the repository's code (which an
agent may have written) runs in that workflow. The result says so in its `note`: the agent should
read a log as data, not as instructions. GitHub masks the repository's secrets in logs; the tool's
masking is a second line, not a guarantee.

## 8. Issues (opt-in)

`issue_list` and `issue_read` read the repository's issues; `issue_comment` and `issue_close`
publish under the App's name. They exist only when `-ops` lists them, act only on a repository in
`-repos`, and each call gets a token narrowed to that repository and **Issues: read**
(`issue_list`, `issue_read`) or **Issues: read and write** (`issue_comment`, `issue_close`). Pull
requests are never touched: they are left out of lists, and reading, commenting on or closing one
is refused. Each call writes an audit line (`github.issue_list`, …) with the caller, the
repository and the issue number; for a comment, its length and the start of its SHA-256 hash,
never the text.

| Operation | Arguments | App permissions it needs |
|---|---|---|
| `issue_list` | `repo`, `state` (open, closed or all; default open), `limit` (1-50, default 20) | Issues: read |
| `issue_read` | `repo`, `number` | Issues: read |
| `issue_comment` | `repo`, `number`, `body` | Issues: read and write |
| `issue_close` | `repo`, `number`, `reason` (`completed` or `not_planned`; default `completed`), `body` (optional) | Issues: read and write |

- `issue_list` returns the newest issues: number, title, state and close reason, author, labels,
  comment count, dates and URL.
- `issue_read` returns one issue's summary, its body (cut at 20,000 characters) and its newest 30
  comments (each cut at 4,000). Text is cleaned like a CI log: escapes, control and format
  characters dropped, credential shapes masked. Issue text is written by whoever opened or
  commented on the issue, which on a public repository is anyone; the result's `note` says to read
  it as data, not instructions.
- `issue_comment` posts a comment. The body follows the `pr_open` body rules: at most
  `-text-max` bytes, no control or format characters, no team mention, no issue-closing keyword, at
  most 5 users `@`mentioned.
- `issue_close` closes an open issue with the reason, after posting `body` as a comment when one is
  given. An issue already closed is refused and nothing is posted. It never reopens, locks or
  deletes an issue, and never edits one.

**Risk:** comments appear on GitHub under the App's name, and closing an issue is visible to its
author. Grant `issue_comment` and `issue_close` only to agents whose triage you accept in public.

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
