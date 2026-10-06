package apply

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/retry"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/termsafe"
)

// AgentSessionProbe reads an agent's session from inside its container and
// can make the agent report its session running (jail.AgentProbe in
// production). Every answer is the agent's own word — it controls its
// container — so it only ever decides whether lever runs a heal on THAT
// agent's own record, never what anything else may do.
type AgentSessionProbe interface {
	HubToken(ctx context.Context, ref string) (jail.HubTokenTimes, error)
	HarnessAlive(ctx context.Context, ref string) (bool, error)
	ReportSessionRunning(ctx context.Context, ref string) error
}

// sessionHealSettle bounds the re-read of the phase after the agent reported
// its session running: sciontool's hub call is synchronous, so the first
// list normally shows it.
var sessionHealSettle = RetryBudget{Attempts: 3, Interval: time.Second}

// HealAgentSession repairs two faults of an agent whose container is live,
// both of which leave every other signal green, and returns the record as
// it now stands (rec itself when nothing changed):
//
//   - An expired hub token. sciontool refreshes the agent's
//     10 h token 2 h before expiry with a Go timer, which stands still while
//     the host sleeps; once the token expired it cannot refresh itself, and
//     every agent-to-hub call (replies, status, heartbeats) fails with 401.
//     The heal is `scion reset-auth` (scion.Client.ResetAuth): a new token of
//     the agent's stored role, written by scion's runtime broker.
//   - A hub phase "stopped" over a harness that is still running.
//     sciontool reports phase stopped on ANY SessionEnd hook in the
//     container, and every claude process there shares the agent's hook
//     settings — so a `claude mcp list` run through `podman exec` marks the
//     agent stopped when it exits. The hub then refuses attach, and resumes
//     the record with a FRESH session (it passes --continue for a suspended
//     record only). The heal is the agent reporting its session running
//     itself (AgentSessionProbe.ReportSessionRunning), the same hook Claude
//     Code runs at start.
//
// The token heal runs first: the phase report travels on the agent's token.
// Best-effort: every failure is logged with its manual fix and the record is
// returned as last seen. A nil probe or log, a nil record, a container that
// is not live, or another phase skip the heal.
//
// What an agent gains by lying to the probes is a heal of its own record: a
// forged expired token buys it a fresh token of its own stored role (which it
// can already refresh), a forged live harness buys a "running" phase (which
// its own token may already post). Neither touches another agent or widens a
// role.
//
// A stopped phase can also be scion's own, set by a stop whose container is
// still shutting down; a heal racing it reports running for a harness about
// to die. That corrects itself: running is not a terminal phase, so the
// runtime broker's next heartbeat moves the record to the container's real
// state, and the liveness gates that follow a heal fail on a dead container.
func HealAgentSession(ctx context.Context, h SessionHealer, project string, rec *scion.Agent) *scion.Agent {
	sc, probe, log := h.Scion, h.Probe, h.Log
	if probe == nil || log == nil || rec == nil || !scion.ContainerLive(rec.ContainerStatus) {
		return rec
	}
	if rec.Phase != scion.PhaseRunning && rec.Phase != scion.PhaseStopped {
		return rec
	}
	if h.Revoked != nil && h.Revoked(rec.Slug) {
		// `lever revoke` is the kill-switch: no token reset, no probe of its
		// container, no session report on its behalf.
		log("agent %q is revoked (lever revoke), or the revocation list cannot be read; lever does not heal its session", rec.Slug)
		return rec
	}
	ref := jail.ContainerName(path.Base(project), rec.Slug)
	healHubToken(ctx, h, project, rec.Slug, ref)
	if rec.Phase != scion.PhaseStopped {
		return rec
	}
	alive, err := probe.HarnessAlive(ctx, ref)
	if err != nil {
		log("WARNING: agent %q has hub phase stopped over a live container, and lever could not tell whether its harness still runs (%v)", rec.Slug, err)
		return rec
	}
	if !alive {
		return rec
	}
	log("agent %q: the hub says its session ended (phase stopped), but claude still runs in its container — a SessionEnd from another claude process there (e.g. `claude mcp list` through podman exec) does this; reporting the session running again", rec.Slug)
	if err := probe.ReportSessionRunning(ctx, ref); err != nil {
		log("WARNING: agent %q: could not report its session running (%v); a resume now restarts claude in a new session (no --continue) — the old conversation stays in the agent home (`lever attach`, then `/resume`)", rec.Slug, err)
		return rec
	}
	healed := rec
	b := sessionHealSettle
	_ = retry.Until(ctx, b.Attempts, b.Interval, func() (bool, error) {
		agents, err := sc.List(ctx, project)
		if err != nil {
			return false, nil
		}
		if a := scion.FindAgent(agents, rec.Slug); a != nil {
			healed = a
			return a.Phase == scion.PhaseRunning, nil
		}
		return false, nil
	})
	if healed.Phase == scion.PhaseRunning {
		log("agent %q: session reported running again; its conversation continues", rec.Slug)
		return healed
	}
	log("WARNING: agent %q: the hub still reads phase %s after the session report (is its hub token valid? see `lever doctor`); a resume now restarts claude in a new session (no --continue) — the old conversation stays in the agent home (`lever attach`, then `/resume`)",
		rec.Slug, scion.BoundedQuote(healed.Phase))
	return healed
}

