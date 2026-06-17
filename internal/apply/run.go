package apply

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
)

// Deps are the executor's collaborators, injected so Run is testable offline.
// JailUp/LoadImage are host-side (backend.EnsureUp, docker-save|podman-load);
// Scion runs IN the jail (built on a JailRunner).
type Deps struct {
	JailUp    func(ctx context.Context, app *config.App) error
	LoadImage func(ctx context.Context, imageRef string) error
	Scion     *scion.Client
	ReadCred  func(path string) (string, error) // nil ⇒ defaultReadCred
	JailMount string                            // jail path where app.Tree is bind-mounted (e.g. "/lever"); "" disables translation
}

// Run executes the bring-up Plan for app. jail-up/load-image are host-side; the
// rest run in the jail via Deps.Scion.
func Run(ctx context.Context, app *config.App, d Deps) error {
	for _, step := range Plan(app) {
		if err := runStep(ctx, app, step, d); err != nil {
			return fmt.Errorf("step %s: %w", step.Kind, err)
		}
	}
	return nil
}

func runStep(ctx context.Context, app *config.App, s Step, d Deps) error {
	switch s.Kind {
	case "jail-up":
		return d.JailUp(ctx, app)
	case "load-image":
		return d.LoadImage(ctx, s.Target)
	case "init-machine":
		return d.Scion.InitMachine(ctx)
	case "config-registry":
		return d.Scion.ConfigSetGlobal(ctx, "image_registry", "scionlocal")
	case "scion-server":
		return d.Scion.ServerStart(ctx)
	case "credential":
		read := d.ReadCred
		if read == nil {
			read = defaultReadCred
		}
		tok, err := read(s.Target)
		if err != nil {
			return fmt.Errorf("reading credential %s: %w", s.Target, err)
		}
		return d.Scion.SecretSet(ctx, "CLAUDE_CODE_OAUTH_TOKEN", tok)
	case "register-manager", "register-grove":
		// Remove a stale `.scion` marker FILE left in the tree by a previous
		// bring-up. It survives `orb delete` (it lives in the bind-mounted tree),
		// and `scion init` writes workspace_path only on fresh-create — resolving
		// a stale marker skips it, so the agent mounts an empty managed config-dir
		// copy instead of the live tree (the in-place mount silently breaks).
		// Removing it forces a fresh, correct init. s.Target is the host path and
		// the tree is bind-mounted, so a host-side remove reaches the jail.
		if err := removeStaleMarker(s.Target); err != nil {
			return err
		}
		jp := jailPath(s.Target, app.Tree, d.JailMount)
		if err := d.Scion.InitProject(ctx, jp); err != nil {
			return err
		}
		return d.Scion.HubLink(ctx, jp)
	case "start-manager":
		task := ""
		if p := app.ManagerPromptPath(); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("reading manager prompt %s: %w", p, err)
			}
			task = strings.TrimSpace(string(b))
		}
		jp := jailPath(app.Tree, app.Tree, d.JailMount)
		return d.Scion.Start(ctx, scion.StartOpts{
			Grove: app.Name, Task: task, Project: jp, Image: app.Manager.Image, Harness: "claude",
			// Workspace = the in-jail project tree, so the manager edits the real
			// host files in place (verified 2026-06-16). Without it scion mounts a
			// managed copy of the externalized config dir, not the live tree.
			Workspace: jp,
		})
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
}

// removeStaleMarker removes a `.scion` MARKER FILE at dir (left by a prior
// bring-up; it persists in the bind-mounted tree across jail teardown). It
// leaves a `.scion` DIRECTORY untouched — that's an in-repo git-mode project,
// not a stale directory marker. Absent `.scion` is a no-op.
func removeStaleMarker(dir string) error {
	p := filepath.Join(dir, ".scion")
	info, err := os.Lstat(p)
	if err != nil {
		return nil // nothing there (or unreadable) — fine
	}
	if info.IsDir() {
		return nil // in-repo project marker dir — leave it
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("removing stale .scion marker %s: %w", p, err)
	}
	return nil
}

// jailPath maps a host path under tree to its location inside the jail (mount + suffix).
// Returns hostPath unchanged when mount=="" or hostPath is not under tree.
func jailPath(hostPath, tree, mount string) string {
	if mount == "" || tree == "" {
		return hostPath
	}
	rel, err := filepath.Rel(tree, hostPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return hostPath
	}
	if rel == "." {
		return mount
	}
	return path.Join(mount, filepath.ToSlash(rel))
}

func defaultReadCred(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
