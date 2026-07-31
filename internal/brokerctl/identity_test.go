package brokerctl

import (
	"testing"

	"github.com/stevegeek/lever/internal/config"
)

// ConfigHash must be deterministic, sensitive to broker-relevant config
// (tools, worker specs), and INSENSITIVE to config the broker does not bake
// in at start (a manager-image change must not bounce the broker).
func TestConfigHash(t *testing.T) {
	mk := func() *config.App {
		return &config.App{
			Name: "hello", Backend: "orbstack", Tree: "/tmp/tree",
			Manager: config.Manager{Image: "img"},
			Broker: config.Broker{
				JailPort: 8443, AdminPort: 8444,
				Tools: []config.Tool{{Name: "db", Command: []string{"db-server"}}},
			},
			Workers: []config.Worker{{Name: "scratch", Dir: "workers/scratch"}},
		}
	}

	base := mk()
	if got, again := ConfigHash(base), ConfigHash(mk()); got == "" || got != again {
		t.Fatalf("hash not deterministic: %q vs %q", got, again)
	}

	tool := mk()
	tool.Broker.Tools = append(tool.Broker.Tools, config.Tool{Name: "qmd", Command: []string{"qmd-server"}})
	if ConfigHash(tool) == ConfigHash(base) {
		t.Fatal("adding a tool must change the hash")
	}

	worker := mk()
	worker.Workers[0].Dir = "workers/elsewhere"
	if ConfigHash(worker) == ConfigHash(base) {
		t.Fatal("changing a worker spec must change the hash")
	}

	image := mk()
	image.Manager.Image = "img:v2"
	if ConfigHash(image) != ConfigHash(base) {
		t.Fatal("a manager-image change must NOT change the hash (no broker restart)")
	}
}
