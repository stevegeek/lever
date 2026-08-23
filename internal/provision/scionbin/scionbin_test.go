package scionbin

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestValidateNamesAllThreeKeys(t *testing.T) {
	err := Spec{}.Validate()
	if err == nil {
		t.Fatal("expected an error when no scion mode is configured")
	}
	for _, want := range []string{"binary", "source", "version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the %q key", err, want)
		}
	}
}

func TestResolveSourceMissingNeverBuilds(t *testing.T) {
	f := proc.NewFakeRunner()
	_, err := Resolve(context.Background(), f, Spec{Source: "/does/not/exist"}, "arm64", "m")
	var srcErr *SourceError
	if !errors.As(err, &srcErr) || srcErr.Path != "/does/not/exist" || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error should be a SourceError wrapping the stat failure; got: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("go must not run when the source is missing: %+v", f.Calls)
	}
}

func TestResolveSourceCrossCompiles(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("go build", proc.Result{})
	src := t.TempDir()
	out, err := Resolve(context.Background(), f, Spec{Source: src}, "arm64", "lever-jail")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if out != OutputPath("lever-jail") || !strings.Contains(out, "lever-scion-lever-jail") {
		t.Fatalf("output path %q", out)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %+v", f.Calls)
	}
	c := f.Calls[0]
	if c.Name != "go" || c.Dir != src || c.Env["GOOS"] != "linux" || c.Env["GOARCH"] != "arm64" {
		t.Fatalf("build call = %+v", c)
	}
	if strings.Join(c.Args, " ") != "build -o "+out+" ./cmd/scion" {
		t.Fatalf("build args = %v", c.Args)
	}
}

// Version mode must resolve the real go binary, `go mod download` the pinned
// module, and cross-compile FROM the module's source dir using that absolute
// binary (so the toolchain resolves outside any project dir).
func TestResolveVersionBuildsFromPinnedModule(t *testing.T) {
	const pin = "666333f9"
	const moduleDir = "/mod/github.com/!google!cloud!platform/scion@v0.0.0-x"
	f := proc.NewFakeRunner()
	f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json "+ModulePath+"@"+pin,
		proc.Result{Stdout: `{"Version":"v0.0.0-x","Dir":"` + moduleDir + `"}`})
	f.Script("/opt/go/bin/go build -o", proc.Result{})

	if _, err := Resolve(context.Background(), f, Spec{Version: pin}, "arm64", "m"); err != nil {
		t.Fatalf("Resolve(version): %v", err)
	}
	var build *proc.Call
	for i := range f.Calls {
		if c := f.Calls[i]; c.Name == "/opt/go/bin/go" && len(c.Args) > 0 && c.Args[0] == "build" {
			build = &f.Calls[i]
		}
	}
	if build == nil {
		t.Fatal("expected a cross-compile build with the resolved absolute go binary")
	}
	if build.Dir != moduleDir {
		t.Fatalf("build ran in %q, want the pinned module dir %q", build.Dir, moduleDir)
	}
}

func TestFetchModuleErrors(t *testing.T) {
	cases := []struct {
		name, stdout, want string
	}{
		{"download error", `{"Error":"unknown revision deadbeef"}`, "unknown revision"},
		{"no dir", `{"Version":"v0"}`, "no source dir"},
		{"garbage", `not json`, "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
			f.Script("/opt/go/bin/go mod download -json", proc.Result{Stdout: c.stdout})
			_, _, err := FetchModule(context.Background(), f, "deadbeef")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// "No Go toolchain on the jail host" (issue #27) means exactly this: in binary
// mode nothing ever invokes `go`.
func TestResolveBinaryModeNeverInvokesGo(t *testing.T) {
	bin := writeELF64(t, t.TempDir(), emAArch64, etExec)
	f := proc.NewFakeRunner()
	out, err := Resolve(context.Background(), f, Spec{Binary: bin}, "arm64", "m")
	if err != nil {
		t.Fatalf("Resolve(binary): %v", err)
	}
	if out != bin {
		t.Fatalf("binary mode must return the supplied path; got %q", out)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("binary mode must never run a command; got %+v", f.Calls)
	}
}

func TestResolveBinaryModeRejectsWrongArch(t *testing.T) {
	bin := writeELF64(t, t.TempDir(), emAArch64, etExec)
	if _, err := Resolve(context.Background(), proc.NewFakeRunner(), Spec{Binary: bin}, "amd64", "m"); err == nil {
		t.Fatal("expected an arch mismatch error")
	}
}

func TestBuildsWebAssets(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want bool
	}{
		{"version + web ui", Spec{Version: "e82a2a08", WebUI: true}, true},
		{"source + web ui", Spec{Source: "/src/scion", WebUI: true}, true},
		{"version without web ui", Spec{Version: "e82a2a08"}, false},
		// A prebuilt binary carries no source to build from, and may already
		// embed its own assets — skip, never fail.
		{"binary + web ui", Spec{Binary: "/bin/scion", WebUI: true}, false},
		{"nothing", Spec{WebUI: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.BuildsWebAssets(); got != c.want {
				t.Fatalf("BuildsWebAssets()=%v want %v", got, c.want)
			}
		})
	}
}
