package brokerctl

import (
	"fmt"
	"path/filepath"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
)

// WorkerSpecs derives the path-authoritative worker descriptions the broker needs
// from config. jailMount is the in-jail mount dest (e.g. /lever); jailUID the
// run user's uid in the jail, which places each worker's ticket directory
// under that user's runtime dir (jail.WorkerTicketDir) — "" leaves TicketDir
// empty and the worker undispatchable, which is the fail-closed shape for a
// broker with no jail wired. The manager never supplies any of these; they
// are config-authoritative.
func WorkerSpecs(app *config.App, jailMount, jailUID string) []broker.WorkerSpec {
	specs := make([]broker.WorkerSpec, 0, len(app.Workers))
	for _, g := range app.Workers {
		ticketDir := ""
		if jailUID != "" {
			ticketDir = jail.WorkerTicketDir(jailUID, g.Name)
		}
		specs = append(specs, broker.WorkerSpec{
			Name:            g.Name,
			WorkspaceSubdir: g.Dir,                          // e.g. "workers/scratch" — relative to the project root (/lever); scion mounts this subtree at /workspace
			HostWorkspace:   filepath.Join(app.Tree, g.Dir), // <tree>/<dir> — MkdirAll'd before start (scion's guard requires it to exist)
			TicketDir:       ticketDir,
			Image:           app.WorkerImage(g),
			Model:           app.WorkerModel(g), // own model:, else the manager's; empty ⇒ scion decides
			// Own instructions_file only — never the manager's (see
			// config.WorkerInstructionsPath). Content is read at dispatch.
			InstructionsPath: app.WorkerInstructionsPath(g),
			APIKey:           app.EffectiveWorkerLLMAuth(g) == config.LLMAuthAPIKey,
		})
	}
	return specs
}

func workerBrokerURL(host string, port int) string {
	return fmt.Sprintf("https://%s:%d", host, port)
}
