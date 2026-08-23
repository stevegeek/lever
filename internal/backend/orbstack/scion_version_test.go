package orbstack

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/backendtest"
	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/proc"
)

// ensureScion in version mode must resolve the real go binary, `go mod download`
// the pinned module, and cross-compile ./cmd/scion FROM the module's source dir
// using that absolute binary (so the toolchain resolves outside any project dir).
func TestEnsureScionVersionBuildsFromPinnedModule(t *testing.T) {
	const pin = "666333f9"
	const moduleDir = "/mod/github.com/!google!cloud!platform/scion@v0.0.0-x"
	f := proc.NewFakeRunner()
	f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json github.com/GoogleCloudPlatform/scion@"+pin,
		proc.Result{Stdout: `{"Version":"v0.0.0-x","Dir":"` + moduleDir + `"}`})
	f.Script("/opt/go/bin/go build -o", proc.Result{})
	f.Script("orb -m lever-vtest uname -m", proc.Result{Stdout: "arm64\n"})
	f.Script("orb -m lever-vtest sh -c", proc.Result{Code: 1})
	f.Script("orb -u root -m lever-vtest", proc.Result{})
	f.Script("bash -c", proc.Result{})

	backendtest.StageFakeBuildOutput(t, "lever-vtest")
	o := New(f, "lever-vtest")
	if err := o.Guest().EnsureScion(context.Background(), guest.ScionSpec{Version: pin}); err != nil {
		t.Fatalf("EnsureScion(version): %v", err)
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
	if build.Env["GOOS"] != "linux" || build.Env["GOARCH"] != "arm64" {
		t.Fatalf("build not cross-compiled for the jail: %v", build.Env)
	}
}

// A failed `go mod download` (bad commit) must surface, not silently fall through.
func TestEnsureScionVersionDownloadErrorSurfaces(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("go env GOROOT", proc.Result{Stdout: "/opt/go\n"})
	f.Script("orb -m lever-vtest uname -m", proc.Result{Stdout: "arm64\n"})
	f.Script("/opt/go/bin/go mod download -json", proc.Result{Stdout: `{"Error":"unknown revision deadbeef"}`})
	o := New(f, "lever-vtest")
	if err := o.Guest().EnsureScion(context.Background(), guest.ScionSpec{Version: "deadbeef"}); err == nil {
		t.Fatal("expected error when go mod download reports a bad revision")
	} else if !strings.Contains(err.Error(), "unknown revision") {
		t.Errorf("error must carry the download failure, got %v", err)
	}
}
