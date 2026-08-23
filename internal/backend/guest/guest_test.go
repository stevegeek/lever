package guest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
			if len(f.Calls) != 6 {
				t.Fatalf("expected 6 calls, got %d: %+v", len(f.Calls), f.Calls)
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

			// call 5: user stages the pasta host-loopback mapping (per-agent netns).
			fifth := f.Calls[5]
			if fifth.Name != shape.userPrefix[0] {
				t.Fatalf("call 5 should be user-prefixed, got %+v", fifth)
			}
			fifthScript := fifth.Args[len(fifth.Args)-1]
			if !strings.Contains(fifthScript, "containers.conf.d") || !strings.Contains(fifthScript, "map-host-loopback") {
				t.Errorf("call 5 script missing pasta host-loopback mapping: %q", fifthScript)
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

// stageFakeBuildOutput creates the file a faked `go build` would have written,
// at the exact path scionbin.Resolve passes to `-o`. InstallRootBinaryIfChanged
// hashes that file for real, so it has to exist even when the build is a stub.
func stageFakeBuildOutput(t *testing.T, machine string) {
	t.Helper()
	p := filepath.Join(os.TempDir(), "lever-scion-"+machine)
	if err := os.WriteFile(p, []byte("fake-scion-"+machine), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
}

func TestEnsureScionBuildsAndInstalls(t *testing.T) {
	for _, shape := range prefixShapes("lever-jail") {
		t.Run(shape.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " ")+" uname -m", exec.Result{Stdout: "arm64\n"})
			f.Script("go build", exec.Result{})
			f.Script(strings.Join(shape.userPrefix, " ")+" /usr/bin/sha256sum", exec.Result{Code: 1})
			f.Script(strings.Join(shape.rootPrefix, " "), exec.Result{})
			src := t.TempDir() // must exist for the stat check
			stageFakeBuildOutput(t, "lever-jail")
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-jail"}

			if err := g.EnsureScion(context.Background(), ScionSpec{Source: src}); err != nil {
				t.Fatalf("EnsureScion: %v", err)
			}

			var sawBuild, sawInstall bool
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
				// The install is `<rootPrefix> bash -c <script>` with the binary
				// on stdin: argv only, no host shell, no quoted prefix words.
				if c.Stdin == "fake-scion-lever-jail" {
					want := append(append([]string{}, shape.rootPrefix[1:]...), "bash", "-c")
					if c.Name != shape.rootPrefix[0] || len(c.Args) != len(want)+1 || !reflect.DeepEqual(c.Args[:len(want)], want) {
						t.Fatalf("install argv: want %s %v <script>, got %s %v", shape.rootPrefix[0], want, c.Name, c.Args)
					}
					script := c.Args[len(c.Args)-1]
					if strings.Contains(script, "scion.tmp") &&
						strings.Contains(script, "mv") &&
						strings.Contains(script, "/usr/local/bin/scion") {
						sawInstall = true
					}
				}
			}
			if !sawBuild {
				t.Fatalf("expected go build for ./cmd/scion in %q; calls=%+v", src, f.Calls)
			}
			if !sawInstall {
				t.Fatalf("expected atomic scion install via RootPrefix %v with the binary on stdin; calls=%+v", shape.rootPrefix, f.Calls)
			}
		})
	}
}

func TestEnsureScionSourceMissing(t *testing.T) {
	f := exec.NewFakeRunner()
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-jail"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-jail"}, Machine: "lever-jail"}

	err := g.EnsureScion(context.Background(), ScionSpec{Source: "/does/not/exist"})
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
	f.Script("orb -m lever-vtest cat", exec.Result{Code: 1})
	f.Script("orb -u root -m lever-vtest", exec.Result{})

	stageFakeBuildOutput(t, "lever-vtest")
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-vtest"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-vtest"}, Machine: "lever-vtest"}
	if err := g.EnsureScion(context.Background(), ScionSpec{Version: pin}); err != nil {
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
	f.Script("orb -m lever-vtest uname -m", exec.Result{Stdout: "arm64\n"})
	f.Script("/opt/go/bin/go mod download -json", exec.Result{Stdout: `{"Error":"unknown revision deadbeef"}`})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-vtest"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-vtest"}, Machine: "lever-vtest"}
	if err := g.EnsureScion(context.Background(), ScionSpec{Version: "deadbeef"}); err == nil {
		t.Fatal("expected error when go mod download reports a bad revision")
	} else if !strings.Contains(err.Error(), "unknown revision") {
		t.Errorf("error must carry the download failure, got %v", err)
	}
}

