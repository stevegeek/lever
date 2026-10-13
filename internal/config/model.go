package config

// ManagerModel returns the manager's model, passed through verbatim: the
// value is scion's own vocabulary, and a lever mapping would go stale
// against the pinned scion. Empty makes the caller omit `--model` and leave
// the choice to scion; lever has no default model of its own.
func (a *App) ManagerModel() string {
	return a.Manager.Model
}

// WorkerModel returns a worker's model, falling back to the manager's. It is
// resolved host-side from config, so the manager cannot pick a worker's model
// when it asks for one to be started.
func (a *App) WorkerModel(g Worker) string {
	if g.Model != "" {
		return g.Model
	}
	return a.ManagerModel()
}
