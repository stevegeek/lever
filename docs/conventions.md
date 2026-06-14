# Conventions (recommended, not enforced)

Lever's core ships **no opinion in code** about how you organise your tree. The `lever` binary
requires only a tree root and a grove location; everything below is a *pattern*, not a rule.

The one convention that is genuinely framework-relevant is **groves**. The rest of this page
documents how the **reference instance** (the authors' personal assistant) organises itself — shown
as one worked example of an instance, not as something the framework expects you to adopt. Take what
helps, ignore the rest.

## Groves (framework-relevant)

A **grove** is a project directory an agent works in (`groves/<name>/`). The grove directory is a
plain, non-git directory; any git repositories live *inside* it. This keeps the runtime's project
model simple (a project is a directory) and lets a grove hold one or several repos. Groves are how
the manager hands isolated, bounded work to agents — so this is the one organisational idea the
framework actually leans on.

---

The conventions below are **the reference instance's**, included as an illustration of the kind of
structure an instance might layer on top of the core. None of it is in the `lever` binary.

## Projects vs areas (reference instance)

The reference instance separates two kinds of long-lived concern into different directories:

- **Project** — has a finish line. It ships, completes, and gets archived (a feature, a migration, a
  one-off deliverable). Triage question: *"is it still active, and when does it ship?"*
- **Area** — an ongoing responsibility with no finish line (maintenance, administration, a domain
  kept healthy). Triage question: *"am I keeping it healthy?"* An area never "completes."

Keeping them apart stops finite work and perpetual upkeep from being triaged by the same wrong
question.

## Archive convention (reference instance)

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

## A goals layer (reference instance, optional)

Above projects and areas, an optional **goals** layer captures long-running aspirations that are
never "done" — they get *served*. Each goal lists the projects and areas serving it. The value is
triage: when a project drifts, the question is not only "is it active?" but "does it still serve any
goal?" — and a project serving no goal is a candidate to archive.

## Task ↔ agent invariant (framework-relevant)

When the manager dispatches work to a grove, record it as a tracked task in your instance and pass a
correlation id at dispatch. The core echoes that id on a `completed` event; wire that to close the
task. The live agent stream tells you *how it's going*; your task records remain the authority on
*what* and *whether done*. (See [architecture.md §4](architecture.md) for the contract.)
