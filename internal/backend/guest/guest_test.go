package guest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

// orbShaped and limaShaped are two argv-prefix shapes exercised by every test
// below, so the package proves it's genuinely backend-agnostic and not just
// orbstack with the serial numbers filed off.
type prefixShape struct {
	name       string
	userPrefix []string
	rootPrefix []string
}

func prefixShapes(machine string) []prefixShape {
	return []prefixShape{
		{"orb-shaped", []string{"orb", "-m", machine}, []string{"orb", "-u", "root", "-m", machine}},
		{"lima-shaped", []string{"limactl", "shell", machine}, []string{"limactl", "shell", machine, "sudo"}},
	}
}

func TestEnsureRuntimesArgv(t *testing.T) {
	for _, shape := range prefixShapes("lever-x") {
		t.Run(shape.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script(strings.Join(shape.rootPrefix, " "), exec.Result{})
			f.Script(strings.Join(shape.userPrefix, " "), exec.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}

			if err := g.EnsureRuntimes(context.Background(), "stephen"); err != nil {
				t.Fatalf("EnsureRuntimes: %v", err)
			}
			if len(f.Calls) != 5 {
				t.Fatalf("expected 5 calls, got %d: %+v", len(f.Calls), f.Calls)
			}

			// call 0: root apt install — RootPrefix then bash -lc <script>.
			first := f.Calls[0]
			wantFirstPrefix := append(append([]string{}, shape.rootPrefix[1:]...), "bash", "-lc")
			if first.Name != shape.rootPrefix[0] || !equalPrefix(first.Args, wantFirstPrefix) {
				t.Fatalf("call 0 = %+v, want name %q then prefix %v", first, shape.rootPrefix[0], wantFirstPrefix)
			}
			firstScript := first.Args[len(first.Args)-1]
			if !strings.Contains(firstScript, "apt-get install") || !strings.Contains(firstScript, "podman") {
				t.Errorf("call 0 script missing apt-get install/podman: %q", firstScript)
			}

			// call 1: root relax unprivileged userns (rootless-runtime prereq).
			userns := f.Calls[1]
			if userns.Name != shape.rootPrefix[0] {
				t.Fatalf("call 1 should be root-prefixed, got %+v", userns)
			}
			usernsScript := userns.Args[len(userns.Args)-1]
			if !strings.Contains(usernsScript, "apparmor_restrict_unprivileged_userns") {
				t.Errorf("call 1 script missing userns relaxation: %q", usernsScript)
			}

			// call 2: root subuid/subgid/linger — must mention the run user.
			second := f.Calls[2]
			if second.Name != shape.rootPrefix[0] {
				t.Fatalf("call 2 should be root-prefixed, got %+v", second)
			}
			secondScript := second.Args[len(second.Args)-1]
			if !strings.Contains(secondScript, "stephen") || !strings.Contains(secondScript, "loginctl enable-linger") {
				t.Errorf("call 2 script missing subid/linger for runUser: %q", secondScript)
			}

			// call 3: user rootless install.
			third := f.Calls[3]
			if third.Name != shape.userPrefix[0] {
				t.Fatalf("call 3 should be user-prefixed, got %+v", third)
			}
			thirdScript := third.Args[len(third.Args)-1]
			if !strings.Contains(thirdScript, "get.docker.com/rootless") {
				t.Errorf("call 3 script missing rootless install: %q", thirdScript)
			}

			// call 4: user dockerd start.
			fourth := f.Calls[4]
			if fourth.Name != shape.userPrefix[0] {
				t.Fatalf("call 4 should be user-prefixed, got %+v", fourth)
			}
			fourthScript := fourth.Args[len(fourth.Args)-1]
			if !strings.Contains(fourthScript, "docker info") {
				t.Errorf("call 4 script missing dockerd start: %q", fourthScript)
			}
		})
	}
}

func equalPrefix(args, want []string) bool {
	if len(args) < len(want) {
		return false
	}
	for i, w := range want {
		if args[i] != w {
			return false
		}
	}
	return true
}

