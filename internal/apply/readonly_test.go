package apply

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

func TestManagerTreeVolumes(t *testing.T) {
	if got := managerTreeVolumes("/lever", nil); got != nil {
		t.Fatalf("no plan must give no volumes (so no inline config), got %v", got)
	}
	got := managerTreeVolumes("/lever", []config.TreeMount{
		{Rel: "assistant", ReadOnly: false},
		{Rel: "assistant/tools", ReadOnly: true},
	})
	want := []scion.VolumeMount{
		{Source: "/lever/assistant", Target: "/workspace/assistant", ReadOnly: false},
		{Source: "/lever/assistant/tools", Target: "/workspace/assistant/tools", ReadOnly: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("volumes =\n%v\nwant\n%v", got, want)
	}
}

// readOnlyConfig writes an instance whose manager protects assistant/tools,
// creating that directory under the tree unless the caller wants to shape
// the tree itself.
func readOnlyConfig(t *testing.T, makeDir bool) (dir string, app *config.App) {
	t.Helper()
	dir = t.TempDir()
	if makeDir {
		if err := os.MkdirAll(filepath.Join(dir, "workspace", "assistant", "tools"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, config.CanonicalName)
	body := "name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n  read_only:\n    - assistant/tools\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return dir, app
}

// manager.read_only reaches the CREATE as inline-config volumes on stdin:
// the entry read-only and its ancestor pinned read-write, both over their
// own place in the workspace.
func TestStartManagerCarriesReadOnlyVolumes(t *testing.T) {
	_, app := readOnlyConfig(t, true)
	f := scionOKRunner()
	if err := runApply(app, Deps{Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{})}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var start *proc.Call
	for i := range f.Calls {
		if strings.Contains(strings.Join(f.Calls[i].Args, " "), "-- hello") {
			start = &f.Calls[i]
		}
	}
	if start == nil {
		t.Fatalf("no manager start call; calls=%+v", f.Calls)
	}
	if !slices.Contains(start.Args, "--config") {
		t.Fatalf("start argv %q must send inline config", start.Args)
	}
	var body struct {
		Volumes []scion.VolumeMount `json:"volumes"`
	}
	if err := json.Unmarshal([]byte(start.Stdin), &body); err != nil {
		t.Fatalf("stdin %q: %v", start.Stdin, err)
	}
	jp := JailPath(app.Tree, app.Tree, "")
	want := []scion.VolumeMount{
		{Source: filepath.ToSlash(filepath.Join(jp, "assistant")), Target: "/workspace/assistant"},
		{Source: filepath.ToSlash(filepath.Join(jp, "assistant", "tools")), Target: "/workspace/assistant/tools", ReadOnly: true},
	}
	if !reflect.DeepEqual(body.Volumes, want) {
		t.Fatalf("volumes =\n%v\nwant\n%v", body.Volumes, want)
	}
}

// A read_only path reached through a symlink fails the apply naming the
// link, with no start attempted (and so, under --fresh, no delete either:
// the check runs before the record is touched).
func TestStartManagerRefusesSymlinkedReadOnlyPath(t *testing.T) {
	dir, app := readOnlyConfig(t, false)
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "workspace", "assistant")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	f := scionOKRunner()
	err := runApply(app, Deps{Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{})})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), "assistant/tools") {
		t.Fatalf("want a symlink refusal naming the entry, got %v", err)
	}
	for _, c := range f.Calls {
		if j := strings.Join(c.Args, " "); strings.Contains(j, "-- hello") || slices.Contains(c.Args, "delete") {
			t.Fatalf("no start or delete may be attempted; got %q", j)
		}
	}
}
