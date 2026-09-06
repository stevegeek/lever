package jail

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// TarImage is one image described by a docker archive's manifest.json.
type TarImage struct {
	// RepoTags as written by `docker save`: unqualified names such as
	// "scionlocal/lever-claude:arm64" or "alpine:latest".
	RepoTags []string
	// ConfigDigest is the bare hex sha256 of the image config blob — the value
	// `podman load` assigns as the image ID (see normalizeImageID), so it is
	// what an "already loaded" check compares against the jail.
	ConfigDigest string
	// Labels from the config's `config.Labels` (nil when the image has none).
	Labels map[string]string
}

// maxConfigBlob bounds how much of a manifest-named config entry is read into
// memory; a real config is a few KB, and a layer that a corrupt manifest
// points at must not be slurped.
const maxConfigBlob = 8 << 20

// ReadImageTar parses a docker archive (`docker save` output, either the
// legacy "<hex>.json" layout or the OCI "blobs/sha256/<hex>" layout that
// docker ≥ 25 writes) and returns every image it carries. docker writes
// manifest.json AFTER the layer blobs, so the archive is opened as an
// *os.File and walked twice: archive/tar skips entry bodies with Seek when
// its reader is a Seeker, which makes each walk a handful of header reads,
// not a multi-GB read. The file must therefore reach tar.NewReader directly
// (no bufio wrapper, which would hide the Seeker).
func ReadImageTar(tarPath string) ([]TarImage, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("image tar: %w", err)
	}
	defer f.Close()
	manifest, err := readTarEntry(f, "manifest.json", maxConfigBlob)
	if err != nil {
		return nil, fmt.Errorf("image tar %s: %w", tarPath, err)
	}
	var entries []struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.Unmarshal(manifest, &entries); err != nil {
		return nil, fmt.Errorf("image tar %s: manifest.json: %w", tarPath, err)
	}
	imgs := make([]TarImage, 0, len(entries))
	for _, e := range entries {
		cfg, err := readTarEntry(f, e.Config, maxConfigBlob)
		if err != nil {
			return nil, fmt.Errorf("image tar %s: config %s: %w", tarPath, e.Config, err)
		}
		var parsed struct {
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"config"`
		}
		if err := json.Unmarshal(cfg, &parsed); err != nil {
			return nil, fmt.Errorf("image tar %s: config %s: %w", tarPath, e.Config, err)
		}
		sum := sha256.Sum256(cfg)
		imgs = append(imgs, TarImage{
			RepoTags:     e.RepoTags,
			ConfigDigest: hex.EncodeToString(sum[:]),
			Labels:       parsed.Config.Labels,
		})
	}
	return imgs, nil
}

// readTarEntry rewinds f and returns the body of the entry named name (path-
// cleaned, so "./manifest.json" matches), or an error naming it when absent.
func readTarEntry(f *os.File, name string, limit int64) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	want := path.Clean(name)
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s entry (not a docker archive?)", name)
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if path.Clean(h.Name) != want {
			continue
		}
		if h.Size > limit {
			return nil, fmt.Errorf("%s is %d bytes, over the %d limit", name, h.Size, limit)
		}
		return io.ReadAll(io.LimitReader(tr, limit))
	}
}

// FindTarImage returns the image in imgs tagged ref, comparing normalised
// references (see normalizeRef) so a qualified or tagless config ref matches
// the unqualified RepoTags docker writes. The error names the ref and lists
// the tags the tar does carry.
func FindTarImage(imgs []TarImage, ref string) (TarImage, error) {
	want := normalizeRef(ref)
	var have []string
	for _, img := range imgs {
		for _, t := range img.RepoTags {
			if normalizeRef(t) == want {
				return img, nil
			}
			have = append(have, t)
		}
	}
	return TarImage{}, fmt.Errorf("image %q is not in the tar (tags present: %s)", ref, strings.Join(have, ", "))
}

// SameImageRef reports whether two image references name the same image
// once podman's and docker's qualifications are stripped: "docker.io/" or the
// "localhost/" alias the load step adds (aliasLocalhost), the implied
// "library/", and an implied ":latest". A different tag or registry is a
// different image.
func SameImageRef(a, b string) bool {
	return normalizeRef(strings.TrimPrefix(a, "localhost/")) == normalizeRef(strings.TrimPrefix(b, "localhost/"))
}

// normalizeRef canonicalises a docker image reference for equality: strips a
// leading "docker.io/", prefixes the implied "library/" of a single-component
// name, and appends ":latest" when the name carries no tag or digest. The tag
// test is "a ':' after the last '/'", so a registry port is not a tag.
func normalizeRef(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "docker.io/")
	if !strings.Contains(ref, "/") {
		ref = "library/" + ref
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	if !strings.ContainsAny(name, ":@") {
		ref += ":latest"
	}
	return ref
}

// ImageTarLabel returns the value of label key on the image tagged ref in
// the archive at tarPath ("" when the image carries no such label). It is
// doctor's substitute for `docker image inspect` on a host with no docker.
func ImageTarLabel(tarPath, ref, key string) (string, error) {
	imgs, err := ReadImageTar(tarPath)
	if err != nil {
		return "", err
	}
	img, err := FindTarImage(imgs, ref)
	if err != nil {
		return "", fmt.Errorf("image tar %s: %w", tarPath, err)
	}
	return img.Labels[key], nil
}
