package host

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/servicehost"
	"github.com/stevegeek/lever/internal/state"
)

type serviceHostProbe func(ctx context.Context, address string, client tls.Certificate, pin servicehost.Fingerprint) (servicehost.Info, error)

// checkServiceHosts is one row per declared service host.
func checkServiceHosts(ctx context.Context, app *config.App, st state.State, probe serviceHostProbe) []checkResult {
	var out []checkResult
	for _, h := range app.Broker.ServiceHosts {
		out = append(out, checkServiceHost(ctx, app, st, h, probe))
	}
	return out
}

func checkServiceHost(ctx context.Context, app *config.App, st state.State, h config.ServiceHost, probe serviceHostProbe) checkResult {
	name := "service host " + h.Name
	tools := app.ServiceHostTools(h.Name)
	if len(tools) == 0 {
		return warnResult(name, "declared, but no broker tool has host: "+h.Name, "add a tool with host: "+h.Name+", or remove the entry")
	}
	cert, fp, err := servicehost.LoadClientKeyPair(st.ServiceHostClientCert(h.Name), st.ServiceHostClientKey(h.Name))
	if errors.Is(err, fs.ErrNotExist) {
		return checkResult{name, false, "not paired: no client key", "run `lever service-host pair " + h.Name + "`, put its fingerprint in the service host's client_fingerprint, then `lever apply`"}
	}
	if err != nil {
		return checkResult{name, false, "client key: " + err.Error(), "fix the key's mode, or `lever service-host pair " + h.Name + " --rotate`"}
	}
	pin, err := servicehost.ParseFingerprint(h.Fingerprint)
	if err != nil {
		return checkResult{name, false, err.Error(), "copy the fingerprint `lever service-host init` prints on the service host"}
	}
	info, err := probe(ctx, h.Address, cert, pin)
	var pe *servicehost.ProbeError
	if errors.As(err, &pe) {
		switch pe.Step {
		case "pin":
			return checkResult{name, false, "the server at " + h.Address + " is not the pinned one: " + pe.Err.Error(),
				"if the service host's key was remade, set fingerprint to what `lever service-host init` prints there"}
		case "refused":
			return checkResult{name, false, "the service host refused this instance's client key",
				"set client_fingerprint on the service host to " + string(fp) + ", then restart `lever service-host serve`"}
		case "answer":
			return checkResult{name, false, "unexpected answer: " + pe.Err.Error(), "check the service host's log"}
		}
	}
	if err != nil {
		return checkResult{name, false, "unreachable at " + h.Address + ": " + err.Error(),
			"start `lever service-host serve` there, and check that the tailnet ACL admits this host to the port"}
	}
	served := map[string][]string{}
	var names []string
	for _, t := range info.Tools {
		served[t.Name] = t.Allow
		names = append(names, t.Name)
	}
	var missing, outside []string
	for _, t := range tools {
		allow, ok := served[t.Name]
		if !ok {
			missing = append(missing, t.Name)
			continue
		}
		if slices.Equal(allow, []string{servicehost.AllOperations}) {
			continue
		}
		for _, o := range t.Operations {
			if !slices.Contains(allow, o.Name) {
				outside = append(outside, t.Name+"."+o.Name)
			}
		}
	}
	detail := fmt.Sprintf("reachable at %s, mutual TLS ok; serves %s", h.Address, strings.Join(names, ", "))
	switch {
	case len(missing) > 0:
		return checkResult{name, false, detail + "; not served there: " + strings.Join(missing, ", "),
			"add the tool(s) to the service host's config, or move them off host: " + h.Name}
	case len(outside) > 0:
		return warnResult(name, detail+"; outside its ceiling, every call refused: "+strings.Join(outside, ", "),
			"add the operation(s) to the tool's allow list on the service host, or drop them from operations")
	}
	return checkResult{name, true, detail, ""}
}
