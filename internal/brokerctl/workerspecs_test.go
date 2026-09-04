package brokerctl

import (
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

func TestWorkerSpecs(t *testing.T) {
	app := &config.App{
		Tree:    "/host/tree",
		Manager: config.Manager{Image: "mgr:img", Model: "mgr-model"},
		// Explicit subscription: api-key is the default post-7d86f73; this makes the helper worker assert APIKey:false.
		Broker: config.Broker{LLMAuth: config.LLMAuthSubscription},
		Workers: []config.Worker{
			{Name: "worker", Dir: "workers/worker", LLMAuth: config.LLMAuthAPIKey},
			{Name: "helper", Dir: "workers/helper", Image: "helper:img", Model: "helper-model", InstructionsFile: "helper-manual.md"},
		},
	}
	specs := WorkerSpecs(app, "/lever")
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	w := specs[0]
	if w.Name != "worker" || w.WorkspaceSubdir != "workers/worker" ||
		w.HostWorkspace != filepath.Join("/host/tree", "workers/worker") ||
		w.BootstrapDir != filepath.Join("/host/tree", "workers/worker", ".lever") ||
		w.Image != "mgr:img" /* inherits manager */ || w.Model != "mgr-model" /* ditto */ || !w.APIKey {
		t.Fatalf("bad worker spec: %+v", w)
	}
	if specs[1].Image != "helper:img" || specs[1].Model != "helper-model" || specs[1].APIKey {
		t.Fatalf("bad helper spec: %+v", specs[1])
	}
	// instructions_file is the worker's OWN or nothing — never inherited from
	// the manager — and the spec carries the config-resolved host path.
	if specs[1].InstructionsPath != app.WorkerInstructionsPath(app.Workers[1]) || specs[1].InstructionsPath == "" {
		t.Fatalf("helper InstructionsPath = %q, want the config-resolved path", specs[1].InstructionsPath)
	}
	if w.InstructionsPath != "" {
		t.Fatalf("worker without instructions_file must get none, got %q", w.InstructionsPath)
	}
}

func TestWorkerBrokerURL(t *testing.T) {
	if got := workerBrokerURL("10.0.0.2", 8080); got != "https://10.0.0.2:8080" {
		t.Fatalf("url = %q", got)
	}
}
