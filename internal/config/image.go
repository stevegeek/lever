package config

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// imageArch is the arch appended to a tagless local image ref (see archImage).
// It defaults to the running lever binary's arch, which — since lever runs
// natively on the host and the jail (OrbStack/Lima) is that same host arch — is
// the arch the jail's podman needs. (A non-native binary, e.g. amd64 under Rosetta
// on an arm64 mac, would mis-resolve; a backend-reported jail arch would be
// stricter but isn't needed for native installs.) A package var so tests are
// arch-deterministic (CI runs amd64; a dev host is often arm64).
var imageArch = runtime.GOARCH

// archImage resolves a container image ref to the arch actually needed in the
// jail: a **tagless** local name (`scionlocal/lever-claude`) gains an arch tag
// (`…:arm64` / `…:amd64`), so one config is portable across an arm64 laptop and
// an amd64 server and the two arch builds never clobber each other under a shared
// `:latest`. An already-tagged (`…:latest`, `…:v3`) or digest-pinned (`…@sha256:…`)
// ref is an explicit choice and is left untouched — that's the escape hatch. The
// tag test is "a `:` in the component after the last `/`", so a registry port
// (`host:5000/img`) is not mistaken for a tag.
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

// ManagerImage returns the manager's container image, arch-resolved (archImage).
func (a *App) ManagerImage() string {
	return archImage(a.Manager.Image, imageArch)
}

// WorkerImage returns the container image a worker should run on: its own
// `image:` if set, else the manager image (the common single-image case, and
// the image apply already loads into the jail). The manager dispatches workers
// later, so this is the single source of truth both apply (what to load) and
// lever-manager (what to pass to `scion start`) resolve against. Arch-resolved
// (archImage), so a tagless name gets the jail's arch tag.
func (a *App) WorkerImage(g Worker) string {
	if g.Image != "" {
		return archImage(g.Image, imageArch)
	}
	return a.ManagerImage()
}

// ImageTagPolicy returns the check apply applies to every tag an image_tar
// archive carries before it is streamed into the jail (jail.LoadImageTar's
// allowTag), or nil when no allowed_image_registries is configured. It is
// the same whole-component prefix rule validateImage applies to the config's
// own image refs — an archive can hold more images than the one the config
// names, and each of them gets imported (R4).
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

// ManagerImageTarPath returns the absolute path of the archive that ships
// the manager image, or "" when the image comes from host docker. Resolved
// at the instance ROOT like ManagerPromptPath, for the same reason: the
// archive is the code the agent runs, so an agent in the mount must not be
// able to author it.
func (a *App) ManagerImageTarPath() string {
	if a.Manager.ImageTar == "" {
		return ""
	}
	return filepath.Join(a.dir, a.Manager.ImageTar)
}

// WorkerImageTarPath returns the archive that ships a worker's image: its
// own `image_tar` when it names its own `image:`, the manager's when it
// inherits the manager image (which is where that image ships), and "" when
// it names an image with no tar (host docker) or no tar exists at all.
func (a *App) WorkerImageTarPath(g Worker) string {
	if g.Image != "" {
		if g.ImageTar == "" {
			return ""
		}
		return filepath.Join(a.dir, g.ImageTar)
	}
	return a.ManagerImageTarPath()
}
