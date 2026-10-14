package config

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stevegeek/lever/internal/imageref"
)

// imageArch is the running lever binary's arch, which is the jail's for a
// native install; a non-native binary (amd64 under Rosetta) would
// mis-resolve. A var so tests are arch-deterministic.
var imageArch = runtime.GOARCH

// archImage gives a tagless ref an arch tag, so one config is portable across
// arches and the two builds never clobber each other under `:latest`. A
// tagged or digest-pinned ref is left alone. The tag is looked for after the
// last `/`, so a registry port is not mistaken for one.
func archImage(ref, arch string) string {
	if ref == "" || strings.Contains(ref, "@") {
		return ref
	}
	name := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	if strings.Contains(name, ":") {
		return ref
	}
	return ref + ":" + arch
}

// ManagerImage returns the manager's container image, arch-resolved.
func (a *App) ManagerImage() string {
	return archImage(a.Manager.Image, imageArch)
}

// WorkerImage returns a worker's container image, arch-resolved, falling
// back to the manager's. Both apply and lever-manager resolve against it.
func (a *App) WorkerImage(g Worker) string {
	if g.Image != "" {
		return archImage(g.Image, imageArch)
	}
	return a.ManagerImage()
}

// ManagerAgentImage is the name scion starts the manager on: the jail's
// podman holds ManagerImage under it (see imageref.Agent).
func (a *App) ManagerAgentImage() string {
	return imageref.Agent(a.ManagerImage())
}

// WorkerAgentImage is WorkerImage under the name scion starts the worker on.
func (a *App) WorkerAgentImage(g Worker) string {
	return imageref.Agent(a.WorkerImage(g))
}

// CheckTarTag refuses a tag in the archive that ships stepRef when it names
// another configured image, under its configured name or the name agents run
// it as: podman load imports every tag in an archive, so the tag would repoint
// that agent's image.
func (a *App) CheckTarTag(stepRef, tag string) error {
	plain := strings.TrimPrefix(strings.TrimPrefix(tag, "docker.io/"), "library/")
	own := imageref.Agent(stepRef)
	refs := []string{a.ManagerImage()}
	for _, g := range a.Workers {
		refs = append(refs, a.WorkerImage(g))
	}
	for _, r := range refs {
		agent := imageref.Agent(r)
		if r == "" || agent == own {
			continue
		}
		if plain == r || tag == r || imageref.Agent(plain) == agent || tag == agent {
			return fmt.Errorf("image %q is configured for another agent (it runs as %s); one archive may not carry it", tag, agent)
		}
	}
	return nil
}

// ImageTagPolicy returns the check applied to every tag an image_tar archive
// carries, or nil with no allowed_image_registries: an archive can hold more
// images than the one the config names, and each of them gets imported.
func (s Security) ImageTagPolicy() func(ref string) error {
	if len(s.AllowedImageRegistries) == 0 {
		return nil
	}
	allowed := strings.Join(s.AllowedImageRegistries, ", ")
	return func(ref string) error {
		if !registryAllowed(ref, s.AllowedImageRegistries) {
			return fmt.Errorf("image %q is not from an allowed registry (allowed: %s)", ref, allowed)
		}
		return nil
	}
}

// ManagerImageTarPath is resolved at the instance root like
// ManagerPromptPath: the archive is the code the agent runs, so an agent in
// the mount must not be able to author it.
func (a *App) ManagerImageTarPath() string {
	if a.Manager.ImageTar == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Manager.ImageTar)
}

// WorkerImageTarPath returns the archive that ships a worker's image; a
// worker that inherits the manager image inherits its tar.
func (a *App) WorkerImageTarPath(g Worker) string {
	if g.Image != "" {
		if g.ImageTar == "" {
			return ""
		}
		return filepath.Join(a.dir, g.ImageTar)
	}
	return a.ManagerImageTarPath()
}
