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
	args := append(append([]string{}, g.UserPrefix[1:]...), "bash", "-lc", scionConfigRemoveScript(wp))
	if _, err := g.Host.Run(ctx, nil, g.UserPrefix[0], args...); err != nil {
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
for s in "$HOME"/.scion/project-configs/*/.scion/settings.yaml; do
  [ -e "$s" ] || continue
  cur=$(grep -E '^workspace_path:' "$s" 2>/dev/null | head -1 | sed 's/^workspace_path:[[:space:]]*//')
  if [ "$cur" = "$target" ]; then rm -rf "$(dirname "$(dirname "$s")")"; fi
done
`
}

// ScionProjectRegistered reports whether workspacePath already has EXACTLY ONE
// valid scion registration: precisely one ~/.scion/project-configs entry whose
// workspace_path == workspacePath, AND the in-tree marker
// (workspacePath/.scion) present. Anything else — zero entries, duplicate
// entries, or an entry with the marker gone (the bad-teardown signature) — is
// NOT a valid registration and resolves false, routing the register-manager/
// register-grove apply step (internal/apply/run.go) to its existing
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
// backend.ScionProjectState shape, just with the opposite polarity: that one
// flags corruption for `lever doctor`, this one gates a destructive apply
// step).
func scionProjectRegistered(st backend.ScionProjectState, workspacePath string) bool {
	n := 0
	for _, e := range st.Entries {
		if e.WorkspacePath == workspacePath {
			n++
		}
	}
	return n == 1 && st.MarkerPresent
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
