package brokerctl

import (
	"fmt"
	"path/filepath"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
)

// WorkerSpecs derives the path-authoritative worker descriptions the broker needs
// from config. jailMount is the in-jail mount dest (e.g. /lever). The manager
// never supplies any of these; they are config-authoritative.
func WorkerSpecs(app *config.App, jailMount string) []broker.WorkerSpec {
	specs := make([]broker.WorkerSpec, 0, len(app.Workers))
	for _, g := range app.Workers {
		specs = append(specs, broker.WorkerSpec{
			Name:            g.Name,
			WorkspaceSubdir: g.Dir,                          // e.g. "workers/scratch" — relative to the project root (/lever); scion mounts this subtree at /workspace
			HostWorkspace:   filepath.Join(app.Tree, g.Dir), // <tree>/<dir> — MkdirAll'd before start (scion's guard requires it to exist)
			BootstrapDir:    filepath.Join(app.Tree, g.Dir, ".lever"),
			Image:           app.WorkerImage(g),
			APIKey:          app.EffectiveWorkerLLMAuth(g) == config.LLMAuthAPIKey,
		})
	}
	return specs
}

func workerBrokerURL(host string, port int) string {
	return fmt.Sprintf("https://%s:%d", host, port)
}
