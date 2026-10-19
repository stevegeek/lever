package brokerctl

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/servicehost"
	"github.com/stevegeek/lever/internal/state"
)

// ServiceHostRoutes builds the gateway's route for every tool that runs on a
// service host. It fails closed: an unpaired host, or a state directory an
// agent could read the client key in, stops the broker.
func ServiceHostRoutes(app *config.App, st state.State) (map[string]broker.ServiceHostRoute, error) {
	if len(app.Broker.ServiceHosts) == 0 {
		return nil, nil
	}
	if inside, err := StateInsideTreeErr(app, st); inside {
		if err == nil {
			err = errors.New("the state directory is inside the tree")
		}
		return nil, fmt.Errorf("brokerctl: service hosts need the client keys out of the agents' reach: %w", err)
	}
	routes := map[string]broker.ServiceHostRoute{}
	for _, h := range app.Broker.ServiceHosts {
		tools := app.ServiceHostTools(h.Name)
		if len(tools) == 0 {
			continue
		}
		certPath, keyPath := st.ServiceHostClientCert(h.Name), st.ServiceHostClientKey(h.Name)
		_, _, err := servicehost.LoadClientKeyPair(certPath, keyPath)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("brokerctl: service host %q is not paired: run `lever service-host pair %s`", h.Name, h.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("brokerctl: service host %q client key: %w", h.Name, err)
		}
		pin, err := servicehost.ParseFingerprint(h.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("brokerctl: service host %q: %w", h.Name, err)
		}
		tr := servicehost.FileTransport(certPath, keyPath, pin)
		for _, t := range tools {
			routes[t.Name] = broker.ServiceHostRoute{Host: h.Name, Transport: tr}
		}
	}
	return routes, nil
}
