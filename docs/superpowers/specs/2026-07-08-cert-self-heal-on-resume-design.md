# Agent-certificate self-heal on `lever up` + doctor check — design spec

**Status:** implemented 2026-07-08 · branch `fix/cert-selfheal-on-resume`

## Problem

A manager (or worker) agent's mTLS **leaf** certificate is short-lived (~24h). While the instance is up, an in-container renew sidecar keeps it fresh. But the sidecar cannot run while the instance is **stopped**, so any downtime longer than the leaf lifetime guarantees the leaf expires. On the next `lever up`, the manager resumes with the dead leaf: every brokered call fails the mTLS handshake (`x509: certificate has expired` → `remote error: tls: bad certificate` in `broker.out.log`), so all brokered tools and the control layer go dark while local data still works. Observed live 2026-07-08.

Two properties make this worse than it should be:

1. **Recovery required a full teardown.** `lever-agent` boot already re-enrols an expired leaf (`internal/agent/enrol.go` `ValidCert` → false), but only if a fresh, unspent enrolment ticket is staged at `<tree>/.lever/bootstrap.json`. The **resume** path staged none — only the create path called `ensureFreshBootstrap`. So a plain `lever up` (resume) couldn't heal it; only `lever destroy && lever up` (which re-stages a ticket via the create path) recovered, at the cost of a machine teardown.
2. **No diagnosis.** `lever doctor` had no check for it. A naive host-side CA check reads green throughout the outage — the CA is valid for years; it's the leaf that died. The only reliable signal is the broker's own handshake-error log.

## Fix

### 1. Self-heal on the resume path (`internal/apply/run.go`)

In `start-manager`'s `suspended`/`stopped` branch, call `ensureFreshBootstrap(ctx, d, boot)` **before** `Resume`. This stages a fresh enrolment ticket so boot re-enrols an expired leaf during the normal `up` resume — no teardown.

`ensureFreshBootstrap` is already correctly guarded, which makes this surgical rather than a blanket broker bounce:

- **Normal `stop`→`up`:** `stop` stops the broker; `up`'s `broker-up` restarts it → `/bootstrap` latch reopens → `mint-manager-bootstrap` mints fresh material this run (`boot.minted = true`) → resume-path `ensureFreshBootstrap` is a **no-op**. No second bounce.
- **Expired-leaf case (broker outlived a spent latch across downtime):** `mint-manager-bootstrap` tolerates the spent latch without minting (`boot.minted = false`) → resume-path `ensureFreshBootstrap` **re-arms** (bounces the broker) and stages a fresh ticket → boot re-enrols the dead leaf → healed.
- **Valid leaf, spent latch (rare):** re-arms and stages an unspent ticket; harmless — boot's `ValidCert` passes and skips enrol, leaving it unredeemed.

This deliberately overturns the prior "resume never re-arms" invariant (`TestStartManagerResumeNeverRearms`), whose rationale — "the agent home / enrol cert already exists, so re-arming is pointless" — was the bug: an existing-but-**expired** leaf is exactly the outage. Replaced by two tests: resume re-arms when no fresh material was minted this run, and resume does **not** double-bounce when it was.

### 2. Doctor check (`internal/cli/doctor_checks.go`)

`checkAgentCert` scans the tail of `broker.out.log` for the expired-/bad-certificate handshake fingerprint and parses each match's Go-stdlog timestamp (local time). If the most recent match is within `certRejectWindow` (15m), it reports a **failing** check ("brokered tools are down") with the fix `run \`lever up\``; older matches are treated as stale (a since-healed leaf), so the check self-clears after recovery. A missing/unreadable log passes (a missing broker is `checkBrokerAlive`'s job). Wired into `doctor.go` right after the broker-alive check.

## Scope & tradeoffs

- **In scope:** the resume-path self-heal and the doctor scan. Both are host-side, reuse existing machinery, and add no new broker/agent surface.
- **Deferred (complementary, not required):** lengthening the leaf lifetime (days, not hours) so downtime-expiry is rare in the first place — a broker-side CA signing-duration knob; and reading the leaf `notAfter` directly from inside the jail (more surgical than the log scan, but needs the container-home path and a new backend read seam).
- **Accepted cost:** in the anomalous "broker persisted with a spent latch while the manager was down and the leaf is still valid" state, resume bounces the broker once needlessly. It never happens on the normal `stop`→`up` path (mint already staged fresh material), and the bounce is at an agent-free moment (manager not yet up, workers not up).
- **Orthogonal to the single-project re-architecture** (`2026-07-08-lever-single-project-rearchitecture-design.md`): that spec carries the enrol/renew model forward unchanged, so this fix is needed regardless and lands independently.

## Tests

- `internal/apply/run_test.go`: `TestStartManagerResumeRearmsWhenNoFreshMaterial` (heal-then-resume, no fresh create), `TestStartManagerResumeSkipsRearmWhenAlreadyMinted` (no double bounce).
- `internal/cli/doctor_cert_test.go`: active rejection fails, stale rejection passes, `tls: bad certificate` fingerprint fails, no-rejection log passes, absent log passes, and the scan picks the newest match regardless of file order.
