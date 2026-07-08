package brokerctl

import (
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

func TestWorkerSpecs(t *testing.T) {
	app := &config.App{
		Tree:    "/host/tree",
		Manager: config.Manager{Image: "mgr:img"},
		// Explicit subscription: api-key is the default post-7d86f73; this makes the helper worker assert APIKey:false.
		Broker: config.Broker{LLMAuth: config.LLMAuthSubscription},
		Workers: []config.Worker{
			{Name: "worker", Dir: "workers/worker", LLMAuth: config.LLMAuthAPIKey},
			{Name: "helper", Dir: "workers/helper", Image: "helper:img"},
		},
	}
	specs := WorkerSpecs(app, "/lever")
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	w := specs[0]
	if w.Name != "worker" || w.Workspace != "/lever/workers/worker" ||
		w.HostWorkspace != filepath.Join("/host/tree", "workers/worker") ||
		w.BootstrapDir != filepath.Join("/host/tree", "workers/worker", ".lever") ||
		w.Image != "mgr:img" /* inherits manager */ || !w.APIKey {
		t.Fatalf("bad worker spec: %+v", w)
	}
	if specs[1].Image != "helper:img" || specs[1].APIKey {
		t.Fatalf("bad helper spec: %+v", specs[1])
	}
}

func TestWorkerBrokerURL(t *testing.T) {
	if got := workerBrokerURL("10.0.0.2", 8080); got != "https://10.0.0.2:8080" {
		t.Fatalf("url = %q", got)
	}
}
