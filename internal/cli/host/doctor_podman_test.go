package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

func storeReader(h jail.StoreHealth, err error) func(context.Context) (jail.StoreHealth, error) {
	return func(context.Context) (jail.StoreHealth, error) { return h, err }
}

func TestCheckPodmanStorageHealthy(t *testing.T) {
	r := checkPodmanStorage(context.Background(), storeReader(jail.StoreHealth{Version: "4.9.3", Driver: "overlay", Images: 2, Containers: 3, Layers: 40}, nil))
	if !r.ok || r.fix != "" || r.name != "podman storage" {
		t.Fatalf("healthy store = %+v", r)
	}
	if !strings.Contains(r.detail, "podman 4.9.3, overlay: 2 images, 3 containers, 40 layers, no orphan layers") {
		t.Errorf("detail = %q", r.detail)
	}
}

func TestCheckPodmanStorageOrphansWarnOn4x(t *testing.T) {
	r := checkPodmanStorage(context.Background(), storeReader(jail.StoreHealth{Version: "4.9.3", Driver: "overlay", Layers: 80, Orphans: 37, OrphanBytes: 1_500_000_000, Incomplete: 1}, nil))
	if !r.ok || r.fix == "" {
		t.Fatalf("orphans must warn, not fail: %+v", r)
	}
	for _, want := range []string{"37 layers no image or container uses (1.5 GB)", "1 layer flagged incomplete"} {
		if !strings.Contains(r.detail, want) {
			t.Errorf("detail lacks %q: %q", want, r.detail)
		}
	}
	for _, want := range []string{"before 5.x has no command", "back up", "podman system reset --force", "without --fresh", "never deletes a layer"} {
		if !strings.Contains(r.fix, want) {
			t.Errorf("fix lacks %q: %q", want, r.fix)
		}
	}
	if strings.Contains(r.fix, "system check") {
		t.Errorf("a 4.x fix must not offer podman system check: %q", r.fix)
	}
}

func TestCheckPodmanStorageOrphansOn5xOfferSystemCheck(t *testing.T) {
	r := checkPodmanStorage(context.Background(), storeReader(jail.StoreHealth{Version: "5.2.1", Driver: "overlay", Orphans: 1}, nil))
	if !r.ok || !strings.Contains(r.fix, "podman system check --repair") || !strings.Contains(r.detail, "1 layer no image") {
		t.Fatalf("5.x orphans = %+v", r)
	}
}

func TestCheckPodmanStorageCorruptionFails(t *testing.T) {
	r := checkPodmanStorage(context.Background(), storeReader(jail.StoreHealth{
		Version: "4.9.3", Driver: "overlay", Orphans: 2,
		Warnings: []string{"WARN[0000] Image x exists in local storage but may be corrupted"},
	}, nil))
	if r.ok {
		t.Fatalf("corruption must fail: %+v", r)
	}
	if !strings.Contains(r.detail, "podman reports storage corruption: WARN[0000] Image x") || !strings.Contains(r.detail, "2 layers") {
		t.Errorf("detail = %q", r.detail)
	}
	if !strings.Contains(r.fix, "podman system reset --force") {
		t.Errorf("fix = %q", r.fix)
	}
}

func TestCheckPodmanStorageReadErrorIsNotChecked(t *testing.T) {
	r := checkPodmanStorage(context.Background(), storeReader(jail.StoreHealth{}, errors.New("podman info: exit status 125\nmore")))
	if !r.ok || r.fix != "" || !strings.HasPrefix(r.detail, "not checked") || strings.Contains(r.detail, "more") {
		t.Fatalf("read error = %+v", r)
	}
}

// The row end to end over a fake jail runner: the parse in jail and the
// verdict here agree on a store left with orphans by a failed load.
func TestCheckPodmanStorageOverAFakeJail(t *testing.T) {
	const root = "/home/u/.local/share/containers/storage"
	r := proc.NewFakeRunner()
	r.Script("timeout 20 podman info --format json", proc.Result{Stdout: `{"store":{"graphDriverName":"overlay","graphRoot":"` + root + `"},"version":{"Version":"4.9.3"}}`})
	r.Script("timeout 20 podman images -a -q", proc.Result{Stdout: "i1\n"})
	for rel, body := range map[string]string{
		"overlay-layers/layers.json":         `[{"id":"base"},{"id":"top","parent":"base"},{"id":"left","diff-size":2048}]`,
		"overlay-images/images.json":         `[{"id":"i1","layer":"top"}]`,
		"overlay-containers/containers.json": `[]`,
	} {
		r.Script("sh -c test -e \"$1\" || exit 3; exec timeout 20 cat -- \"$1\" _ "+root+"/"+rel, proc.Result{Stdout: body})
	}
	got := checkPodmanStorage(context.Background(), func(ctx context.Context) (jail.StoreHealth, error) { return jail.ReadStoreHealth(ctx, r) })
	if !got.ok || got.fix == "" || !strings.Contains(got.detail, "1 layer no image or container uses (2.0 KB)") {
		t.Fatalf("row = %+v", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 KB", 1_500_000: "1.5 MB", 37_000_000_000: "37.0 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
