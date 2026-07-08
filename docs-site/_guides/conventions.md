---
title: Conventions
nav_order: 8
---
# Conventions (recommended, not enforced)

Lever's core ships **no opinion in code** about how you organise your tree. The `lever` binary
requires only `name`, `backend`, and `tree` (workers are optional); everything below is a *pattern*,
not a rule.

The one convention that is genuinely framework-relevant is **workers**. The rest of this page
documents how the **reference instance** (the authors' personal assistant) organises itself, shown
as one worked example of an instance, not as something the framework expects you to adopt. Take what
helps, ignore the rest.

## Workers (framework-relevant)

A **worker** is a Scion agent bound to a subdirectory of the instance's tree (`workers/<name>/`), in
the one Scion project the manager and every worker run in. The worker's subdirectory is a plain,
non-git directory; any git repositories live *inside* it. This keeps the runtime's project model
simple (one instance, one Scion project, in-place subdirectory workspaces) and lets a worker's
directory hold one or several repos. Workers are how the manager hands isolated, bounded work to
agents, so this is the one organisational idea the framework actually leans on.

## Reference-instance conventions (not enforced)

The patterns below are **the reference instance's**, shown as one illustration of structure an
instance might layer on the core. None of it is in the `lever` binary.

### Projects vs areas

The reference instance separates two kinds of long-lived concern into different directories:

- **Project**, has a finish line. It ships, completes, and gets archived (a feature, a migration, a
  one-off deliverable). Triage question: *"is it still active, and when does it ship?"*
- **Area**, an ongoing responsibility with no finish line (maintenance, administration, a domain
  kept healthy). Triage question: *"am I keeping it healthy?"* An area never "completes."

### Archive convention

When a project leaves the active set, the reference instance moves it to an archive directory with a
one-word **outcome tag** at the top, drawn from a small fixed vocabulary so the archive stays
greppable:

| Tag | Meaning |
|---|---|
| `completed` | shipped, nothing more to do |
| `abandoned` | considered, decided against |
| `on-ice` | paused, may revive (note the revival trigger) |
| `superseded` | folded into another effort (name it) |
| `maintenance` | shipped, alive but quiet |

### A goals layer (optional)

Above projects and areas, an optional **goals** layer captures long-running aspirations that are
never "done", they get *served*. Each goal lists the projects and areas serving it. The value is
triage: when a project drifts, the question is not only "is it active?" but "does it still serve any
goal?", and a project serving no goal is a candidate to archive.

## Task ↔ agent invariant (framework-relevant)

When the manager dispatches work to a worker, record it as a tracked task in your instance. The core
relays Scion's agent events verbatim; correlating an event back to your task is an instance
convention (e.g. have the agent echo a task id in its messages), not something the core tracks. The
live agent stream tells you *how it's going*; your task records remain the authority on *what* and
*whether done*. (See [architecture.md §4](/architecture/) for the dispatch model.)
