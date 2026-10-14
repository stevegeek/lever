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
// can make the agent report its session running. Every answer is the agent's
// own word (it controls its container), so it only ever decides whether lever
// heals THAT agent's own record, never what anything else may do.
type AgentSessionProbe interface {
	HubToken(ctx context.Context, ref string) (jail.HubTokenTimes, error)
	HarnessAlive(ctx context.Context, ref string) (bool, error)
	ReportSessionRunning(ctx context.Context, ref string) error
}

// sessionHealSettle is short because sciontool's hub call is synchronous, so
// the first list normally shows the new phase.
var sessionHealSettle = RetryBudget{Attempts: 3, Interval: time.Second}

// HealAgentSession repairs two faults of an agent whose container is live,
// both of which leave every other signal green, and returns the record as it
// now stands (rec itself when nothing changed):
//
//   - An expired hub token. sciontool refreshes the token with a Go timer,
//     which stands still while the host sleeps; once expired it cannot refresh
//     itself and every agent-to-hub call fails with 401. The heal is
//     `scion reset-auth`: a new token of the agent's stored role.
//   - A hub phase "stopped" over a harness that still runs. sciontool reports
//     stopped on ANY SessionEnd hook in the container (e.g. a `claude mcp list`
//     through `podman exec`); the hub then refuses attach and resumes with a
//     fresh session instead of --continue. The heal is the agent reporting its
//     session running itself.
//
// The token heal runs first: the phase report travels on the agent's token.
// Best-effort: failures are logged with their manual fix and the record is
// returned as last seen.
//
// Lying to the probes only heals the agent's own record: a forged expired
// token buys a fresh token of its own stored role, a forged live harness a
// "running" phase its own token may already post. Neither touches another
// agent or widens a role.
//
// A heal racing scion's own stop reports running for a dying harness; running
// is not terminal, so the next heartbeat corrects it, and the liveness gates
// after a heal fail on a dead container.
func HealAgentSession(ctx context.Context, h SessionHealer, project string, rec *scion.Agent) *scion.Agent {
	sc, probe, log := h.Scion, h.Probe, h.Log
	if probe == nil || log == nil || rec == nil || !scion.ContainerLive(rec.ContainerStatus) {
		return rec
	}
	if rec.Phase != scion.PhaseRunning && rec.Phase != scion.PhaseStopped {
		return rec
	}
	if h.Revoked != nil && h.Revoked(rec.Slug) {
		// `lever revoke` is the kill-switch: no token reset, no probe, no
		// session report on its behalf.
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
		log("lever: warning: agent %q has hub phase stopped over a live container, and lever could not tell whether its harness still runs (%v)", rec.Slug, err)
		return rec
	}
	if !alive {
		return rec
	}
	log("agent %q: the hub says its session ended (phase stopped), but claude still runs in its container — a SessionEnd from another claude process there (e.g. `claude mcp list` through podman exec) does this; reporting the session running again", rec.Slug)
	if err := probe.ReportSessionRunning(ctx, ref); err != nil {
		log("lever: warning: agent %q: could not report its session running (%v); a resume now restarts claude in a new session (no --continue) — the old conversation stays in the agent home (`lever attach`, then `/resume`)", rec.Slug, err)
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
	log("lever: warning: agent %q: the hub still reads phase %s after the session report (is its hub token valid? see `lever doctor`); a resume now restarts claude in a new session (no --continue) — the old conversation stays in the agent home (`lever attach`, then `/resume`)",
		rec.Slug, scion.BoundedQuote(healed.Phase))
	return healed
}

// SessionHealer is what HealAgentSession acts with.
type SessionHealer struct {
	Scion *scion.Client
	Probe AgentSessionProbe
	// VerifyRole is the pre-role record guard (Deps.VerifyAgentRole). The hub
	// mints a reset token from the record's stored role, and an old record's
	// role may have been grandfathered to full by scion's migration, so the
	// reset runs only once the guard passes. nil means no reset.
	VerifyRole func(ctx context.Context, project, agent string) error
	// Revoked reports an agent (by slug) the broker has revoked; its session
	// is not healed at all. nil means no revocation is known. A read error
	// must answer true (fail closed).
	Revoked func(agent string) bool
	Log     func(string, ...any)
}

func healHubToken(ctx context.Context, h SessionHealer, project, slug, ref string) {
	sc, probe, log := h.Scion, h.Probe, h.Log
	tok, err := probe.HubToken(ctx, ref)
	if err != nil {
		log("lever: warning: could not read agent %q's hub token expiry (%v); `lever doctor` shows the token row", slug, err)
		return
	}
	if !tok.Expired() {
		return
	}
	expired := fmt.Sprintf("its hub token expired at %s (guest clock %s) — every reply, status and heartbeat it sends fails with 401",
		tok.Expiry.Format(time.RFC3339), tok.Now.Format(time.RFC3339))
	if h.VerifyRole == nil {
		log("lever: warning: agent %q: %s; it is not reset here — run `lever apply`", slug, expired)
		return
	}
	if err := h.VerifyRole(ctx, path.Base(project), slug); err != nil {
		log("lever: warning: agent %q: %s; it is NOT reset: %s", slug, expired, termsafe.Sanitize(err.Error()))
		return
	}
	log("agent %q: %s; resetting it (scion reset-auth)", slug, expired)
	if err := sc.ResetAuth(ctx, slug, project); err != nil {
		log("lever: warning: agent %q: scion reset-auth failed: %s — in the guest, run `scion reset-auth %s -g %s` with the controller PAT", slug, termsafe.Sanitize(scion.ErrSummary(err)), slug, project)
		return
	}
	if now, err := probe.HubToken(ctx, ref); err == nil && !now.Expired() {
		log("agent %q: new hub token, valid until %s", slug, now.Expiry.Format(time.RFC3339))
		return
	}
	log("lever: warning: agent %q: scion reset-auth answered, but its token file does not show a valid token yet; re-run `lever doctor` in a minute", slug)
}

// HealSessions runs HealAgentSession over the manager and every configured
// worker, from one listing.
func HealSessions(ctx context.Context, d Deps, app *config.App, project string) {
	healNamedSessions(ctx, d, project, append([]string{app.Name}, workerNames(app)...))
}

// healNamedSessions runs without the broker's per-worker lifecycle lock, so
// it can race a lifecycle op the manager ordered. Both heals are safe: a
// reset-auth on a dying container fails or writes a token the next start
// replaces, and a "running" report over a dying harness is corrected by the
// next heartbeat.
func healNamedSessions(ctx context.Context, d Deps, project string, names []string) {
	if d.AgentSession == nil || d.Log == nil || len(names) == 0 {
		return
	}
	agents, err := d.Scion.List(ctx, project)
	if err != nil {
		d.Log("lever: warning: could not list agents to check their hub sessions: %s", termsafe.Sanitize(scion.ErrSummary(err)))
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

func (d Deps) sessionHealer() SessionHealer {
	return SessionHealer{Scion: d.Scion, Probe: d.AgentSession, VerifyRole: d.VerifyAgentRole, Revoked: d.AgentRevoked, Log: d.Log}
}
