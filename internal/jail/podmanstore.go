package jail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/termsafe"
)

// StoreHealth is what `lever doctor` reads about the jail's rootless podman
// store (lever#173): how many layers no image and no container uses, how many
// layers carry containers/storage's "incomplete" flag, and any warning podman
// itself prints about corruption.
//
// Why orphan layers matter: when podman loads or pulls an image it looks for
// an existing layer with the same content to reuse, and it searches every
// layer in the store, used or not. An orphan layer left damaged by an
// interrupted write (its overlay link gone) is then picked, and the load
// fails with "creating file-getter: readlink .../overlay/l/: no such file or
// directory". `podman image prune` removes dangling images, never layers no
// image owns, and podman before 5.x has no command that removes them.
type StoreHealth struct {
	Version    string // podman version, e.g. "4.9.3"
	Driver     string // storage driver, e.g. "overlay"
	Images     int
	Containers int
	Layers     int
	// Orphans counts layers reachable from no image (its top layer or an
	// id-mapped copy of it) and no container, walking parent links.
	Orphans     int
	OrphanBytes int64 // their uncompressed size, as the store records it
	Incomplete  int   // layers flagged incomplete (a write that never finished)
	// Warnings are podman's own stderr lines that say the store is corrupted
	// ("storage corruption", "may be corrupted"), sanitized and clipped, at
	// most storeWarningsMax. They fail the doctor row.
	Warnings []string
	// Notices are podman stderr lines that hint at damage but also appear in
	// stores that work ("layer not known", a bare "corrupt"). They warn.
	Notices []string
}