// TestInstallRootBinaryClosesSingleQuoteInjectionInDestPath proves destPath
// (and its derived .tmp) are safe to interpolate even if a future caller
// passes a metacharacter-laden value: the guest-side install script embeds
// destPath inside single quotes, so an embedded `'` substituted raw would
// close that quote early and let anything after it run as an extra command
// in the guest. This test uses the REAL runner (not FakeRunner) with a
// harmless `env` RootPrefix stand-in (env just execs its argv, mirroring "the
// guest prefix runs bash -c <script>" exactly) so the actual script is
// genuinely parsed by bash, not just string-matched.
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

// stageBinary writes a host-local file standing in for a built scion binary and
// returns its path and hex sha256.
func stageBinary(t *testing.T, content string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scion")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return p, hex.EncodeToString(sum[:])
}

// installCalls counts the streaming install (a `bash -c 'cat > …'` script
// with the binary on stdin), which is the 158MB cost the digest check exists
// to avoid.
func installCalls(f *exec.FakeRunner) int {
	n := 0
	for _, c := range f.Calls {
		if len(c.Args) > 1 && c.Args[len(c.Args)-2] == "-c" && strings.Contains(c.Args[len(c.Args)-1], "cat > ") {
			n++
		}
	}
	return n
}

func TestInstallIfChangedSkipsWhenGuestBinaryMatches(t *testing.T) {
	for _, shape := range prefixShapes("lever-jail") {
		t.Run(shape.name, func(t *testing.T) {
			local, sum := stageBinary(t, "scion-bytes")
			f := exec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " ")+" /usr/bin/sha256sum",
				exec.Result{Stdout: sum + "  /usr/local/bin/scion\n"})
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-jail"}

			installed, err := g.InstallRootBinaryIfChanged(context.Background(), local, "/usr/local/bin/scion")
			if err != nil {
				t.Fatalf("InstallRootBinaryIfChanged: %v", err)
			}
			if installed {
				t.Fatal("a matching guest binary must report installed=false")
			}
			if n := installCalls(f); n != 0 {
				t.Fatalf("a matching guest binary must not be re-streamed; got %d install call(s)", n)
			}
		})
	}
}

func TestInstallIfChangedInstallsWhenGuestBinaryDiffersOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		guestDigest exec.Result
	}{
		{"guest binary differs", exec.Result{Stdout: "0000  /usr/local/bin/scion\n"}},
		{"guest binary absent", exec.Result{Code: 1, Stderr: "No such file"}},
		{"sha256sum unavailable", exec.Result{Code: 127, Stderr: "command not found"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, _ := stageBinary(t, "scion-bytes")
			shape := prefixShapes("lever-jail")[0]
			f := exec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " ")+" /usr/bin/sha256sum", tc.guestDigest)
			f.Script(strings.Join(shape.rootPrefix, " "), exec.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-jail"}

			installed, err := g.InstallRootBinaryIfChanged(context.Background(), local, "/usr/local/bin/scion")
			if err != nil {
				t.Fatalf("InstallRootBinaryIfChanged: %v", err)
			}
			if !installed {
				t.Fatal("an install must report installed=true")
			}
			if n := installCalls(f); n != 1 {
				t.Fatalf("want exactly 1 install call, got %d", n)
			}
		})
	}
}
func TestInstallIfChangedFailsOnUnreadableLocalFile(t *testing.T) {
	f := exec.NewFakeRunner()
	g := Guest{Host: f, UserPrefix: []string{"orb"}, RootPrefix: []string{"orb"}, Machine: "lever-jail"}
	_, err := g.InstallRootBinaryIfChanged(context.Background(), filepath.Join(t.TempDir(), "absent"), "/usr/local/bin/scion")
	if err == nil {
		t.Fatal("expected an error hashing a missing file")
	}
	if installCalls(f) != 0 {
		t.Error("nothing may be installed when the artifact cannot be read")
	}
}

