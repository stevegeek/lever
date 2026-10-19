package buildstamp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		name, base, rev   string
		dirty             bool
		modVer, bin, want string
	}{
		{"commit", "0.5.0", "a8abdaef12345678", false, "", "", "0.5.0 (a8abdaef1234)"},
		{"commit-dirty", "0.5.0", "a8abdaef12345678", true, "", "", "0.5.0 (a8abdaef1234-dirty)"},
		{"short-commit-not-truncated", "0.5.0", "abc123", false, "", "", "0.5.0 (abc123)"},
		{"module-version-when-no-vcs", "0.5.0", "", false, "v0.5.0", "", "0.5.0 (v0.5.0)"},
		{"devel-module-ignored", "0.5.0", "", false, "(devel)", "", "0.5.0"},
		{"nothing-available", "0.5.0", "", false, "", "", "0.5.0"},
		{"commit-wins-over-module", "0.5.0", "deadbeef", true, "v0.5.0", "", "0.5.0 (deadbeef-dirty)"},
		{"worktree-build-gets-binary-hash", "0.5.0", "", false, "(devel)", "1a2b3c4d5e6f", "0.5.0 (bin 1a2b3c4d5e6f)"},
		{"dirty-build-gets-binary-hash", "0.5.0", "deadbeef", true, "", "1a2b3c4d5e6f", "0.5.0 (deadbeef-dirty, bin 1a2b3c4d5e6f)"},
	} {
		if got := format(c.base, c.rev, c.dirty, c.modVer, c.bin); got != c.want {
			t.Errorf("%s: format(%q,%q,%v,%q,%q) = %q, want %q", c.name, c.base, c.rev, c.dirty, c.modVer, c.bin, got, c.want)
		}
	}
}

// TestNeedsBinaryHash: a clean commit and a tagged module are not hashed.
func TestNeedsBinaryHash(t *testing.T) {
	for _, c := range []struct {
		rev    string
		dirty  bool
		modVer string
		want   bool
	}{
		{"deadbeef", false, "", false},
		{"deadbeef", true, "", true},
		{"", false, "v0.5.0", false},
		{"", false, "(devel)", true},
		{"", false, "", true},
	} {
		if got := needsBinaryHash(c.rev, c.dirty, c.modVer); got != c.want {
			t.Errorf("needsBinaryHash(%q,%v,%q) = %v, want %v", c.rev, c.dirty, c.modVer, got, c.want)
		}
	}
	if h := executableHash(); len(h) != 12 {
		t.Fatalf("executableHash() = %q, want 12 hex digits of the test binary", h)
	}
}

// TestFromBuildStampWins: an ldflags stamp replaces the VCS stamp whole, its
// clean state included, since the VCS stamp may describe another checkout.
func TestFromBuildStampWins(t *testing.T) {
	outer := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0uter0000000000"}, {Key: "vcs.modified", Value: "true"},
	}}
	for _, c := range []struct {
		name          string
		info          *debug.BuildInfo
		rev, modified string
		want          provenance
	}{
		{"vcs only", outer, "", "", provenance{rev: "0uter0000000000", dirty: true, modVersion: "(devel)"}},
		{"stamp clean", outer, "abc", "false", provenance{rev: "abc", modVersion: "(devel)"}},
		{"stamp dirty", outer, "abc", "true", provenance{rev: "abc", dirty: true, modVersion: "(devel)"}},
		{"stamp without modified is clean", outer, "abc", "", provenance{rev: "abc", modVersion: "(devel)"}},
		{"modified alone is ignored", outer, "", "false", provenance{rev: "0uter0000000000", dirty: true, modVersion: "(devel)"}},
		{"no build info", nil, "abc", "true", provenance{rev: "abc", dirty: true}},
		{"nothing", nil, "", "", provenance{}},
	} {
		if got := fromBuild(c.info, c.rev, c.modified); got != c.want {
			t.Errorf("%s: fromBuild = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestStampedBuild links a binary with the -X names the Makefile and the
// release build pass, so a renamed variable cannot leave the stamp silently
// unset.
func TestStampedBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const pkg = "github.com/stevegeek/lever/internal/buildstamp"
	for _, f := range []string{"Makefile", ".goreleaser.yaml"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{pkg + ".revision=", pkg + ".modified="} {
			if !strings.Contains(string(b), "-X "+v) {
				t.Errorf("%s does not stamp %s", f, v)
			}
		}
	}
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if p, err := exec.LookPath("go"); err == nil {
		gobin = p
	}
	for _, c := range []struct{ modified, want string }{
		{"false", "1.2.3 (0123456789ab)\n"},
		{"true", "1.2.3 (0123456789ab-dirty, bin "},
	} {
		exe := filepath.Join(t.TempDir(), "stamped")
		cmd := exec.Command(gobin, "build", "-buildvcs=false",
			"-ldflags", "-X "+pkg+".revision=0123456789abcdef -X "+pkg+".modified="+c.modified,
			"-o", exe, "./internal/buildstamp/testdata/stamped")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
		out, err := exec.Command(exe).Output()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(out), c.want) {
			t.Errorf("modified=%s: stamped binary printed %q, want prefix %q", c.modified, out, c.want)
		}
	}
}
