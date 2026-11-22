package host

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
)

// liveSpawned is the spawned agents the registry holds that are not
// destroyed: lever drives them, so their hub records are no orphans.
func liveSpawned(st broker.AgentsState) []string {
	var out []string
	for _, r := range st.Agents {
		if r.State != broker.AgentDestroyed && broker.IsAgentName(r.Name) {
			out = append(out, r.Name)
		}
	}
	return out
}

// checkSpawnedAgents reports the registry of spawned agents: a spawn or
// destroy a restart left unfinished, and agents whose template is gone.
func checkSpawnedAgents(app *config.App, st broker.AgentsState, loadErr error) checkResult {
	const check = "spawned agents"
	if loadErr != nil {
		return checkResult{check, false, "the registry cannot be read: " + firstLine(loadErr.Error()),
			"the broker refuses to start without it; restore agents.json from a backup of the state directory"}
	}
	var live, unsettled, orphaned []string
	for _, r := range st.Agents {
		if r.State == broker.AgentDestroyed || !broker.IsAgentName(r.Name) {
			continue
		}
		live = append(live, r.Name)
		switch {
		case r.State == broker.AgentSpawning || r.State == broker.AgentDestroying:
			unsettled = append(unsettled, fmt.Sprintf("%s (%s)", r.Name, r.State))
		default:
			if _, ok := app.TemplateByName(r.Template); !ok {
				orphaned = append(orphaned, r.Name)
			}
		}
	}
	if len(live) == 0 {
		return checkResult{check, true, fmt.Sprintf("none (%d template(s))", len(app.Templates)), ""}
	}
	slices.Sort(unsettled)
	slices.Sort(orphaned)
	detail := fmt.Sprintf("%d spawned agent(s) not destroyed", len(live))
	var fixes []string
	if len(unsettled) > 0 {
		detail += "; unfinished after a restart: " + strings.Join(unsettled, ", ")
		fixes = append(fixes, "the broker settles these at start; if one stays, run `lever agent destroy NAME --force`")
	}
	if len(orphaned) > 0 {
		detail += "; template no longer configured: " + strings.Join(orphaned, ", ")
		fixes = append(fixes, "they cannot be started or resumed: destroy each with `lever agent destroy NAME --force`, or declare the template again")
	}
	return checkResult{check, true, detail, strings.Join(fixes, "; ")}
}
