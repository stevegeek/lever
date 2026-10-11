package jail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

const testGraphRoot = "/home/u/.local/share/containers/storage"

const testInfoJSON = `{"host":{"arch":"amd64"},"store":{"graphDriverName":"overlay","graphRoot":"` + testGraphRoot + `","imageStore":{"number":2}},"version":{"Version":"4.9.3"}}`

func storeFileKey(rel string) string {
	return "sh -c " + storeFileScript + " _ " + testGraphRoot + "/" + rel
}

// storeRunner scripts a store with two images sharing a base, one container
// on the newer one, an id-mapped copy of the newer image's top layer, and
// three orphans: the remains of a failed load (a chain of two) and an
// incomplete layer.
func storeRunner(t *testing.T) *proc.FakeRunner {
	t.Helper()
	r := proc.NewFakeRunner()
	r.Script("timeout 20 podman info --format json", proc.Result{Stdout: testInfoJSON})
	r.Script("timeout 20 podman images -a -q", proc.Result{Stdout: "img1\nimg2\n"})
	r.Script(storeFileKey("overlay-layers/layers.json"), proc.Result{Stdout: `[
		{"id":"base","diff-size":100},
		{"id":"a1","parent":"base","diff-size":10},
		{"id":"b1","parent":"base","diff-size":20},
		{"id":"b1-mapped","parent":"base","diff-size":20},
		{"id":"ctr","parent":"b1","diff-size":1},
		{"id":"orph1","diff-size":1000},
		{"id":"orph2","parent":"orph1","diff-size":500},
		{"id":"half","parent":"base","flags":{"incomplete":true},"diff-size":7}
	]`})
	r.Script(storeFileKey("overlay-images/images.json"), proc.Result{Stdout: `[
		{"id":"img1","layer":"a1"},
		{"id":"img2","layer":"b1","mapped-layers":["b1-mapped"]}
	]`})
	r.Script(storeFileKey("overlay-containers/containers.json"), proc.Result{Stdout: `[{"id":"c1","image":"img2","layer":"ctr"}]`})
	return r
}

func TestReadStoreHealthCountsOrphans(t *testing.T) {
	r := storeRunner(t)
	h, err := ReadStoreHealth(context.Background(), r)
	if err != nil {
		t.Fatalf("ReadStoreHealth: %v", err)
	}
	if h.Version != "4.9.3" || h.MajorVersion() != 4 || h.Driver != "overlay" {
		t.Errorf("version/driver = %q/%d/%q", h.Version, h.MajorVersion(), h.Driver)
	}
	if h.Images != 2 || h.Containers != 1 || h.Layers != 8 {
		t.Errorf("counts = %d images, %d containers, %d layers", h.Images, h.Containers, h.Layers)
	}
	if h.Orphans != 3 || h.OrphanBytes != 1507 || h.Incomplete != 1 {
		t.Errorf("orphans = %d (%d bytes), incomplete %d; want 3 (1507), 1", h.Orphans, h.OrphanBytes, h.Incomplete)
	}
	if len(h.Warnings) != 0 {
		t.Errorf("warnings = %q", h.Warnings)
	}
	// Read-only: no command but info, images and the cat script ran.
	for _, c := range r.Calls {
		argv := c.Argv()
		if !strings.HasPrefix(argv, "timeout 20 podman info ") && !strings.HasPrefix(argv, "timeout 20 podman images -a -q") &&
			!strings.HasPrefix(argv, "sh -c "+storeFileScript) {
			t.Errorf("unexpected command %q", argv)
		}
	}
}

func TestReadStoreHealthCollectsPodmansCorruptionWarning(t *testing.T) {
	r := storeRunner(t)
	r.Script("timeout 20 podman images -a -q", proc.Result{
		Stdout: "img1\n",
		Stderr: "WARN[0000] Image img2 exists in local storage but may be corrupted (remove the image to resolve the issue): layer not known\n" +
			"WARN[0000] Image img2 exists in local storage but may be corrupted (remove the image to resolve the issue): layer not known\n" +
			"WARN[0000] unrelated\n",
	})
	h, err := ReadStoreHealth(context.Background(), r)
	if err != nil {
		t.Fatalf("ReadStoreHealth: %v", err)
	}
	if len(h.Warnings) != 1 || !strings.Contains(h.Warnings[0], "may be corrupted") {
		t.Fatalf("warnings = %q, want the one corruption line once", h.Warnings)
	}
}

func TestReadStoreHealthMissingFilesAreEmpty(t *testing.T) {
	r := proc.NewFakeRunner()
	r.Script("timeout 20 podman info --format json", proc.Result{Stdout: testInfoJSON})
	r.Script("timeout 20 podman images -a -q", proc.Result{})
	r.Script(storeFileKey("overlay-layers/layers.json"), proc.Result{Stdout: ""})
	// images.json and containers.json are absent: the script exits 3.
	for _, rel := range []string{"overlay-images/images.json", "overlay-containers/containers.json"} {
		r.Script(storeFileKey(rel), proc.Result{Code: 3})
	}
	h, err := ReadStoreHealth(context.Background(), missingRunner{r})
	if err != nil {
		t.Fatalf("ReadStoreHealth: %v", err)
	}
	if h.Layers != 0 || h.Orphans != 0 || h.Images != 0 {
		t.Errorf("empty store = %+v", h)
	}
}

// missingRunner turns a scripted Code 3 into the error a real exit 3 brings.
type missingRunner struct{ *proc.FakeRunner }

func (m missingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := m.FakeRunner.Run(ctx, env, name, args...)
	if err == nil && res.Code != 0 {
		err = errors.New("exit status 3")
	}
	return res, err
}

func TestReadStoreHealthRefusesOddInfo(t *testing.T) {
	for name, info := range map[string]string{
		"relative root": `{"store":{"graphDriverName":"overlay","graphRoot":"storage"},"version":{"Version":"5.0.0"}}`,
		"dotted root":   `{"store":{"graphDriverName":"overlay","graphRoot":"/a/../b"},"version":{"Version":"5.0.0"}}`,
		"odd driver":    `{"store":{"graphDriverName":"over lay","graphRoot":"/s"},"version":{"Version":"5.0.0"}}`,
		"not json":      `Error: oops`,
	} {
		r := proc.NewFakeRunner()
		r.Script("timeout 20 podman info --format json", proc.Result{Stdout: info})
		if _, err := ReadStoreHealth(context.Background(), r); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if len(r.Calls) != 1 {
			t.Errorf("%s: read on after a bad info: %d calls", name, len(r.Calls))
		}
	}
}

func TestReadStoreHealthInfoFailure(t *testing.T) {
	r := proc.NewFakeRunner() // podman info unscripted: fails
	if _, err := ReadStoreHealth(context.Background(), r); err == nil || !strings.Contains(err.Error(), "podman info") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrphanLayersSurvivesAParentCycle(t *testing.T) {
	layers := []storeLayer{{ID: "a", Parent: "b"}, {ID: "b", Parent: "a"}, {ID: "c"}}
	n, _, _ := orphanLayers(layers, []storeImage{{ID: "i", Layer: "a"}}, nil)
	if n != 1 {
		t.Fatalf("orphans = %d, want 1 (c)", n)
	}
}

func TestMajorVersion(t *testing.T) {
	for v, want := range map[string]int{"4.9.3": 4, "5.2.1-dev": 5, "": 0, "x": 0, "10.0": 10} {
		if got := (StoreHealth{Version: v}).MajorVersion(); got != want {
			t.Errorf("MajorVersion(%q) = %d, want %d", v, got, want)
		}
	}
}