// writeELF64 writes a minimal 64-bit little-endian ELF header for the given
// machine (elf.EM_* value) and object type — enough for scionbin.VerifyELFArch
// to accept or reject it. The full check is tested in internal/provision/
// scionbin; the copies here exist so EnsureScion's binary-mode flow can be
// exercised end to end through the guest transport.
func writeELF64(t *testing.T, dir string, machine uint16, etype uint16) string {
	t.Helper()
	h := make([]byte, 64)
	copy(h, []byte{0x7f, 'E', 'L', 'F'})
	h[4] = 2 // EI_CLASS: 64-bit
	h[5] = 1 // EI_DATA: little-endian
	h[6] = 1 // EI_VERSION
	binary.LittleEndian.PutUint16(h[16:], etype)
	binary.LittleEndian.PutUint16(h[18:], machine)
	binary.LittleEndian.PutUint32(h[20:], 1)  // e_version
	binary.LittleEndian.PutUint16(h[52:], 64) // e_ehsize
	path := filepath.Join(dir, "scion")
	if err := os.WriteFile(path, h, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	emAArch64 = 183 // elf.EM_AARCH64
	etExec    = 2
)

// THE load-bearing test for issue #27. "No Go toolchain on the jail host" means
// exactly this: in binary mode nothing ever invokes `go`. If it regresses, the
// feature is silently useless on the machine it exists for.
func TestEnsureScionBinaryModeNeverInvokesGo(t *testing.T) {
	for _, shape := range prefixShapes("lever-jail") {
		t.Run(shape.name, func(t *testing.T) {
			bin := writeELF64(t, t.TempDir(), emAArch64, etExec)
			f := exec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " ")+" uname -m", exec.Result{Stdout: "aarch64\n"})
			f.Script(strings.Join(shape.userPrefix, " ")+" /usr/bin/sha256sum", exec.Result{Code: 1})
			f.Script(strings.Join(shape.rootPrefix, " "), exec.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-jail"}

			if err := g.EnsureScion(context.Background(), ScionSpec{Binary: bin}); err != nil {
				t.Fatalf("EnsureScion(binary): %v", err)
			}
			for _, c := range f.Calls {
				if c.Name == "go" {
					t.Fatalf("binary mode must never invoke go; got %+v", c)
				}
			}
			if installCalls(f) != 1 {
				t.Errorf("want the supplied binary installed exactly once, got %d", installCalls(f))
			}
		})
	}
}

func TestEnsureScionBinaryModeRejectsWrongArch(t *testing.T) {
	// The guest is amd64, the supplied binary is arm64. Nothing may be written.
	bin := writeELF64(t, t.TempDir(), emAArch64, etExec)
	shape := prefixShapes("lever-jail")[0]
	f := exec.NewFakeRunner()
	f.Script(strings.Join(shape.userPrefix, " ")+" uname -m", exec.Result{Stdout: "x86_64\n"})
	g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-jail"}

	err := g.EnsureScion(context.Background(), ScionSpec{Binary: bin})
	if err == nil {
		t.Fatal("expected an arch mismatch error")
	}
	if installCalls(f) != 0 {
		t.Error("nothing may be installed when the arch check fails")
	}
}

func TestEnsureScionRejectsNoModeNamingAllThreeKeys(t *testing.T) {
	// Fails on the config alone: no guest round-trip, so it also works when the
	// machine is not up.
	f := exec.NewFakeRunner()
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-jail"}, RootPrefix: []string{"orb"}, Machine: "lever-jail"}

	err := g.EnsureScion(context.Background(), ScionSpec{})
	if err == nil {
		t.Fatal("expected an error when no scion mode is configured")
	}
	for _, want := range []string{"binary", "source", "version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the %q key", err, want)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("a config error must not touch the guest; got %+v", f.Calls)
	}
}