// SessionHealer is what HealAgentSession acts with: the scion client, the
// in-container probe, the pre-role record guard and the log line sink.
type SessionHealer struct {
	Scion *scion.Client
	Probe AgentSessionProbe
	// VerifyRole is the pre-role record guard (Deps.VerifyAgentRole: project
	// key, agent). The hub mints a reset token from the record's stored role,
	// and a record created before scion#1089 carries a role scion's
	// migration grandfathered to full (or, on older pins, none, which
	// resolved to full) — so the reset runs only once the guard passes. nil
	// ⇒ no reset (the expiry is still logged with its fix).
	VerifyRole func(ctx context.Context, project, agent string) error
	// Revoked reports an agent (by slug) the broker has revoked: `lever
	// revoke` stays the kill-switch, so its session is not healed at all (no
	// token reset, no probe, no session report). nil ⇒ no
	// revocation is known. A read error must answer true (fail closed).
	Revoked func(agent string) bool
	Log     func(string, ...any)
}

// healHubToken runs `scion reset-auth` for an agent whose hub token expired.
func healHubToken(ctx context.Context, h SessionHealer, project, slug, ref string) {
	sc, probe, log := h.Scion, h.Probe, h.Log
	tok, err := probe.HubToken(ctx, ref)
	if err != nil {
		log("WARNING: could not read agent %q's hub token expiry (%v); `lever doctor` shows the token row", slug, err)
		return
	}
	if !tok.Expired() {
		return
	}
	expired := fmt.Sprintf("its hub token expired at %s (guest clock %s) — every reply, status and heartbeat it sends fails with 401",
		tok.Expiry.Format(time.RFC3339), tok.Now.Format(time.RFC3339))
	if h.VerifyRole == nil {
		log("WARNING: agent %q: %s; it is not reset here — run `lever apply`", slug, expired)
		return
	}
	if err := h.VerifyRole(ctx, path.Base(project), slug); err != nil {
		log("WARNING: agent %q: %s; it is NOT reset: %s", slug, expired, termsafe.Sanitize(err.Error()))
		return
	}
	log("agent %q: %s; resetting it (scion reset-auth)", slug, expired)
	if err := sc.ResetAuth(ctx, slug, project); err != nil {
		log("WARNING: agent %q: scion reset-auth failed: %s — in the guest, run `scion reset-auth %s -g %s` with the controller PAT", slug, termsafe.Sanitize(scion.ErrSummary(err)), slug, project)
		return
	}
	if now, err := probe.HubToken(ctx, ref); err == nil && !now.Expired() {
		log("agent %q: new hub token, valid until %s", slug, now.Expiry.Format(time.RFC3339))
		return
	}
	log("WARNING: agent %q: scion reset-auth answered, but its token file does not show a valid token yet; re-run `lever doctor` in a minute", slug)
}

// HealSessions runs HealAgentSession over the manager and every configured
// worker, from one listing. `lever up` calls it for a manager it found
// running (it bypasses Run); Run heals the manager before it converges it,
// and the workers after.
func HealSessions(ctx context.Context, d Deps, app *config.App, project string) {
	healNamedSessions(ctx, d, project, append([]string{app.Name}, workerNames(app)...))
}

// healNamedSessions heals the named agents' sessions from one listing.
//
// For a worker this runs without the broker's per-worker lifecycle lock
// (that lock lives in the broker process, which apply cannot see), so it can
// race a start, resume, stop or suspend the manager ordered through the
// broker. Both heals are safe against that: a reset-auth on a container that
// is going away fails or writes a token the next start replaces (a start
// mints its own), and a session report over a dying harness sets phase
// running, which is not terminal — the runtime broker's next heartbeat moves
// the record to the container's real state. The broker's own token watch
// does take the lock.
func healNamedSessions(ctx context.Context, d Deps, project string, names []string) {
	if d.AgentSession == nil || d.Log == nil || len(names) == 0 {
		return
	}
	agents, err := d.Scion.List(ctx, project)
	if err != nil {
		d.Log("WARNING: could not list agents to check their hub sessions: %s", termsafe.Sanitize(scion.ErrSummary(err)))
		return
	}
	for _, name := range names {
		HealAgentSession(ctx, d.sessionHealer(), project, scion.FindAgent(agents, name))
	}
}

func workerNames(app *config.App) []string {
	out := make([]string, 0, len(app.Workers))
	for _, w := range app.Workers {
		out = append(out, w.Name)
	}
	return out
}

// sessionHealer is the SessionHealer apply acts with.
func (d Deps) sessionHealer() SessionHealer {
	return SessionHealer{Scion: d.Scion, Probe: d.AgentSession, VerifyRole: d.VerifyAgentRole, Revoked: d.AgentRevoked, Log: d.Log}
}
