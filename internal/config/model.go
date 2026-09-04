package config

// ManagerModel returns the LLM model the manager agent should run on, or ""
// when the config names none.
//
// There is deliberately no transformation here (contrast ManagerImage, which
// arch-resolves a tagless ref): the value is scion's own vocabulary — an alias
// (small, medium, large, extra-large/xl) or an explicit model ID — so lever
// passes it through verbatim rather than maintaining its own mapping that would
// go stale against the pinned scion.
//
// Empty is a meaningful answer, not a missing one: it makes the caller omit
// `--model` entirely, leaving scion's own resolution in place (scion#908
// resolves an agent with no configured model to the alias "opus"). lever
// intentionally has no default model of its own — inventing one would silently
// override the installed pin's idea of current, and would need re-vetting on
// every model release.
func (a *App) ManagerModel() string {
	return a.Manager.Model
}

// WorkerModel returns the model a worker should run on: its own `model:` if
// set, else the manager's. Same inheritance as WorkerImage and for the same
// reason — the common case is one model across the instance, and a worker names
// its own only when its task wants a different capability/cost point.
//
// Like the image, this is resolved host-side from validated config (the broker
// bakes the resolved worker specs into its dispatch table when it starts), so
// the manager cannot pick a worker's model when it asks for one to be started.
func (a *App) WorkerModel(g Worker) string {
	if g.Model != "" {
		return g.Model
	}
	return a.ManagerModel()
}
