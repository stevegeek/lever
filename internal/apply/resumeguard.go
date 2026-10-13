package apply

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/retry"
)

// ManagerResume guards one resume of the manager outside the start-manager
// step: `lever up`'s own resume (also after a guest restart, lever#179),
// `lever conversation restore` and the resume after a failed restore or
// backup, and the broker's re-enrol bounce. It runs what start-manager runs
// around its resume (lever#158), so no path reports the manager up over
// mounts lever has not read back:
//
//   - BeginManagerResume, before the resume: the host tree
//     (config.App.PrepareManagerReadOnlyHost: every read_only entry, worker
//     dir and shared folder a real directory reached through no symbolic
//     link) and the mounts the record holds (checkStaleTreeMounts: a
//     directory that is gone refuses the resume);
//   - Resumed, right after the resume call: where every tree mount comes
//     from, read back from the guest (verifyResumedTreeMounts). A mismatch
//     suspends the manager and returns the error;
//   - Live, once the manager is live (after the caller's gate), or Settle
//     on a path with no gate: the same check again when it could not run
//     the first time (the container was not running yet), and a warning if
//     it still cannot;
//   - AfterFailure, when the resume call or the gate failed: a container
//     the resume started anyway is checked before the failure is reported.
//
// The caller must not report the manager up while Resumed, Live or Settle
// returned an error. The checks run detached from the caller's
// cancellation (bounded), so an interrupt does not skip them.
type ManagerResume struct {
	r        *run
	project  string
	verified bool
}

// BeginManagerResume runs the checks before a resume of the manager of app
// in project (the in-jail tree root). subject prefixes the messages ("up",
// "conversation", "broker"). An error means: do not resume.
func BeginManagerResume(ctx context.Context, d Deps, app *config.App, project, subject string) (*ManagerResume, error) {
	m := &ManagerResume{r: &run{app: app, d: d, subject: subject}, project: project}
	// Fail closed: without the two readers the read-back would pass
	// without looking (verifyResumedTreeMounts treats them as optional
	// for apply's tests).
	if d.InspectContainerMounts == nil || d.CheckLiveMounts == nil {
		return nil, fmt.Errorf("%s: internal error: the manager's mount checks are not wired, so lever does not resume it", subject)
	}
	if err := app.PrepareManagerReadOnlyHost(); err != nil {
		return nil, fmt.Errorf("%s: %w", subject, err)
	}
	// Every caller resumes the record (none keeps it running as it is), so
	// a gone directory refuses, as for apply's resume.
	if err := m.r.checkStaleTreeMounts(ctx, project, false); err != nil {
		return nil, err
	}
	return m, nil
}

// Resumed checks the resumed manager's tree mounts as soon as the resume
// call returned. An error means the mounts are not what lever planned: the
// manager was suspended (or the error says it could not be).
func (m *ManagerResume) Resumed(ctx context.Context) error {
	v, err := m.r.verifyResumedTreeMounts(ctx, m.project)
	m.verified = v
	return err
}

// Live checks again, once the manager is live, when Resumed could not run
// the check; it warns if the check still cannot run.
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

// AfterFailure is for a resume call or a gate that failed with cause: the
// resume may still have started the container, so its mounts are read
// back (a mismatch suspends it) and joined to cause. A container that is
// not running says nothing.
func (m *ManagerResume) AfterFailure(ctx context.Context, cause error) error {
	if m.verified {
		return cause
	}
	if _, err := m.r.verifyResumedTreeMounts(ctx, m.project); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Verified reports whether the resumed manager's mounts were read back.
func (m *ManagerResume) Verified() bool { return m.verified }

// settleBudget bounds Settle's wait for a container to run.
var settleBudget = RetryBudget{Attempts: 30, Interval: time.Second}

// Settle is Live for a path with no liveness gate after its resume: it
// checks again until the check runs, for up to settleBudget, then warns
// if it never did.
func (m *ManagerResume) Settle(ctx context.Context) error {
	if m.verified {
		return nil
	}
	var verr error
	// Detached, like each check: an interrupt must not end the wait.
	_ = retry.Until(context.WithoutCancel(ctx), settleBudget.Attempts, settleBudget.Interval, func() (bool, error) {
		v, err := m.r.verifyResumedTreeMounts(ctx, m.project)
		m.verified, verr = v, err
		return v, nil
	})
	if verr == nil && !m.verified {
		m.r.warnUnverifiedResume()
	}
	return verr
}
