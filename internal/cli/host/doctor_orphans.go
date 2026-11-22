package host

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/broker"
	scionpkg "github.com/stevegeek/lever/internal/scion"
)

// checkOrphanRecords warns about the project's hub records that are neither
// the manager nor a declared worker: no lever verb drives them, `lever stop`
// leaves them running, and a running one counts against
// broker.max_running_workers. A worker removed from the config leaves one.
func checkOrphanRecords(ctx context.Context, project, manager string, workers []string, list agentLister) checkResult {
	const check = "orphan agent records"
	if list == nil {
		return checkResult{check, true, "not checked", ""}
	}
	agents, err := list(ctx, project)
	if err != nil {
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var named []string
	unnamed := 0
	for _, a := range agents {
		if a.Slug == manager || slices.Contains(workers, a.Slug) {
			continue
		}
		if !broker.IsAgentName(a.Slug) {
			unnamed++
			continue
		}
		named = append(named, fmt.Sprintf("%s (%s)", a.Slug, scionpkg.PhaseLabel(a.Phase)))
	}
	if len(named) == 0 && unnamed == 0 {
		return checkResult{check, true, "none: every hub record is the manager or a declared worker", ""}
	}
	slices.Sort(named)
	detail := "hub record(s) that are neither the manager nor a declared worker: " + strings.Join(named, ", ")
	if unnamed > 0 {
		if len(named) == 0 {
			detail = "hub record(s) that are neither the manager nor a declared worker:"
		}
		detail += fmt.Sprintf(" (and %d whose name is not an agent name)", unnamed)
	}
	detail += "; lever does not drive them, and a running one counts against broker.max_running_workers"
	return checkResult{check, true, detail,
		"delete each with `lever worker purge NAME --orphan --force` (the broker must be running), or declare it again under `workers:` and run `lever reload`"}
}
