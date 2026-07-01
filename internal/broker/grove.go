package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/scion"
)

// groveBootstrap is a broker-local mirror of the bootstrap envelope written to
// <bootstrapDir>/bootstrap.json. JSON tags MUST remain byte-for-byte identical
// to agent.Bootstrap so the agent package's LoadBootstrap can decode it.
type groveBootstrap struct {
	Ticket    string `json:"ticket"`
	BrokerCA  string `json:"broker_ca"`
	BrokerURL string `json:"broker_url"`
	AgentCN   string `json:"agent_cn"`
}

// GroveRuntime is the subset of scion.Client the broker uses to drive grove
// agents host-side. *scion.Client satisfies it; tests inject a fake.
type GroveRuntime interface {
	List(ctx context.Context, project string) ([]scion.Agent, error)
	Start(ctx context.Context, o scion.StartOpts) error
	Resume(ctx context.Context, grove, project string) error
	Stop(ctx context.Context, grove, project string) error
	Suspend(ctx context.Context, grove, project string) error
	EnvSet(ctx context.Context, projectDir, key, value string) error
}

// GroveSpec is the config-derived, path-authoritative description of one grove.
// The broker never accepts any of these from the manager; they come from config.
type GroveSpec struct {
	Name         string // grove identity (== scion agent slug + project name)
	JailProject  string // jail-absolute -g/--workspace, e.g. /lever/groves/worker
	BootstrapDir string // host path to <tree>/<dir>/.lever (where bootstrap.json is staged)
	Image        string // effective agent image
	APIKey       bool   // true ⇒ api-key LLM mode for this grove
}

func (b *Broker) groveSpec(name string) (GroveSpec, bool) {
	s, ok := b.groves[name]
	return s, ok
}

type groveStartRequest struct {
	Grove string `json:"grove"`
	Task  string `json:"task"`
}

type groveResponse struct {
	Grove string `json:"grove"`
	Phase string `json:"phase"`
}

// runtimeReady returns true when the scion runtime is wired. When the runtime
// is nil (no LEVER_JAIL_USER/UID env, e.g. a manual `lever broker serve` with
// no prior `lever apply`), it writes a 502 and an audit line and returns false.
// MUST be called after authn/authz: it audits b.manager (correct only
// post-authn) and unauthenticated callers must receive 403, not 502.
func (b *Broker) runtimeReady(w http.ResponseWriter) bool {
	if b.runtime == nil {
		b.audit("grove", b.manager, "error", "runtime not wired")
		http.Error(w, "grove dispatch unavailable", http.StatusBadGateway)
		return false
	}
	return true
}

// requireManagerGrove authenticates the caller as the manager and authorizes the
// requested grove against config. Returns the resolved spec, or writes 403/502.
func (b *Broker) requireManagerGrove(w http.ResponseWriter, r *http.Request, grove string) (GroveSpec, bool) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("grove", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return GroveSpec{}, false
	}
	if caller != b.manager {
		b.audit("grove", caller, "deny", "not the manager identity")
		http.Error(w, "forbidden", http.StatusForbidden)
		return GroveSpec{}, false
	}
	spec, ok := b.groveSpec(grove)
	if !ok {
		b.audit("grove", caller, "deny", "unknown grove: "+grove)
		http.Error(w, "forbidden", http.StatusForbidden)
		return GroveSpec{}, false
	}
	// Runtime check is last — authn/authz above must fire first so an
	// unauthenticated caller gets 403, not 502.
	if !b.runtimeReady(w) {
		return GroveSpec{}, false
	}
	return spec, true
}

func (b *Broker) phaseOf(ctx context.Context, spec GroveSpec) (string, error) {
	agents, err := b.runtime.List(ctx, spec.JailProject)
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Slug == spec.Name {
			return a.Phase, nil
		}
	}
	return "", nil
}

