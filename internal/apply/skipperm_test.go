package apply

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
)

// A bare manager's create carries the rm confirmation as inline env, and
// apply sets the project's hub env so records without it get "0" too.
func TestApplySetsSkipPermissionsOff(t *testing.T) {
	for _, on := range []bool{false, true} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "name: hello\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img\n"
		if on {
			body += "  even_more_dangerously_skip_permissions: true\n"
		}
		cfg := filepath.Join(dir, config.CanonicalName)
		if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		app, err := config.Load(cfg)
		if err != nil {
			t.Fatal(err)
		}
		f := scionOKRunner()
		if err := runApply(app, Deps{Scion: scion.New(&agentLifecycleRunner{FakeRunner: f, slug: app.Name}, scion.Options{})}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var start *proc.Call
		envSet, envBeforeStart := false, false
		for i := range f.Calls {
			a := strings.Join(f.Calls[i].Args, " ")
			if strings.Contains(a, "hub env set") && strings.Contains(a, scion.SkipPermissionsEnv+"=0") {
				envSet = true
				envBeforeStart = start == nil
			}
			if strings.Contains(a, "-- hello") {
				start = &f.Calls[i]
			}
		}
		if !envSet || !envBeforeStart {
			t.Fatalf("on=%v: project env set=%v before start=%v; calls=%+v", on, envSet, envBeforeStart, f.Calls)
		}
		if start == nil || !slices.Contains(start.Args, "--config") {
			t.Fatalf("on=%v: no manager start with inline config; calls=%+v", on, f.Calls)
		}
		var cfgBody struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(start.Stdin), &cfgBody); err != nil {
			t.Fatalf("stdin %q: %v", start.Stdin, err)
		}
		want := "0"
		if on {
			want = "1"
		}
		if got := cfgBody.Env[scion.SkipPermissionsEnv]; got != want {
			t.Fatalf("on=%v: inline env %s=%q, want %q", on, scion.SkipPermissionsEnv, got, want)
		}
	}
}