// MajorVersion returns the leading number of Version, or 0 when it has none.
func (h StoreHealth) MajorVersion() int {
	n := 0
	for _, c := range h.Version {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

const (
	// storeReadTimeout bounds the whole read; each guest command also runs
	// under `timeout storeCmdSeconds`, so a hung podman cannot outlive it.
	storeReadTimeout = 60 * time.Second
	storeCmdSeconds  = "20"
	// storeOutputLimit caps each stream of each command (proc.WithOutputLimit).
	// layers.json takes about 1 KB a layer, so this is thousands of layers.
	storeOutputLimit = 16 << 20
	storeWarningsMax = 3
)

// driverName is the shape of a containers/storage driver name; the store's
// metadata directories are named after it.
var driverName = regexp.MustCompile(`^[a-z0-9]+$`)

// podmanInfo is the part of `podman info --format json` the read needs.
type podmanInfo struct {
	Store struct {
		GraphRoot       string `json:"graphRoot"`
		GraphDriverName string `json:"graphDriverName"`
	} `json:"store"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

// The records of containers/storage's metadata files that the walk needs:
// <graphroot>/<driver>-layers/layers.json, -images/images.json and
// -containers/containers.json. These files are internal to containers/storage,
// but stable for years and written atomically (a rename), so a reader sees a
// whole file; podman before 5.x has no command that lists layers, so they are
// the only read-only way to count orphans there.
type storeLayer struct {
	ID       string         `json:"id"`
	Parent   string         `json:"parent"`
	Flags    map[string]any `json:"flags"`
	DiffSize int64          `json:"diff-size"`
}

type storeImage struct {
	ID           string   `json:"id"`
	Layer        string   `json:"layer"`
	MappedLayers []string `json:"mapped-layers"`
}

type storeContainer struct {
	ID    string `json:"id"`
	Layer string `json:"layer"`
}

// storeFileScript prints the file named by $1, or exits 3 when there is none
// (a store with no containers yet has no containers.json). Read-only.
const storeFileScript = `test -e "$1" || exit 3; exec timeout ` + storeCmdSeconds + ` cat -- "$1"`

// ReadStoreHealth reads the health of the jail's rootless podman store. r is
// the in-jail runner (Backend.JailRunner), so every command runs as the jail
// run user with its XDG_RUNTIME_DIR. lever changes nothing (podman's own
// startup cleanup may still run inside podman info): `podman
// info`, `podman images -a -q` (for podman's own warnings) and a cat of the
// three metadata files. Nothing is changed and no layer is ever removed.
func ReadStoreHealth(ctx context.Context, r proc.Runner) (StoreHealth, error) {
	ctx, cancel := context.WithTimeout(proc.WithOutputLimit(ctx, storeOutputLimit), storeReadTimeout)
	defer cancel()
	var h StoreHealth

	res, err := r.Run(ctx, nil, "timeout", storeCmdSeconds, "podman", "info", "--format", "json")
	if err != nil {
		return h, fmt.Errorf("podman info: %w: %s", err, podmanErrTail(res.Stderr))
	}
	h.Warnings, h.Notices = appendCorruptionWarnings(h.Warnings, h.Notices, res.Stderr)
	var info podmanInfo
	if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil {
		return h, fmt.Errorf("podman info: unreadable output: %w", err)
	}
	h.Version = termsafe.Sanitize(strings.TrimSpace(info.Version.Version))
	root, driver := info.Store.GraphRoot, info.Store.GraphDriverName
	if !path.IsAbs(root) || path.Clean(root) != root || strings.ContainsFunc(root, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return h, fmt.Errorf("podman info: unexpected graph root %q", root)
	}
	if !driverName.MatchString(driver) {
		return h, fmt.Errorf("podman info: unexpected storage driver %q", driver)
	}
	h.Driver = driver

	// podman's own listing, for the warnings it prints about images whose
	// layers are damaged; the IDs themselves are not needed, and a failed
	// listing is judged by its stderr like a good one.
	res, _ = r.Run(ctx, nil, "timeout", storeCmdSeconds, "podman", "images", "-a", "-q")
	h.Warnings, h.Notices = appendCorruptionWarnings(h.Warnings, h.Notices, res.Stderr)

	var layers []storeLayer
	var images []storeImage
	var containers []storeContainer
	for _, f := range []struct {
		rel string
		v   any
	}{
		{driver + "-layers/layers.json", &layers},
		{driver + "-images/images.json", &images},
		{driver + "-containers/containers.json", &containers},
	} {
		if err := readStoreFile(ctx, r, path.Join(root, f.rel), f.v); err != nil {
			return h, err
		}
	}
	h.Images, h.Containers, h.Layers = len(images), len(containers), len(layers)
	h.Orphans, h.OrphanBytes, h.Incomplete = orphanLayers(layers, images, containers)
	return h, nil
}

// readStoreFile reads one metadata file into v. A missing file is an empty
// list; an empty file is too (containers/storage writes "[]", but a fresh
// store may hold a zero-length file).
func readStoreFile(ctx context.Context, r proc.Runner, file string, v any) error {
	res, err := r.Run(ctx, nil, "sh", "-c", storeFileScript, "_", file)
	if err != nil {
		if res.Code == 3 {
			return nil
		}
		if errors.Is(err, proc.ErrOutputLimit) {
			return fmt.Errorf("reading %s: larger than %d MiB", path.Base(file), storeOutputLimit>>20)
		}
		return fmt.Errorf("reading %s: %w: %s", path.Base(file), err, podmanErrTail(res.Stderr))
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(res.Stdout), v); err != nil {
		return fmt.Errorf("reading %s: %w", path.Base(file), err)
	}
	return nil
}

// orphanLayers counts the layers no image and no container reaches. The
// roots are every image's top layer and its id-mapped copies (a rootless run
// under another user-namespace mapping makes one) and every container's own
// layer; each root's parent chain is walked to the base. A layer the walk
// does not reach is an orphan. It also counts layers flagged incomplete.
func orphanLayers(layers []storeLayer, images []storeImage, containers []storeContainer) (orphans int, orphanBytes int64, incomplete int) {
	parent := make(map[string]string, len(layers))
	for _, l := range layers {
		parent[l.ID] = l.Parent
	}
	used := make(map[string]bool, len(layers))
	mark := func(id string) {
		// The bound guards a parent cycle in a damaged file.
		for n := 0; id != "" && !used[id] && n <= len(layers); n++ {
			used[id] = true
			id = parent[id]
		}
	}
	for _, img := range images {
		mark(img.Layer)
		for _, m := range img.MappedLayers {
			mark(m)
		}
	}
	for _, c := range containers {
		mark(c.Layer)
	}
	for _, l := range layers {
		if v, ok := l.Flags["incomplete"]; ok && v != false {
			incomplete++
		}
		if !used[l.ID] {
			orphans++
			orphanBytes += max(l.DiffSize, 0)
		}
	}
	return orphans, orphanBytes, incomplete
}

// appendCorruptionWarnings sorts podman's stderr lines that name damage
// into ws (podman says the store is corrupted: they fail the doctor row) and
// ns (hints such as "layer not known" that also appear in working stores:
// they warn), sanitized, clipped and de-duplicated, up to storeWarningsMax
// each.
func appendCorruptionWarnings(ws, ns []string, stderr string) ([]string, []string) {
	add := func(list []string, l string) []string {
		if len(list) >= storeWarningsMax {
			return list
		}
		l = clipMiddle(strings.TrimSpace(termsafe.Sanitize(l)), 300)
		for _, w := range list {
			if w == l {
				return list
			}
		}
		return append(list, l)
	}
	for _, l := range strings.FieldsFunc(stderr, func(r rune) bool { return r == '\n' || r == '\r' }) {
		low := strings.ToLower(l)
		switch {
		case strings.Contains(low, "storage corruption") || strings.Contains(low, "may be corrupted"):
			ws = add(ws, l)
		case podmanStorageDamaged(l) || strings.Contains(low, "corrupt"):
			ns = add(ns, l)
		}
	}
	return ws, ns
}
