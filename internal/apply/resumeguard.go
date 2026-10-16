package apply

import (
	"context"
	"errors"
	"fmt"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/wait"
)

// ManagerResume guards one resume of the manager outside the start-manager
// step with the same checks start-manager runs, so no path reports the
// manager up over mounts lever has not read back:
//
//   - BeginManagerResume, before the resume: the host tree has no symbolic
//     links (config.App.PrepareManagerReadOnlyHost) and no recorded mount
//     directory is gone (checkStaleTreeMounts);
//   - Resumed, right after the resume call: every tree mount's source is read
//     back from the guest; a mismatch suspends the manager;
//   - Live, once the manager is live, or Settle on a path with no gate: the
//     check again if it could not run the first time, and a warning if it
//     still cannot;
//   - AfterFailure, when the resume call or the gate failed: a container the
//     resume started anyway is checked before the failure is reported.
//
// The caller must not report the manager up while Resumed, Live or Settle
// returned an error. The checks run detached from the caller's cancellation
// (bounded), so an interrupt does not skip them.
type ManagerResume struct {
	r        *run
	project  string
	verified bool
}

// BeginManagerResume runs the checks before a resume. project is the in-jail
// tree root; subject prefixes the messages. An error means: do not resume.
func BeginManagerResume(ctx context.Context, d Deps, app *config.App, project, subject string) (*ManagerResume, error) {
	m := &ManagerResume{r: &run{app: app, d: d, subject: subject}, project: project}
	// Fail closed: without the two readers the read-back would pass without
	// looking (verifyResumedTreeMounts treats them as optional for tests).
	if d.InspectContainerMounts == nil || d.CheckLiveMounts == nil {
		return nil, fmt.Errorf("%s: internal error: the manager's mount checks are not wired, so lever does not resume it", subject)
	}
	if err := app.PrepareManagerReadOnlyHost(); err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	// Every caller resumes the record, so a gone directory refuses.
	if err := m.r.checkStaleTreeMounts(ctx, project, false); err != nil {
		return nil, err
	}
	return m, nil
}

// Resumed checks the resumed manager's tree mounts. An error means the mounts
// are not what lever planned: the manager was suspended (or the error says it
// could not be).
func (m *ManagerResume) Resumed(ctx context.Context) error {
	v, err := m.r.verifyResumedTreeMounts(ctx, m.project)
	m.verified = v
	return err
}

func (m *ManagerResume) Live(ctx context.Context) error {
	if m.verified {
		return nil
	}
	v, err := m.r.verifyResumedTreeMounts(ctx, m.project)
	m.verified = v
	if err == nil && !v {
		m.r.warnUnverifiedResume()
	}
	return err
}

// AfterFailure is for a resume call or gate that failed with cause: the
// resume may still have started the container, so its mounts are read back
// (a mismatch suspends it) and joined to cause.
func (m *ManagerResume) AfterFailure(ctx context.Context, cause error) error {
	if m.verified {
		return cause
	}
	if _, err := m.r.verifyResumedTreeMounts(ctx, m.project); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (m *ManagerResume) Verified() bool { return m.verified }

var settleBudget = budgetOf(wait.ManagerMountsVerified)

// Settle is Live for a path with no liveness gate: it retries until the check
// runs, for up to settleBudget, then warns if it never did.
func (m *ManagerResume) Settle(ctx context.Context) error {
	if m.verified {
		return nil
	}
	var verr error
	// Detached, like each check: an interrupt must not end the wait.
	_ = wait.For(context.WithoutCancel(ctx), settleBudget.spec(wait.ManagerMountsVerified.Name), func(context.Context) wait.Result {
		v, err := m.r.verifyResumedTreeMounts(ctx, m.project)
		m.verified, verr = v, err
		if v {
			return wait.Ready()
		}
		return wait.NotYetErr(err)
	})
	if verr == nil && !m.verified {
		m.r.warnUnverifiedResume()
	}
	return verr
}
