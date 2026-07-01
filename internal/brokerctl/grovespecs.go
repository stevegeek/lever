package brokerctl

import (
	"fmt"
	"path/filepath"

	"github.com/lever-to/lever/internal/broker"
	"github.com/lever-to/lever/internal/config"
)

// GroveSpecs derives the path-authoritative grove descriptions the broker needs
// from config. jailMount is the in-jail mount dest (e.g. /lever). The manager
// never supplies any of these; they are config-authoritative.
func GroveSpecs(app *config.App, jailMount string) []broker.GroveSpec {
	specs := make([]broker.GroveSpec, 0, len(app.Groves))
	for _, g := range app.Groves {
		specs = append(specs, broker.GroveSpec{
			Name:         g.Name,
			JailProject:  filepath.Join(jailMount, g.Dir),
			BootstrapDir: filepath.Join(app.Tree, g.Dir, ".lever"),
			Image:        app.GroveImage(g),
			APIKey:       app.EffectiveGroveLLMAuth(g) == config.LLMAuthAPIKey,
		})
	}
	return specs
}

func groveBrokerURL(host string, port int) string {
	return fmt.Sprintf("https://%s:%d", host, port)
}
