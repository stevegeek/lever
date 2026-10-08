---
title: Conventions
nav_order: 8
---
# Conventions (recommended, not enforced)

Lever's core ships **no opinion in code** about how you organise your tree. The `lever` binary
requires only `name`, `backend`, and `tree`, plus `broker.api_key_file` under the default
`llm_auth: api-key` and one `scion` source at bring-up (workers are optional); everything below is
a *pattern*, not a rule.

Two conventions are framework-relevant: **workers** and the **task ↔ agent invariant**.

## Workers (framework-relevant)

A **worker** is a Scion agent bound to a subdirectory of the instance's tree (`workers/<name>/`), in
the one Scion project the manager and every worker run in. The worker's subdirectory is a plain,
non-git directory; any git repositories live *inside* it. This keeps the runtime's project model
simple (one instance, one Scion project, in-place subdirectory workspaces) and lets a worker's
directory hold one or several repos. Workers are how the manager hands isolated, bounded work to
agents.

## Task ↔ agent invariant (framework-relevant)

When the manager dispatches work to a worker, record it as a tracked task in your instance. The core
relays Scion's agent events verbatim; correlating an event back to your task is an instance
convention (e.g. have the agent echo a task id in its messages), not something the core tracks. The
live agent stream tells you *how it's going*; your task records remain the authority on *what* and
*whether done*. (See [architecture.md §4](/architecture/) for the dispatch model.)
