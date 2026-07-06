package brokerctl

import (
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

func TestGroveSpecs(t *testing.T) {
	app := &config.App{
		Tree:    "/host/tree",
		Manager: config.Manager{Image: "mgr:img"},
		// Explicit subscription: api-key is the default post-7d86f73; this makes the helper grove assert APIKey:false.
		Broker: config.Broker{LLMAuth: config.LLMAuthSubscription},
		Groves: []config.Grove{
			{Name: "worker", Dir: "groves/worker", LLMAuth: config.LLMAuthAPIKey},
			{Name: "helper", Dir: "groves/helper", Image: "helper:img"},
		},
	}
	specs := GroveSpecs(app, "/lever")
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	w := specs[0]
	if w.Name != "worker" || w.JailProject != "/lever/groves/worker" ||
		w.BootstrapDir != filepath.Join("/host/tree", "groves/worker", ".lever") ||
		w.Image != "mgr:img" /* inherits manager */ || !w.APIKey {
		t.Fatalf("bad worker spec: %+v", w)
	}
	if specs[1].Image != "helper:img" || specs[1].APIKey {
		t.Fatalf("bad helper spec: %+v", specs[1])
	}
}

func TestGroveBrokerURL(t *testing.T) {
	if got := groveBrokerURL("10.0.0.2", 8080); got != "https://10.0.0.2:8080" {
		t.Fatalf("url = %q", got)
	}
}
