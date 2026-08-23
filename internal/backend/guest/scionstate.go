package guest

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// projectSettingsGlob matches every project-configs registration's settings
// file under the guest run user's home: ~/.scion/project-configs/<name>/.scion/
// settings.yaml. Each script below loops over it; the registration directory
// is two levels up from the match.
const projectSettingsGlob = `"$HOME"/` + layout.ProjectConfigsRel + `/*/` + layout.SettingsRel

// workspacePathOf is the shell fragment that prints a project settings file's
// workspace_path value (the first one, leading whitespace stripped).
const workspacePathOf = `grep -E '^` + layout.WorkspacePathKey + `:' "$s" 2>/dev/null | head -1 | sed 's/^` + layout.WorkspacePathKey + `:[[:space:]]*//'`

// ReadScionProjectState reads scion's project-registration state from the guest
// for `lever doctor`: the in-tree marker (<workspacePath>/.scion) and each
// ~/.scion/project-configs registration with the workspace path it claims. It
// runs a read-only script through the machine-only UserPrefix, so it needs no
// run user and works before EnsureUp (only the jail machine must be up).
func (g Guest) ReadScionProjectState(ctx context.Context, workspacePath string) (types.ScionProjectState, error) {
	// Emit a line-parseable report (test/ls/grep only — nothing is mutated):
	//   MARKER 1|0
	//   ENTRY <project-configs-dir> <workspace_path>
	// The space-separated ENTRY format assumes no whitespace in a workspace
	// path; safe because workspacePath is the backend's mount constant "/lever"
	// and worker paths are "/lever/workers/<sanitized-name>".
	script := `
if [ -e ` + shellSingleQuote(workspacePath+"/"+layout.ProjectMarker) + ` ]; then echo "MARKER 1"; else echo "MARKER 0"; fi
for s in ` + projectSettingsGlob + `; do
  [ -e "$s" ] || continue
  d=$(basename "$(dirname "$(dirname "$s")")")
  wp=$(` + workspacePathOf + `)
  echo "ENTRY $d $wp"
done
`
	res, err := g.UserRun(ctx, "bash", "-lc", script)
	if err != nil {
		return types.ScionProjectState{}, fmt.Errorf("guest: read scion project state: %w", err)
	}
	return parseScionState(res.Stdout), nil
}

// RemoveScionProjectConfigs removes every ~/.scion/project-configs/<name>
// registration whose workspace_path == wp, through the machine-only UserPrefix.
// Called before `scion init` in register-* so each apply leaves exactly ONE
// registration per workspace instead of accumulating a duplicate every run. A
// no-op when nothing matches. wp is a lever constant (/lever or
// /lever/workers/<sanitized-name>), never user input.
func (g Guest) RemoveScionProjectConfigs(ctx context.Context, wp string) error {
	if _, err := g.UserRun(ctx, "bash", "-lc", scionConfigRemoveScript(wp)); err != nil {
		return fmt.Errorf("guest: remove scion project configs for %s: %w", wp, err)
	}
	return nil
}

// scionConfigRemoveScript is the exact bash body RemoveScionProjectConfigs
// runs in the guest (shared with the real-bash test so the deletion logic is
// exercised, not just string-matched). It globs every project-configs entry,
// reads its workspace_path, and rm -rf's the entry dir (two levels up from
// settings.yaml) when it matches wp. Entries without a workspace_path line, or
// with a different one, are left untouched. Idempotent (a spent glob is a
// no-op). wp is single-quoted; it is a lever constant, never user input.
func scionConfigRemoveScript(wp string) string {
	return `
target=` + shellSingleQuote(wp) + `
for s in ` + projectSettingsGlob + `; do
  [ -e "$s" ] || continue
  cur=$(` + workspacePathOf + `)
  if [ "$cur" = "$target" ]; then rm -rf "$(dirname "$(dirname "$s")")"; fi
done
`
}

