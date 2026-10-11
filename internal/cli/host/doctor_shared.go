package host

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	scionpkg "github.com/stevegeek/lever/internal/scion"
)

// liveMountChecker checks a running container's mounts from the guest
// (jail.ContainerLiveMounts in production).
type liveMountChecker func(ctx context.Context, ref string, want []jail.LiveMount) ([]jail.LiveMountProblem, error)

// sharedAgentPlan is one agent's expected shared mounts, by target.
type sharedAgentPlan struct {
	name string
	want map[string]config.SharedMount
}

// checkSharedFolders compares every agent's container with its
// shared_folders plan (config.SharedMountsFor): each planned folder mounted
// at its /shared/<name> target from the folder in the in-jail tree, and
// read-only unless the agent writes it. A mount beyond the plan (a folder
// dropped, or access withdrawn) fails: the agent keeps that access until
// its record is discarded, and the broker refuses to resume such a worker.
// A planned mount the container lacks warns: the agent was created before
// the folder was added and gets it only when created again. For a running
// container the read-only mounts are also checked from the guest side
// (jail.ContainerLiveMounts: the kernel's mountinfo, and the same directory
// as on the host), never by a program inside the container.
//
// The manager's tree-path protection for a folder it reads is the
// "manager read-only paths" row's (ManagerTreeMounts includes it). An agent
// with no record is not checked; it gets the plan when it is created.
func checkSharedFolders(ctx context.Context, project string, app *config.App, list agentLister, inspect mountInspector, record recordVolumeReader, live liveMountChecker) checkResult {
	const check = "shared folders"
	agents := append([]string{app.Name}, workerNamesOf(app)...)
	if len(app.SharedFolders) == 0 && list == nil {
		return checkResult{check, true, "none configured", ""}
	}
	if list == nil || inspect == nil {
		return checkResult{check, true, "not checked", ""}
	}
	recs, err := list(ctx, project)
	if err != nil {
		if len(app.SharedFolders) == 0 {
			return checkResult{check, true, "none configured", ""}
		}
		return checkResult{check, true, "not checked (could not list agents): " + firstLine(err.Error()), ""}
	}
	var fails, warns, oks []string
	for _, name := range agents {
		plan := sharedAgentPlan{name: name, want: map[string]config.SharedMount{}}
		for _, m := range app.SharedMountsFor(name) {
			plan.want[m.Target] = m
		}
		a := scionpkg.FindAgent(recs, name)
		if a == nil {
			continue
		}
		ref := a.ContainerID
		if ref == "" {
			ref = jail.ContainerName(hubProjectKey(project), name)
		}
		mounts, err := inspect(ctx, ref)
		fromRecord := false
		if err != nil {
			if record == nil {
				if len(plan.want) > 0 {
					warns = append(warns, fmt.Sprintf("%s: not checked (no container to inspect)", name))
				}
				continue
			}
			vols, rerr := record(ctx, hubProjectKey(project), name)
			if rerr != nil {
				if len(plan.want) > 0 {
					warns = append(warns, fmt.Sprintf("%s: not checked (no container, and the record could not be read)", name))
				}
				continue
			}
			mounts, fromRecord = vols, true
		}
		f, w := sharedGaps(project, plan, mounts)
		fails = append(fails, f...)
		warns = append(warns, w...)
		if len(f) > 0 || len(plan.want) == 0 {
			continue
		}
		if fromRecord || !scionpkg.ContainerLive(a.ContainerStatus) || live == nil {
			if len(w) == 0 {
				warns = append(warns, fmt.Sprintf("%s: mounts listed, live check not run (container not running)", name))
			}
			continue
		}
		var want []jail.LiveMount
		for _, m := range plan.want {
			if m.ReadOnly && hasTarget(mounts, m.Target) {
				want = append(want, jail.LiveMount{Target: m.Target, Source: path.Join(project, m.Rel), ReadOnly: true})
			}
		}
		slices.SortFunc(want, func(x, y jail.LiveMount) int { return strings.Compare(x.Target, y.Target) })
		if len(want) > 0 {
			problems, err := live(ctx, ref, want)
			if err != nil {
				warns = append(warns, fmt.Sprintf("%s: live check could not run (%s)", name, firstLine(err.Error())))
				continue
			}
			for _, p := range problems {
				fails = append(fails, fmt.Sprintf("%s: %s %s", name, p.Target, p.Reason))
			}
			if len(problems) > 0 {
				continue
			}
		}
		if len(w) == 0 {
			oks = append(oks, name)
		}
	}
	switch {
	case len(fails) > 0:
		return checkResult{check, false, strings.Join(append(fails, warns...), "; "),
			"discard and recreate each agent named (a worker: `lever worker purge <worker>`, or the manager's `agent recycle` for a recyclable one; the manager: back up its conversation, then `lever up --fresh`); a folder replaced on the host needs the same, and from then on change its contents in place"}
	case len(warns) > 0:
		return warnResult(check, strings.Join(warns, "; "),
			"an agent created before a folder was added gets it only when created again (a worker: purge or recycle; the manager: back up, then `lever up --fresh`); bring stopped agents up and re-run doctor for the live check")
	case len(app.SharedFolders) == 0:
		return checkResult{check, true, "none configured", ""}
	case len(oks) == 0:
		return checkResult{check, true, fmt.Sprintf("%d folder(s) configured; no agent that mounts one has been created yet", len(app.SharedFolders)), ""}
	}
	return checkResult{check, true, fmt.Sprintf("%d folder(s); mounts as configured in %s (read-only mounts checked live)", len(app.SharedFolders), strings.Join(oks, ", ")), ""}
}