func (b *Broker) handleGroveStart(w http.ResponseWriter, r *http.Request) {
	var req groveStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	spec, ok := b.requireManagerGrove(w, r, req.Grove)
	if !ok {
		return
	}
	ctx := r.Context()
	phase, err := b.phaseOf(ctx, spec)
	if err != nil {
		b.audit("grove", b.manager, "error", "phase: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	if phase == "running" {
		writeJSON(w, groveResponse{Grove: spec.Name, Phase: "running"})
		return
	}
	if phase != "" {
		// exists in a non-running state (suspended/stopped/terminal) → resume, never re-provision
		if err := b.runtime.Resume(ctx, spec.Name, spec.JailProject); err != nil {
			http.Error(w, "runtime error", http.StatusBadGateway)
			return
		}
		b.audit("grove", b.manager, "allow", "resume "+spec.Name)
		writeJSON(w, groveResponse{Grove: spec.Name, Phase: "running"})
		return
	}
	// phase == "" → absent: mint a one-use ticket, stage the grove's OWN bootstrap, start.
	ticket, err := b.tickets.Issue(spec.Name, b.ticketTTL)
	if err != nil {
		http.Error(w, "ticket error", http.StatusInternalServerError)
		return
	}
	bs := groveBootstrap{Ticket: ticket, BrokerCA: b.brokerCAPEM, BrokerURL: b.brokerURL, AgentCN: spec.Name}
	if err := stageBootstrap(spec.BootstrapDir, bs); err != nil {
		http.Error(w, "stage error", http.StatusInternalServerError)
		return
	}
	if spec.APIKey {
		if err := b.runtime.EnvSet(ctx, spec.JailProject, "LEVER_LLM_AUTH", "api-key"); err != nil {
			http.Error(w, "runtime error", http.StatusBadGateway)
			return
		}
	}
	if err := b.runtime.Start(ctx, scion.StartOpts{
		Grove: spec.Name, Task: req.Task, Harness: "claude",
		Project: spec.JailProject, Workspace: spec.JailProject,
		Image: spec.Image, APIKey: spec.APIKey,
	}); err != nil {
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	b.audit("grove", b.manager, "allow", "start "+spec.Name)
	writeJSON(w, groveResponse{Grove: spec.Name, Phase: "running"})
}

func (b *Broker) groveVerb(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, spec GroveSpec) error) {
	var req struct {
		Grove string `json:"grove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	spec, ok := b.requireManagerGrove(w, r, req.Grove)
	if !ok {
		return
	}
	if err := do(r.Context(), spec); err != nil {
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	phase, perr := b.phaseOf(r.Context(), spec)
	if perr != nil {
		phase = "unknown"
	}
	b.audit("grove", b.manager, "allow", r.URL.Path+" "+spec.Name)
	writeJSON(w, groveResponse{Grove: spec.Name, Phase: phase})
}

func (b *Broker) handleGroveStop(w http.ResponseWriter, r *http.Request) {
	b.groveVerb(w, r, func(ctx context.Context, s GroveSpec) error { return b.runtime.Stop(ctx, s.Name, s.JailProject) })
}
func (b *Broker) handleGroveSuspend(w http.ResponseWriter, r *http.Request) {
	b.groveVerb(w, r, func(ctx context.Context, s GroveSpec) error { return b.runtime.Suspend(ctx, s.Name, s.JailProject) })
}
func (b *Broker) handleGroveResume(w http.ResponseWriter, r *http.Request) {
	b.groveVerb(w, r, func(ctx context.Context, s GroveSpec) error { return b.runtime.Resume(ctx, s.Name, s.JailProject) })
}

func (b *Broker) handleGroveList(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil || caller != b.manager {
		b.audit("grove", caller, "deny", "list: not the manager identity")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Runtime check is after the manager-CN check — authz precedes so an
	// unauthenticated caller still gets 403, not 502.
	if !b.runtimeReady(w) {
		return
	}
	all := []scion.Agent{}
	for _, spec := range b.groves {
		agents, err := b.runtime.List(r.Context(), spec.JailProject)
		if err != nil {
			http.Error(w, "runtime error", http.StatusBadGateway)
			return
		}
		all = append(all, agents...)
	}
	writeJSON(w, struct {
		Agents []scion.Agent `json:"agents"`
	}{Agents: all})
}

// stageBootstrap writes bs to <dir>/bootstrap.json (dir 0700, file 0600).
func stageBootstrap(dir string, bs groveBootstrap) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("stage bootstrap: mkdir: %w", err)
	}
	raw, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("stage bootstrap: marshal: %w", err)
	}
	p := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return fmt.Errorf("stage bootstrap: write: %w", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return fmt.Errorf("stage bootstrap: chmod: %w", err)
	}
	return nil
}

// writeJSON encodes v as JSON to w with Content-Type set.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
