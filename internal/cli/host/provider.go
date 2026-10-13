package host

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/scion"
)

// errNoProvider is a project no agent can start in ("no_runtime_broker").
var errNoProvider = errors.New("the instance's hub project has no runtime broker linked")

// providerLink is ensureProjectProvider's seams.
type providerLink struct {
	// live is the hub as the controller PAT; admin as lever-admin.
	live, admin *hubapi.Client
	// jp is the project's jail path: its name on the hub, and the directory the
	// broker runs its agents from.
	jp  string
	log func(format string, args ...any)
}

// ensureProjectProvider makes the instance's hub project list a runtime
// broker, without which no agent starts.
//
// scion links a broker to a project only for a caller holding broker.update
// on that broker (its owner or a super-admin), which no token scope grants,
// so the controller PAT's hub link registers the project but links no
// broker. lever-admin, the hub's only admin, links it here. The jail's hub
// runs exactly one broker, its own; anything else is refused rather than
// guessed at.
//
// A project the hub does not list is not linked here: the settle steps after
// it fail on the same name.
func ensureProjectProvider(ctx context.Context, r providerLink) error {
	id, n, err := projectProviderCount(ctx, r.live, path.Base(r.jp))
	if err != nil || n != 0 {
		return err
	}
	brokers, err := r.admin.RuntimeBrokers(ctx)
	if err != nil {
		return fmt.Errorf("runtime broker: listing the hub's brokers as lever-admin: %w", err)
	}
	if len(brokers) != 1 {
		var ids []string
		for _, b := range brokers {
			ids = append(ids, b.ID+" ("+b.Name+")")
		}
		return fmt.Errorf("runtime broker: %w, and lever links only the jail's own broker, but the hub lists %d brokers [%s]; "+
			"check `scion hub brokers` in the jail, then run `lever apply`", errNoProvider, len(brokers), strings.Join(ids, ", "))
	}
	b := brokers[0]
	r.log("lever: linking the jail's runtime broker %s (%s) to hub project %s as lever-admin", b.ID, b.Name, id)
	if err := r.admin.AddProjectProvider(ctx, id, b.ID, r.jp); err != nil {
		return fmt.Errorf("runtime broker: linking broker %s to hub project %s as lever-admin: %w", b.ID, id, err)
	}
	if _, n, err = projectProviderCount(ctx, r.live, path.Base(r.jp)); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("runtime broker: %w after lever-admin linked broker %s; check `scion hub brokers` in the jail, then run `lever apply`", errNoProvider, b.ID)
	}
	return nil
}

// projectProviderCount is -1 for a project the hub does not list.
func projectProviderCount(ctx context.Context, hc *hubapi.Client, project string) (string, int, error) {
	id, err := hc.ProjectID(ctx, project, scion.DefaultHubEndpoint)
	if apiErr := (*hubapi.APIError)(nil); errors.As(err, &apiErr) && apiErr.NoProject {
		return "", -1, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("runtime broker: finding the hub project %q: %w", project, err)
	}
	ps, err := hc.ProjectProviders(ctx, id)
	if err != nil {
		return "", 0, fmt.Errorf("runtime broker: listing hub project %q's brokers: %w", project, err)
	}
	return id, len(ps), nil
}

// ensureProjectProvider backs Deps.EnsureProjectProvider.
func (w *applyWiring) ensureProjectProvider(ctx context.Context, jp string) error {
	return ensureProjectProvider(ctx, providerLink{
		live:  w.hub(),
		admin: w.assertHub(w.signer.Admin),
		jp:    jp,
		log:   w.log,
	})
}