// sharedGaps compares one agent's mounts under /shared with its plan.
// fails: a mount beyond the plan, from another source, or read-write where
// the plan says read-only. warns: a planned mount that is missing.
func sharedGaps(project string, plan sharedAgentPlan, mounts []jail.Mount) (fails, warns []string) {
	seen := map[string]bool{}
	for _, m := range mounts {
		t := path.Clean(m.Destination)
		if t != config.SharedMountRoot && !strings.HasPrefix(t, config.SharedMountRoot+"/") {
			continue
		}
		seen[t] = true
		want, ok := plan.want[t]
		switch {
		case !ok:
			mode := "read-only"
			if m.RW {
				mode = "read-write"
			}
			fails = append(fails, fmt.Sprintf("%s: %s mounted %s, which the config no longer grants", plan.name, t, mode))
		case path.Clean(m.Source) != path.Join(project, want.Rel):
			fails = append(fails, fmt.Sprintf("%s: %s mounted from another source than %s", plan.name, t, want.Rel))
		case m.RW && want.ReadOnly:
			fails = append(fails, fmt.Sprintf("%s: %s mounted read-write, but the config makes it read-only", plan.name, t))
		case !m.RW && !want.ReadOnly:
			warns = append(warns, fmt.Sprintf("%s: %s mounted read-only, but the config makes it a writer", plan.name, t))
		}
	}
	targets := make([]string, 0, len(plan.want))
	for t := range plan.want {
		targets = append(targets, t)
	}
	slices.Sort(targets)
	for _, t := range targets {
		if !seen[t] {
			warns = append(warns, fmt.Sprintf("%s: %s not mounted (created before the folder was added)", plan.name, t))
		}
	}
	return fails, warns
}

func hasTarget(mounts []jail.Mount, target string) bool {
	return slices.ContainsFunc(mounts, func(m jail.Mount) bool { return path.Clean(m.Destination) == target })
}

func workerNamesOf(app *config.App) []string {
	out := make([]string, 0, len(app.Workers))
	for _, w := range app.Workers {
		out = append(out, w.Name)
	}
	return out
}
