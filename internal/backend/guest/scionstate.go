package guest

import (
	"context"
	"fmt"
	"strings"

	"github.com/lever-to/lever/internal/backend"
)

// ReadScionProjectState reads scion's project-registration state from the guest
// for `lever doctor`: the in-tree marker (<mountDest>/.scion) and each
// ~/.scion/project-configs registration with the workspace path it claims. It
// runs a read-only script through the machine-only UserPrefix, so it needs no
// run user and works before EnsureUp (only the jail machine must be up).
func (g Guest) ReadScionProjectState(ctx context.Context, mountDest string) (backend.ScionProjectState, error) {
	// Emit a line-parseable report (test/ls/grep only — nothing is mutated):
	//   MARKER 1|0
	//   ENTRY <project-configs-dir> <workspace_path>
	// The space-separated ENTRY format assumes no whitespace in a workspace
	// path; safe because mountDest is the backend constant "/lever" and grove
	// paths are "/lever/groves/<sanitized-name>".
	script := `
if [ -e ` + shellSingleQuote(mountDest+"/.scion") + ` ]; then echo "MARKER 1"; else echo "MARKER 0"; fi
for s in "$HOME"/.scion/project-configs/*/.scion/settings.yaml; do
  [ -e "$s" ] || continue
  d=$(basename "$(dirname "$(dirname "$s")")")
  wp=$(grep -E '^workspace_path:' "$s" 2>/dev/null | head -1 | sed 's/^workspace_path:[[:space:]]*//')
  echo "ENTRY $d $wp"
done
`
	args := append(append([]string{}, g.UserPrefix[1:]...), "bash", "-lc", script)
	res, err := g.Host.Run(ctx, nil, g.UserPrefix[0], args...)
	if err != nil {
		return backend.ScionProjectState{}, fmt.Errorf("guest: read scion project state: %w", err)
	}
	return parseScionState(res.Stdout), nil
}

// RemoveScionProjectConfigs removes every ~/.scion/project-configs/<name>
// registration whose workspace_path == wp, through the machine-only UserPrefix.
// Called before `scion init` in register-* so each apply leaves exactly ONE
// registration per workspace instead of accumulating a duplicate every run. A
// no-op when nothing matches. wp is a lever constant (/lever or
// /lever/groves/<sanitized-name>), never user input.
func (g Guest) RemoveScionProjectConfigs(ctx context.Context, wp string) error {
	script := `
target=` + shellSingleQuote(wp) + `
for s in "$HOME"/.scion/project-configs/*/.scion/settings.yaml; do
  [ -e "$s" ] || continue
  cur=$(grep -E '^workspace_path:' "$s" 2>/dev/null | head -1 | sed 's/^workspace_path:[[:space:]]*//')
  if [ "$cur" = "$target" ]; then rm -rf "$(dirname "$(dirname "$s")")"; fi
done
`
	args := append(append([]string{}, g.UserPrefix[1:]...), "bash", "-lc", script)
	if _, err := g.Host.Run(ctx, nil, g.UserPrefix[0], args...); err != nil {
		return fmt.Errorf("guest: remove scion project configs for %s: %w", wp, err)
	}
	return nil
}

// parseScionState turns the report lines into a ScionProjectState. Unknown or
// malformed lines are ignored (fail-safe: a check reading this treats "no
// entries" as "nothing stale").
func parseScionState(out string) backend.ScionProjectState {
	var st backend.ScionProjectState
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
				st.Entries = append(st.Entries, backend.ScionProjectEntry{Name: f[1], WorkspacePath: f[2]})
			}
		}
	}
	return st
}