func TestInstallIfChangedHashesTheGuestBinaryNotAMarker(t *testing.T) {
	// Pin the mechanism: lever must ask the guest what is actually installed.
	// A marker file recording a past install would still match after the binary
	// was deleted, truncated or replaced, and lever would skip — leaving the
	// guest with no working scion and no way for `lever up` to repair it.
	local, sum := stageBinary(t, "scion-bytes")
	shape := prefixShapes("lever-jail")[0]
	f := exec.NewFakeRunner()
	f.Script(strings.Join(shape.userPrefix, " ")+" /usr/bin/sha256sum", exec.Result{Stdout: sum + "  /usr/local/bin/scion\n"})
	g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-jail"}

	if _, err := g.InstallRootBinaryIfChanged(context.Background(), local, "/usr/local/bin/scion"); err != nil {
		t.Fatalf("InstallRootBinaryIfChanged: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want exactly one probe call and no install, got %+v", f.Calls)
	}
	probe := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(probe, "sha256sum /usr/local/bin/scion") {
		t.Errorf("probe %q must hash the installed binary itself", probe)
	}
	// No marker file may be written: nothing to go stale, no ordering to defend.
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), ".sha256") {
			t.Errorf("no marker file may be written; got %+v", c)
		}
	}
}

// aptPrereqScript returns the root apt-prereq script EnsureRuntimes runs first.
func aptPrereqScript(t *testing.T) string {
	t.Helper()
	shape := prefixShapes("lever-x")[0]
	f := exec.NewFakeRunner()
	f.Script(strings.Join(shape.rootPrefix, " "), exec.Result{})
	f.Script(strings.Join(shape.userPrefix, " "), exec.Result{})
	g := Guest{Host: f, UserPrefix: shape.userPrefix, RootPrefix: shape.rootPrefix, Machine: "lever-x"}
	if err := g.EnsureRuntimes(context.Background(), "stephen"); err != nil {
		t.Fatalf("EnsureRuntimes: %v", err)
	}
	return f.Calls[0].Args[len(f.Calls[0].Args)-1]
}

// pkgsAfter reads the package list that follows marker in a shell command,
// stopping at the first token that is shell syntax rather than a package name.
func pkgsAfter(t *testing.T, script, marker string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(script, marker)
	if !ok {
		t.Fatalf("script does not contain %q: %s", marker, script)
	}
	var pkgs []string
	for _, tok := range strings.Fields(rest) {
		if i := strings.IndexAny(tok, ">;&|{}"); i >= 0 {
			if i > 0 {
				pkgs = append(pkgs, tok[:i])
			}
			break
		}
		pkgs = append(pkgs, tok)
	}
	return pkgs
}

// TestAptPrereqsGuardMatchesInstallList: the dpkg presence guard and the
// apt-get install list must name exactly the same packages.
//
// A package in the install list but missing from the guard never reaches a
// guest that already has the others — the guard passes and apt is skipped. A
// package in the guard but missing from the install list is worse: the guard
// can never pass, so every EnsureUp re-runs apt, which (see the comment above
// the script) does not merely cost time — once lever's egress chain is up,
// apt-get update cannot resolve the mirrors and hangs.
func TestAptPrereqsGuardMatchesInstallList(t *testing.T) {
	script := aptPrereqScript(t)
	guard := pkgsAfter(t, script, "dpkg -s ")
	install := pkgsAfter(t, script, "apt-get install -y -qq ")
	if len(guard) == 0 {
		t.Fatal("parsed no packages out of the dpkg guard")
	}
	if !slices.Equal(guard, install) {
		t.Fatalf("guard and install lists differ:\n guard   = %v\n install = %v", guard, install)
	}
}

// TestAptPrereqsDeclareLeverToolDependencies: lever runs these inside the jail
// itself, so a base image that stopped shipping one would break a lever feature
// with no other signal. curl carries every hub call (internal/hubapi/
// jailcurl.go); netcat-openbsd is how the remote-access proxy dials the hub
// through the jail (internal/remoteproxy/jaildial.go).
func TestAptPrereqsDeclareLeverToolDependencies(t *testing.T) {
	guard := pkgsAfter(t, aptPrereqScript(t), "dpkg -s ")
	for _, pkg := range []string{"curl", "netcat-openbsd"} {
		if !slices.Contains(guard, pkg) {
			t.Errorf("%q is not declared in the guest prereqs (%v) — lever runs it in the jail", pkg, guard)
		}
	}
}
