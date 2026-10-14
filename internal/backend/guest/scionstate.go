package guest

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/scion/layout"
)

// projectSettingsGlob matches every project-configs registration's settings
// file; the registration directory is two levels up from the match.
const projectSettingsGlob = `"$HOME"/` + layout.ProjectConfigsRel + `/*/` + layout.SettingsRel

const workspacePathOf = `grep -E '^` + layout.WorkspacePathKey + `:' "$s" 2>/dev/null | head -1 | sed 's/^` + layout.WorkspacePathKey + `:[[:space:]]*//'`

// ReadScionProjectState reads scion's project-registration state from the
// guest. Read-only, through the machine-only UserPrefix, so it works before
// EnsureUp.
func (g Guest) ReadScionProjectState(ctx context.Context, workspacePath string) (types.ScionProjectState, error) {
	// The space-separated ENTRY format assumes no whitespace in a workspace
	// path; workspace paths are lever constants under /lever.
	script := markerStateScript(workspacePath) + `
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

// markerStateScript prints "MARKER 1" only for a regular file. A symlink an
// agent put in the tree is no marker: reading it as one would skip the re-init
// that removes it, and scion writes through it on every agent start.
func markerStateScript(workspacePath string) string {
	m := shellSingleQuote(workspacePath + "/" + layout.ProjectMarker)
	return `if [ -f ` + m + ` ] && [ ! -L ` + m + ` ]; then echo "MARKER 1"; else echo "MARKER 0"; fi`
}

// RemoveScionProjectConfigs removes every ~/.scion/project-configs/<name>
// registration whose workspace_path == wp, through the machine-only
// UserPrefix. wp is a lever constant, never user input.
func (g Guest) RemoveScionProjectConfigs(ctx context.Context, wp string) error {
	if _, err := g.UserRun(ctx, "bash", "-lc", scionConfigRemoveScript(wp)); err != nil {
		return fmt.Errorf("guest: remove scion project configs for %s: %w", wp, err)
	}
	return nil
}

// scionConfigRemoveScript is shared with the real-bash test so the deletion
// logic is exercised, not just string-matched.
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
// valid scion registration: one project-configs entry for it AND the in-tree
// marker present. A query error is returned so the caller can fail OPEN to
// the destructive clean+init path rather than treat it as "safe to skip".
func (g Guest) ScionProjectRegistered(ctx context.Context, workspacePath string) (bool, error) {
	st, err := g.ReadScionProjectState(ctx, workspacePath)
	if err != nil {
		return false, fmt.Errorf("guest: check scion project registration for %s: %w", workspacePath, err)
	}
	return scionProjectRegistered(st, workspacePath), nil
}

func scionProjectRegistered(st types.ScionProjectState, workspacePath string) bool {
	n := 0
	for _, e := range st.Entries {
		if e.WorkspacePath == workspacePath {
			n++
		}
	}
	return n == 1 && st.MarkerPresent
}

// parseScionState ignores unknown or malformed lines.
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

// RepairScionHubEndpoint rewrites the hub endpoint in every project-configs
// registration whose workspace_path == wp, when it differs from endpoint.
// Idempotent. See backend.Backend for why it can be stale. wp and endpoint
// are lever constants, never user input.
func (g Guest) RepairScionHubEndpoint(ctx context.Context, wp, endpoint string) error {
	if endpoint == "" {
		return nil
	}
	if _, err := g.UserRun(ctx, "bash", "-lc", scionHubEndpointRepairScript(wp, endpoint)); err != nil {
		return fmt.Errorf("guest: repair scion hub endpoint for %s: %w", wp, err)
	}
	return nil
}

// scionHubEndpointRepairScript relies on the `hub:` block's endpoint being
// the only `endpoint:` key scion writes into a project settings.yaml.
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
