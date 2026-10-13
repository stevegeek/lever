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
// store: orphan and incomplete layers, and podman's own corruption warnings.
//
// Orphan layers matter because podman reuses any layer in the store with the
// same content, used or not, so an orphan left damaged by an interrupted
// write makes a load fail with "creating file-getter: readlink
// .../overlay/l/: no such file or directory". `podman image prune` never
// removes layers no image owns, and podman before 5.x has no command that
// does.
type StoreHealth struct {
	Version    string // podman version, e.g. "4.9.3"
	Driver     string // storage driver, e.g. "overlay"
	Images     int
	Containers int
	Layers     int
	// Orphans counts layers no image (or id-mapped copy) and no container
	// reaches.
	Orphans     int
	OrphanBytes int64 // their uncompressed size, as the store records it
	Incomplete  int   // layers flagged incomplete (a write that never finished)
	// Warnings are podman's own stderr lines that say the store is corrupted,
	// sanitized and clipped. They fail the doctor row.
	Warnings []string
	// Notices are podman stderr lines that hint at damage but also appear in
	// stores that work ("layer not known", a bare "corrupt"). They warn.
	Notices []string
}

// MajorVersion is the leading number of Version, or 0.
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
	// layers.json takes about 1 KB a layer, so this is thousands of layers.
	storeOutputLimit = 16 << 20
	storeWarningsMax = 3
)

// driverName is checked because the store's metadata paths are built from it.
var driverName = regexp.MustCompile(`^[a-z0-9]+$`)

type podmanInfo struct {
	Store struct {
		GraphRoot       string `json:"graphRoot"`
		GraphDriverName string `json:"graphDriverName"`
	} `json:"store"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

// containers/storage's metadata files are internal but stable and written
// atomically, so a reader sees a whole file; podman before 5.x has no command
// that lists layers, so they are the only read-only way to count orphans.
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

// storeFileScript exits 3 when $1 is absent: a store with no containers yet
// has no containers.json.
const storeFileScript = `test -e "$1" || exit 3; exec timeout ` + storeCmdSeconds + ` cat -- "$1"`

// ReadStoreHealth reads the health of the jail's rootless podman store
// through the in-jail runner r. It is read-only: no layer is ever removed
// (podman's own startup cleanup may still run inside podman info).
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

// readStoreFile treats a missing or zero-length file as an empty list: a
// fresh store may hold a zero-length file.
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

// orphanLayers counts the layers no image and no container reaches. An
// image's id-mapped copies (made by a run under another user-namespace
// mapping) are roots too.
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

// appendCorruptionWarnings sorts podman's stderr lines that name damage into
// ws (the store is corrupted) and ns (hints that also appear in working
// stores).
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
