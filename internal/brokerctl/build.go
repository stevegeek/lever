// Package brokerctl is the host-side controller for the lever capability
// broker: it turns a lever config into a broker.Config, ensures the CA and
// capability signing key, supervises first-party tool subprocesses, runs the
// broker, and stops the host-side daemons.
package brokerctl

import (
	"errors"
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

// llmSentinelBackend is the Backend of the reserved llm pseudo-tool. It
// satisfies registry.Register's non-empty-Backend invariant but is never
// dialed: the broker's /llm proxy serves the llm capability.
const llmSentinelBackend = "lever:llm-proxy"

// errEmptyAPIKeyFile means api_key_file exists but holds only whitespace. An
// absent file is a different error (fs.ErrNotExist from package state).
var errEmptyAPIKeyFile = errors.New("is empty")

// BuildBroker assembles the config-derived groups of a broker.Config:
// Identity (request/delegation policy, the tool registry whose
// config-authoritative envelopes a tool re-supplies at /register, manager
// identity and TTLs) and LLM. decorateConfig fills the host-side groups.
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
			// Cloned so the registry, which takes ownership, never aliases the config.
			ops[o.Name] = registry.Operation{Name: o.Name, CaveatParam: maps.Clone(o.CaveatParam),
				Params: slices.Clone(o.Params)}
		}
		coarse := t.External && t.EffectiveGate() == config.GateCoarse
		if coarse {
			// A coarse external tool's whole surface rides one wildcard
			// capability; the synthetic op satisfies the registry's
			// at-least-one-operation invariant.
			ops[registry.WildcardOp] = registry.Operation{Name: registry.WildcardOp}
		}
		if err := reg.Register(registry.Tool{
			Name:          t.Name,
			Backend:       t.Backend,
			Operations:    ops,
			AllowedValues: registry.CloneAllowedValues(t.AllowedValues),
			// External servers are third-party: the broker enforces the rules
			// and strips the token (they never see a capability).
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
		Identity: broker.IdentityConfig{
			Keys:            keys,
			CA:              caInst,
			Tickets:         tickets,
			Rules:           pol,
			Registry:        reg,
			ManagerIdentity: app.ManagerCN(),
			// The manager's scion slug is the app name, not the manager cert CN.
			ManagerSlug:   app.Name,
			ManagerClaude: app.ManagerClaude(),
			GrantTTL:      app.Broker.GrantTTL,
			TicketTTL:     app.Broker.TicketTTL,
		},
		LLM: broker.LLMConfig{
			Upstream: app.Broker.LLMUpstream, // empty ⇒ broker defaults to api.anthropic.com
		},
	}

	// The api key is host-side only and never enters a container.
	// ReadRequiredSecret re-checks 0600 because brokerctl may run outside
	// the validate path.
	if app.AnyAPIKeyAgent() {
		key, err := state.ReadRequiredSecret(app.Broker.APIKeyFile, "api_key_file")
		if err != nil {
			return broker.Config{}, fmt.Errorf("brokerctl: %w", err)
		}
		if key == "" {
			return broker.Config{}, fmt.Errorf("brokerctl: api_key_file %q %w", app.Broker.APIKeyFile, errEmptyAPIKeyFile)
		}
		cfg.LLM.APIKey = []byte(key)
	}

	return cfg, nil
}