func TestGOARCHMapsUname(t *testing.T) {
	cases := map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}
	for uname, want := range cases {
		t.Run(uname, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script("limactl shell v uname -m", exec.Result{Stdout: uname + "\n"})
			g := Guest{Host: f, UserPrefix: []string{"limactl", "shell", "v"}}
			got, err := g.GOARCH(context.Background())
			if err != nil || got != want {
				t.Errorf("GOARCH(%q) = %q, %v; want %q", uname, got, err, want)
			}
		})
	}
}

func TestGOARCHUnrecognizedErrors(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb -m m uname -m", exec.Result{Stdout: "riscv64\n"})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}}
	if _, err := g.GOARCH(context.Background()); err == nil {
		t.Fatal("expected error for unrecognized guest architecture")
	} else if !strings.Contains(err.Error(), "riscv64") {
		t.Fatalf("error should name the raw uname value; got %v", err)
	}
}

func TestEnsureScionBuildsAndInstalls(t *testing.T) {
	for _, shape := range prefixShapes("lever-jail") {
		t.Run(shape.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " ")+" uname -m", exec.Result{Stdout: "arm64\n"})
			f.Script("go build", exec.Result{})
			f.Script("bash -c", exec.Result{})
			src := t.TempDir() // must exist for the stat check
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-jail"}

			if err := g.EnsureScion(context.Background(), src, ""); err != nil {
				t.Fatalf("EnsureScion: %v", err)
			}

			var sawBuild, sawInstall bool
			wantRootWords := make([]string, 0, len(shape.rootPrefix))
			for _, w := range shape.rootPrefix {
				wantRootWords = append(wantRootWords, "'"+w+"'")
			}
			wantRootJoined := strings.Join(wantRootWords, " ")
			for _, c := range f.Calls {
				if c.Name == "go" && len(c.Args) > 0 && c.Args[0] == "build" {
					if c.Dir != src {
						t.Errorf("build Dir: want %q got %q", src, c.Dir)
					}
					if c.Env["GOOS"] != "linux" || c.Env["GOARCH"] != "arm64" {
						t.Errorf("build env: want linux/arm64 got %+v", c.Env)
					}
					var sawCmd bool
					var binArg string
					for i, a := range c.Args {
						if a == "./cmd/scion" {
							sawCmd = true
						}
						if a == "-o" && i+1 < len(c.Args) {
							binArg = c.Args[i+1]
						}
					}
					if !sawCmd {
						t.Errorf("build args should contain ./cmd/scion; got %+v", c.Args)
					}
					if !strings.Contains(binArg, "lever-scion-lever-jail") {
						t.Errorf("build output path should include per-machine name lever-scion-lever-jail; got %q", binArg)
					}
					sawBuild = true
				}
				if c.Name == "bash" && len(c.Args) >= 2 && c.Args[0] == "-c" {
					script := c.Args[1]
					if strings.Contains(script, "set -o pipefail") &&
						strings.Contains(script, "scion.tmp") &&
						strings.Contains(script, "mv") &&
						strings.Contains(script, "/usr/local/bin/scion") &&
						strings.Contains(script, wantRootJoined) {
						sawInstall = true
					}
				}
			}
			if !sawBuild {
				t.Fatalf("expected go build for ./cmd/scion in %q; calls=%+v", src, f.Calls)
			}
			if !sawInstall {
				t.Fatalf("expected bash -c atomic scion install via RootPrefix %v; calls=%+v", shape.rootPrefix, f.Calls)
			}
		})
	}
}

func TestEnsureScionSourceMissing(t *testing.T) {
	f := exec.NewFakeRunner()
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-jail"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-jail"}, Machine: "lever-jail"}

	err := g.EnsureScion(context.Background(), "/does/not/exist", "")
	if err == nil {
		t.Fatal("expected error for missing scion source, got nil")
	}
	if !strings.Contains(err.Error(), "scion source") {
		t.Fatalf("error should mention scion source; got: %v", err)
	}
	for _, c := range f.Calls {
		if c.Name == "go" && len(c.Args) > 0 && c.Args[0] == "build" {
			t.Fatalf("go build must NOT be called when source missing (stat short-circuits): %+v", c)
		}
	}
}

