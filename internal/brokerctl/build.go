// Package brokerctl is the host-side controller for the lever capability broker:
// it translates a lever config into a broker.Config, ensures the CA + capability
// signing root key, supervises first-party tool subprocesses, runs the broker,
// and stops the host-side daemons. The state directory it reads and writes is
// package state's.
package brokerctl

import (
	"fmt"
	"maps"
	"slices"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/broker/rules"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// llmSentinelBackend is the Backend value of the reserved llm pseudo-tool. It
// satisfies registry.Register's non-empty-Backend invariant but is NEVER dialed:
// the llm capability is exercised by the broker /llm proxy, not an /mcp/<name>/ tool route.
const llmSentinelBackend = "lever:llm-proxy"

// BuildBroker assembles a broker.Config from the parsed app config: the
// request/delegation policy (from manager+worker grants), the pre-loaded tool
// registry (config-authoritative envelopes; caveat_param is the config-declared
// guard, the tool re-supplies it at /register), the agent list, and TTLs. The
// caller supplies the keys/CA/tickets (EnsureKeys).
func BuildBroker(app *config.App, keys token.KeyPair, caInst *ca.CA, tickets *ca.TicketStore) (broker.Config, error) {
	pol := rules.NewPolicy()
	addGrants := func(cn string, obtain []config.Grant, delegate []config.DelegateGrant) {
		for _, g := range obtain {
			pol.AllowObtain(cn, g.Tool, g.Op)
		}
		for _, d := range delegate {
			pol.AllowDelegate(cn, d.Tool, d.Op, d.To...)
		}
	}
	addGrants(app.ManagerCN(), app.Manager.Obtain, app.Manager.Delegate)
	for _, g := range app.Workers {
		addGrants(g.Name, g.Obtain, g.Delegate)
	}

	reg := registry.New()
	for _, t := range app.Broker.Tools {
		ops := make(map[string]registry.Operation, len(t.Operations)+1)
		for _, o := range t.Operations {
			// Cloned so the registry (which takes ownership) never aliases the parsed config.
			ops[o.Name] = registry.Operation{Name: o.Name, CaveatParam: maps.Clone(o.CaveatParam),
				Params: slices.Clone(o.Params)}
		}
		coarse := t.External && t.EffectiveGate() == config.GateCoarse
		if coarse {
			// A coarse external tool's whole surface rides one wildcard
			// capability; the synthetic op satisfies the registry's
			// at-least-one-operation invariant and lets /request mint
			// {tool, "*"} through the standard HasOperation gate.
			ops[registry.WildcardOp] = registry.Operation{Name: registry.WildcardOp}
		}
		if err := reg.Register(registry.Tool{
			Name:          t.Name,
			Backend:       t.Backend,
			Operations:    ops,
			AllowedValues: registry.CloneAllowedValues(t.AllowedValues),
			// External servers are third-party: the broker holds + enforces
			// the rules and strips the token (they never see a capability).
			FirstParty: !t.External,
			External:   t.External,
			Coarse:     coarse,
		}); err != nil {
			return broker.Config{}, fmt.Errorf("brokerctl: register tool %q: %w", t.Name, err)
		}
	}

	if app.AnyAPIKeyAgent() {
		if err := reg.Register(registry.Tool{
			Name:       broker.ReservedLLMTool,
			Backend:    llmSentinelBackend,
			Operations: map[string]registry.Operation{broker.ReservedLLMOp: {Name: broker.ReservedLLMOp}},
			FirstParty: true,
		}); err != nil {
			return broker.Config{}, fmt.Errorf("brokerctl: register reserved llm tool: %w", err)
		}
	}

	cfg := broker.Config{
		Keys:            keys,
		CA:              caInst,
		Tickets:         tickets,
		Rules:           pol,
		Registry:        reg,
		ManagerIdentity: app.ManagerCN(),
		GrantTTL:        app.Broker.GrantTTL,
		TicketTTL:       app.Broker.TicketTTL,
		LLMUpstream:     app.Broker.LLMUpstream, // empty ⇒ broker defaults to api.anthropic.com
	}

	// Load the api_key_file into the broker config so the /llm proxy has the
	// key. This is host-side only; the key never enters a container.
	// Defense-in-depth: ReadSecret re-checks 0600 here even though
	// config.Validate also checks it — brokerctl may be invoked outside the
	// apply/validate path. An absent file reads as "" and is rejected below.
	if app.AnyAPIKeyAgent() {
		key, err := state.ReadSecret(app.Broker.APIKeyFile, "api_key_file")
		if err != nil {
			return broker.Config{}, fmt.Errorf("brokerctl: %w", err)
		}
		if key == "" {
			return broker.Config{}, fmt.Errorf("brokerctl: api_key_file %q is empty", app.Broker.APIKeyFile)
		}
		cfg.APIKey = []byte(key)
	}

	return cfg, nil
}
