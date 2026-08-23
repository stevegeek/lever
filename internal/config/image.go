package config

import (
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