// ensureScion in version mode must resolve the real go binary, `go mod download`
// the pinned module, and cross-compile ./cmd/scion FROM the module's source dir
// using that absolute binary (so the toolchain resolves outside any project dir).
func TestEnsureScionVersionBuildsFromPinnedModule(t *testing.T) {
	const pin = "666333f9"
	const moduleDir = "/mod/github.com/!google!cloud!platform/scion@v0.0.0-x"
	f := exec.NewFakeRunner()
	f.Script("go env GOROOT", exec.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json github.com/GoogleCloudPlatform/scion@"+pin,
		exec.Result{Stdout: `{"Version":"v0.0.0-x","Dir":"` + moduleDir + `"}`})
	f.Script("/opt/go/bin/go build -o", exec.Result{})
	f.Script("orb -m lever-vtest uname -m", exec.Result{Stdout: "arm64\n"})
	f.Script("bash -c", exec.Result{})

	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-vtest"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-vtest"}, Machine: "lever-vtest"}
	if err := g.EnsureScion(context.Background(), "", pin); err != nil {
		t.Fatalf("EnsureScion(version): %v", err)
	}

	var build *exec.Call
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
	f := exec.NewFakeRunner()
	f.Script("go env GOROOT", exec.Result{Stdout: "/opt/go\n"})
	f.Script("/opt/go/bin/go mod download -json", exec.Result{Stdout: `{"Error":"unknown revision deadbeef"}`})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-vtest"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-vtest"}, Machine: "lever-vtest"}
	if err := g.EnsureScion(context.Background(), "", "deadbeef"); err == nil {
		t.Fatal("expected error when go mod download reports a bad revision")
	}
}

// TestInstallRootBinaryClosesSingleQuoteInjectionInDestPath proves destPath
// (and its derived .tmp) are safe to interpolate even if a future caller
// passes a metacharacter-laden value: InstallRootBinary's install script
// embeds destPath inside a single-quoted argument nested inside an outer
// `bash -c` script. An embedded `'` in destPath, substituted raw, closes that
// quote early FROM THE OUTER BASH'S PERSPECTIVE (the inner single-quoted
// segment is itself just text inside the outer script), letting anything
// after it run as an extra, injected command on the HOST. This test uses the
// REAL runner (not FakeRunner) with a harmless `env` RootPrefix stand-in (env
// just execs its argv, mirroring "the guest prefix runs bash -c '<script>'"
// exactly) so the actual composed shell script is genuinely parsed by bash,
// not just string-matched.
func TestInstallRootBinaryClosesSingleQuoteInjectionInDestPath(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "src-bin")
	if err := os.WriteFile(local, []byte("bin-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The injected command's target is a bare, slash-free relative filename:
	// it must not itself embed "/" into destPath, or a CORRECTLY quoted
	// destPath (the fixed behaviour) would legitimately fail `mv` with ENOENT
	// (an unrelated implied intermediate directory), muddying what this test
	// checks. `go test` runs with the package directory as cwd, so a
	// still-vulnerable injection lands the marker there; resolve + clean it up
	// via os.Getwd regardless of outcome.
	const marker = "PWNED-marker-should-not-exist-guest-test"
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(wd, marker)
	defer os.Remove(sentinel)
	// The embedded `'` is the injection; `; touch <marker> #` is the payload
	// that runs as a separate command if the quote isn't neutralized.
	dest := filepath.Join(dir, "dst") + "'; touch " + marker + " #"

	g := Guest{Host: exec.RealRunner{}, RootPrefix: []string{"env"}, Machine: "test"}
	if err := g.InstallRootBinary(context.Background(), local, dest); err != nil {
		t.Fatalf("InstallRootBinary: %v", err)
	}

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("destPath injection ran an extra command (sentinel file was created): shell injection via destPath")
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("expected the binary installed at the literal (quote-laden) destPath %q: %v", dest, err)
	}
	if string(data) != "bin-content" {
		t.Fatalf("installed file content = %q, want %q", data, "bin-content")
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed file should be executable: mode=%v err=%v", fi.Mode(), err)
	}
}

func TestShellSingleQuote(t *testing.T) {
	if got := shellSingleQuote("ab"); got != "'ab'" {
		t.Errorf("shellSingleQuote(ab): want 'ab' got %q", got)
	}
	if got := shellSingleQuote("a'b"); got != `'a'\''b'` {
		t.Errorf(`shellSingleQuote(a'b): want 'a'\''b' got %q`, got)
	}
}