// ScionProjectRegistered reports whether workspacePath already has EXACTLY ONE
// valid scion registration: precisely one ~/.scion/project-configs entry whose
// workspace_path == workspacePath, AND the in-tree marker
// (workspacePath/.scion) present. Anything else — zero entries, duplicate
// entries, or an entry with the marker gone (the bad-teardown signature) — is
// NOT a valid registration and resolves false, routing the register-project
// apply step (internal/apply/run.go) to its existing
// destructive clean+init path instead of skipping it. Reuses
// ReadScionProjectState's script (same read-only, no-EnsureUp transport; see
// its doc for the VirtioFS/marker rationale) rather than duplicating it — a
// query error is returned unchanged so the caller can fail OPEN to the
// destructive path rather than treating a read failure as "safe to skip".
func (g Guest) ScionProjectRegistered(ctx context.Context, workspacePath string) (bool, error) {
	st, err := g.ReadScionProjectState(ctx, workspacePath)
	if err != nil {
		return false, fmt.Errorf("guest: check scion project registration for %s: %w", workspacePath, err)
	}
	return scionProjectRegistered(st, workspacePath), nil
}

// scionProjectRegistered is the pure exactly-one-valid-registration predicate,
// factored out so it is unit-testable without a fake runner (mirrors how
// internal/cli's checkScionProject is a pure function over the same
// types.ScionProjectState shape, just with the opposite polarity: that one
// flags corruption for `lever doctor`, this one gates a destructive apply
// step).
func scionProjectRegistered(st types.ScionProjectState, workspacePath string) bool {
	n := 0
	for _, e := range st.Entries {
		if e.WorkspacePath == workspacePath {
			n++
		}
	}
	return n == 1 && st.MarkerPresent
}

// parseScionState turns the report lines into a types.ScionProjectState. Unknown or
// malformed lines are ignored (fail-safe: a check reading this treats "no
// entries" as "nothing stale").
func parseScionState(out string) types.ScionProjectState {
	var st types.ScionProjectState
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "MARKER":
			st.MarkerPresent = len(f) >= 2 && f[1] == "1"
		case "ENTRY":
			if len(f) >= 3 {
				st.Entries = append(st.Entries, types.ScionProjectEntry{Name: f[1], WorkspacePath: f[2]})
			}
		}
	}
	return st
}

// RepairScionHubEndpoint rewrites the hub endpoint in every
// ~/.scion/project-configs registration whose workspace_path == wp, when it
// differs from endpoint. A no-op when nothing matches or every entry is already
// correct; idempotent.
//
// It exists because minting the controller PAT runs a THROWAWAY dev-auth hub on
// its own port and `hub link`s the project against it, which persists that
// throwaway endpoint into the project config. The register-project step's
// re-init would overwrite it, but that path is deliberately skipped when the
// registration is already sound — so a re-mint on an established instance
// leaves the project pointing at a hub that no longer exists.
//
// Only lever's own calls pass an explicit endpoint, so the damage surfaces
// wherever scion runs bare in the jail. `lever attach` was the live case
// (2026-08-11): it exec'd scion, read this file, and dialled the dead port.
//
// wp and endpoint are lever constants, never user input; both are single-quoted.
func (g Guest) RepairScionHubEndpoint(ctx context.Context, wp, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	if _, err := g.UserRun(ctx, "bash", "-lc", scionHubEndpointRepairScript(wp, endpoint)); err != nil {
		return fmt.Errorf("guest: repair scion hub endpoint for %s: %w", wp, err)
	}
	return nil
}

// scionHubEndpointRepairScript is the exact bash body RepairScionHubEndpoint
// runs (shared with the real-bash test so the rewrite is exercised, not just
// string-matched). It matches the endpoint line under the `hub:` block — the
// only `endpoint:` key scion writes into a project settings.yaml — and rewrites
// it in place, preserving the original indentation.
func scionHubEndpointRepairScript(wp, endpoint string) string {
	return `
target=` + shellSingleQuote(wp) + `
want=` + shellSingleQuote(endpoint) + `
for s in ` + projectSettingsGlob + `; do
  [ -e "$s" ] || continue
  cur=$(` + workspacePathOf + `)
  [ "$cur" = "$target" ] || continue
  have=$(grep -E '^[[:space:]]*endpoint:[[:space:]]*' "$s" 2>/dev/null | head -1 | sed 's/^[[:space:]]*endpoint:[[:space:]]*//')
  [ -n "$have" ] || continue
  [ "$have" = "$want" ] && continue
  tmp="$s.lever-repair"
  sed "s|^\([[:space:]]*\)endpoint:[[:space:]]*.*$|\1endpoint: $want|" "$s" > "$tmp" && mv "$tmp" "$s"
  echo "REPAIRED $s $have -> $want"
done
`
}
