package apply

import (
	"context"
	"fmt"
	"os"
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
		if err := d.Scion.InitProject(ctx, s.Target); err != nil {
			return err
		}
		return d.Scion.HubLink(ctx, s.Target)
	case "start-manager":
		return d.Scion.Start(ctx, scion.StartOpts{
			Grove: app.Name, Project: app.Tree, Image: app.Manager.Image, Harness: "claude",
		})
	default:
		return fmt.Errorf("unknown step kind %q", s.Kind)
	}
}

func defaultReadCred(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
