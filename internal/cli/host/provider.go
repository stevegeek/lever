package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/scion"
)

// errNoProvider is the reason the provider repair opens the dev-auth window.
var errNoProvider = errors.New("the instance's hub project has no runtime broker linked")

// providerRepair is ensureProjectProvider's seams.
type providerRepair struct {
	live    *hubapi.Client
	project string
	// window runs the bootstrap-token step with a required reason: its hub link
	// as the dev identity links the jail's runtime broker.
	window func(ctx context.Context, reason string) error
	// startHub starts the live hub with its normal options; waitBroker waits
	// for its runtime broker to register.
	startHub   func(ctx context.Context) error
	waitBroker func(ctx context.Context) error
	log        func(format string, args ...any)
}

// ensureProjectProvider makes the instance's hub project list a runtime broker,
// without which no agent starts ("no_runtime_broker").
//
// A hub link under the controller PAT cannot link one: scion links a broker to
// a project only for a caller holding broker.update on it, which no user
// access token scope grants. The dev identity in the window can, but the
// window's hub link knows the jail's broker only after the live hub has
// registered it, so a fresh instance's first window creates the project with
// none. Here, after the live hub registered its broker, a project without one
// gets a second window, and the hub starts again.
//
// A project the hub does not list is not repaired here: the settle steps
// after it fail on the same name.
func ensureProjectProvider(ctx context.Context, r providerRepair) (linked bool, err error) {
	n, err := projectProviderCount(ctx, r.live, r.project)
	if err != nil || n != 0 {
		return false, err
	}
	r.log("lever: %s; linking it in the dev-auth window and restarting the hub", errNoProvider)
	if err := r.window(ctx, errNoProvider.Error()); err != nil {
		return false, err
	}
	if err := r.startHub(ctx); err != nil {
		return true, fmt.Errorf("runtime broker: starting the hub after linking the broker: %w", err)
	}
	if err := r.waitBroker(ctx); err != nil {
		return true, fmt.Errorf("runtime broker: %w", err)
	}
	n, err = projectProviderCount(ctx, r.live, r.project)
	if err != nil {
		return true, err
	}
	if n == 0 {
		return true, fmt.Errorf("runtime broker: %w after the dev-auth window linked it; check `scion hub brokers` in the jail, then run `lever apply`", errNoProvider)
	}
	return true, nil
}

// projectProviderCount is -1 for a project the hub does not list.
func projectProviderCount(ctx context.Context, hc *hubapi.Client, project string) (int, error) {
	id, err := hc.ProjectID(ctx, project, scion.DefaultHubEndpoint)
	if apiErr := (*hubapi.APIError)(nil); errors.As(err, &apiErr) && apiErr.NoProject {
		return -1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("runtime broker: finding the hub project %q: %w", project, err)
	}
	ps, err := hc.ProjectProviders(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("runtime broker: listing hub project %q's brokers: %w", project, err)
	}
	return len(ps), nil
}

// ensureProjectProvider backs Deps.EnsureProjectProvider.
func (w *applyWiring) ensureProjectProvider(ctx context.Context, projectName string) (bool, error) {
	return ensureProjectProvider(ctx, providerRepair{
		live:    w.hub(),
		project: projectName,
		window: func(ctx context.Context, reason string) error {
			return ensureControllerPAT(ctx, w.jr, w.state, w.app.Tree, w.b.MountDest(), remoteAccessFor(w.app), w.patOpts(reason))
		},
		startHub: func(ctx context.Context) error {
			opts := apply.HubServerOpts(w.app, w.deps.HubSessionSecret)
			opts.Progress = w.deps.Log
			return w.sc.ServerStart(ctx, opts)
		},
		waitBroker: func(ctx context.Context) error {
			_, err := w.sc.RuntimeBrokerReady(ctx, "global")
			return err
		},
		log: w.log,
	})
}
