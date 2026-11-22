package brokerctl

import (
	"fmt"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
)

// WorkerSpecs derives the config-authoritative worker descriptions the broker
// needs; the manager never supplies any of them. jailUID places each
// worker's ticket directory under the run user's runtime dir; "" leaves it
// empty and the worker undispatchable (fail closed with no jail wired).
func WorkerSpecs(app *config.App, jailMount, jailUID string) []broker.WorkerSpec {
	specs := make([]broker.WorkerSpec, 0, len(app.Workers))
	for _, g := range app.Workers {
		specs = append(specs, workerSpecOf(app, jailMount, jailUID, g, true))
	}
	return specs
}

func workerBrokerURL(host string, port int) string {
	return fmt.Sprintf("https://%s:%d", host, port)
}

// sharedMounts uses the same function as the manager's
// (apply.SharedVolumes).
func sharedMounts(app *config.App, jailMount, worker string) []broker.SharedMount {
	plan := app.SharedMountsFor(worker)
	if len(plan) == 0 {
		return nil
	}
	vols := apply.SharedVolumes(jailMount, plan)
	out := make([]broker.SharedMount, len(plan))
	for i, m := range plan {
		out[i] = broker.SharedMount{Rel: m.Rel, Volume: vols[i]}
	}
	return out
}
