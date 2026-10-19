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
	// RepoTags are unqualified, as `docker save` writes them.
	RepoTags []string
	// ConfigDigest is the bare hex sha256 of the image config blob, the value
	// `podman load` assigns as the image ID.
	ConfigDigest string
	Labels       map[string]string
}

// maxConfigBlob: a real config is a few KB, and a layer that a corrupt
// manifest points at must not be read into memory.
const maxConfigBlob = 8 << 20

// ReadImageTar returns every image a `docker save` archive carries, in the
// legacy or the OCI layout. docker writes manifest.json after the layer
// blobs, so the archive is walked more than once; archive/tar skips bodies
// with Seek when its reader is a Seeker, so the *os.File must reach
// tar.NewReader directly (a bufio wrapper would hide the Seeker and make each
// walk a multi-GB read).
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

// FindTarImage returns the image in imgs tagged ref, so a qualified or
// tagless config ref matches the unqualified RepoTags docker writes.
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
// once podman's and docker's implied qualifications are stripped, including
// the "localhost/" alias the load step adds.
func SameImageRef(a, b string) bool {
	return normalizeRef(strings.TrimPrefix(a, "localhost/")) == normalizeRef(strings.TrimPrefix(b, "localhost/"))
}

// normalizeRef looks for a tag after the last '/', so a registry port is not
// a tag.
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
// the archive at tarPath, for a host with no docker to inspect.
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
