// Package contract holds the agent-image contract check (check-image.sh,
// in-image.sh). These tests keep it in step with lever's code and the image
// Dockerfiles, and exercise check-image.sh against a stand-in container CLI;
// running it against a real image needs docker or podman (make image-contract).
package contract

import (
	"bufio"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/wire"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScriptsParse(t *testing.T) {
	// The image's /bin/sh is dash; prefer it where present, since a bash
	// posing as sh accepts bashisms.
	posix := "sh"
	if _, err := exec.LookPath("dash"); err == nil {
		posix = "dash"
	}
	for _, c := range []struct{ shell, file string }{{posix, "in-image.sh"}, {"bash", "check-image.sh"}, {"bash", "../../cmd/lever-agent/scionhook/toolshell"}} {
		if _, err := exec.LookPath(c.shell); err != nil {
			t.Skipf("%s not found", c.shell)
		}
		if out, err := exec.Command(c.shell, "-n", c.file).CombinedOutput(); err != nil {
			t.Errorf("%s -n %s: %v\n%s", c.shell, c.file, err, out)
		}
	}
}

// Every path, user and command lever's code relies on in the agent container
// is checked by in-image.sh.
func TestInImageCoversLever(t *testing.T) {
	script := read(t, "in-image.sh")
	want := []string{
		"[ -d " + path.Dir(wire.ManagedSettingsPath) + " ]",
		"marker=" + wire.ReadyMarkerPath + "\n",
		"ts=" + wire.ToolShellPath + "\n",
		"'" + wire.ReadyHookCommand + ";",
		`[ "$(id -un)" = ` + jail.AgentUser + " ]",
		"[ -d " + path.Dir(jail.AgentTokenPath) + " ]",
		`[ "$pw_home" = ` + path.Dir(path.Dir(jail.AgentSessionsDir)) + " ]",
		"list-panes -t scion:agent",
		"sciontool version",
		"tmux -V",
	}
	for _, w := range want {
		if !strings.Contains(script, w) {
			t.Errorf("in-image.sh does not check %q", w)
		}
	}
}

// What the lever layer bakes in (its binaries, the hook, its ENV lines) is
// what in-image.sh checks for, so a change to the Dockerfile that the
// contract does not follow fails here rather than in a built image.
func TestInImageCoversLeverLayer(t *testing.T) {
	script := read(t, "in-image.sh")
	df := read(t, filepath.Join("..", "lever-claude", "Dockerfile"))
	copyRe := regexp.MustCompile(`^COPY\s+\S+\s+(\S+)$`)
	envRe := regexp.MustCompile(`^ENV\s+([A-Z_]+)=(\S+)$`)
	var copies, envs int
	sc := bufio.NewScanner(strings.NewReader(df))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := copyRe.FindStringSubmatch(line); m != nil {
			copies++
			dst := m[1]
			if strings.HasPrefix(dst, "/usr/local/bin/") {
				name := path.Base(dst)
				if !regexp.MustCompile(`for b in [^;]*\b` + regexp.QuoteMeta(name) + `\b`).MatchString(script) {
					t.Errorf("in-image.sh does not check the lever binary %s", dst)
				}
			} else if !strings.Contains(script, dst) {
				t.Errorf("in-image.sh does not check %s, which the Dockerfile copies in", dst)
			}
		}
		if m := envRe.FindStringSubmatch(line); m != nil {
			envs++
			if !strings.Contains(script, "expect_env "+m[1]+" "+m[2]+"\n") {
				t.Errorf("in-image.sh does not check ENV %s=%s", m[1], m[2])
			}
		}
	}
	if copies < 4 || envs < 4 {
		t.Fatalf("parsed %d COPY and %d ENV lines from the Dockerfile; the parser no longer matches it", copies, envs)
	}
}

// The slim base adds no Claude Code of its own (the lever layer pins it
// once), and keeps scion's entrypoint.
func TestSlimBaseShape(t *testing.T) {
	df := read(t, filepath.Join("..", "scion-slim", "Dockerfile"))
	if strings.Contains(df, "@anthropic-ai/claude-code") {
		t.Error("the slim base installs Claude Code; the lever layer installs it once at its pin")
	}
	if !strings.Contains(df, `ENTRYPOINT ["sciontool", "init", "--"]`) {
		t.Error("the slim base does not keep scion's entrypoint")
	}
	if !strings.Contains(read(t, filepath.Join("..", "lever-claude", "Dockerfile")), "FROM ${SCION_BASE}") {
		t.Error("the lever layer no longer builds FROM ${SCION_BASE}; the slim flavor depends on it")
	}
}

// fakeCLI is a stand-in for docker: `image inspect --format F IMAGE` answers
// from the env (FAKE_<key>), `run` records its args and stdin and exits
// FAKE_RUN_RC.
const fakeCLI = `#!/bin/sh
case "$1" in
image)
  [ "$5" = "$FAKE_IMAGE" ] || exit 1
  case "$4" in
  '{{.Id}}') echo sha256:feed ;;
  '{{.Architecture}}') echo "$FAKE_ARCH" ;;
  '{{json .Config.Entrypoint}}') echo "$FAKE_ENTRYPOINT" ;;
  '{{.Config.User}}') echo "$FAKE_USER" ;;
  '{{.Size}}') echo "$FAKE_SIZE" ;;
  *claude_code_version*) echo "${FAKE_CLAUDE:-<no value>}" ;;
  *tmux_version*) echo 3.6b-1~bpo13+1 ;;
  *lever_jail_abi*) echo "${FAKE_ABI:-<no value>}" ;;
  *) echo '<no value>' ;;
  esac
  ;;
run)
  shift
  printf '%s\n' "$@" > "$FAKE_DIR/run-args"
  cat > "$FAKE_DIR/run-stdin"
  exit "${FAKE_RUN_RC:-0}"
  ;;
esac
`

func TestCheckImageWithFakeCLI(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "fakecli")
	if err := os.WriteFile(cli, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	const img = "lever.local/lever-claude-slim:arm64"
	good := map[string]string{
		"FAKE_IMAGE":      img,
		"FAKE_ARCH":       "arm64",
		"FAKE_ENTRYPOINT": `["sciontool","init","--"]`,
		"FAKE_USER":       "scion",
		"FAKE_SIZE":       "900000000",
		"FAKE_CLAUDE":     "2.1.296",
		"FAKE_ABI":        "3",
	}
	run := func(t *testing.T, img string, over map[string]string) (string, int) {
		t.Helper()
		cmd := exec.Command("bash", "check-image.sh", img)
		env := append(os.Environ(), "CONTAINER_CLI="+cli, "FAKE_DIR="+dir, "LEVER_IMAGE_ARCH=", "LEVER_JAIL_ABI=", "LEVER_IMAGE_MAX_MB=", "GIT_MIN_VERSION=")
		for k, v := range good {
			env = append(env, k+"="+v)
		}
		for k, v := range over {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}

	out, code := run(t, img, nil)
	if code != 0 || !strings.Contains(out, "contract: "+img+" passes") {
		t.Fatalf("a good image: exit %d\n%s", code, out)
	}
	args := read(t, filepath.Join(dir, "run-args"))
	for _, w := range []string{"--rm\n", "--network\nnone\n", "--entrypoint\n/bin/sh\n", img + "\n-s\n--\narm64\n2.1.296\n3.6b-1~bpo13+1\n2.47.0\n"} {
		if !strings.Contains(args, w) {
			t.Errorf("run args lack %q:\n%s", w, args)
		}
	}
	if read(t, filepath.Join(dir, "run-stdin")) != read(t, "in-image.sh") {
		t.Error("the container did not get in-image.sh on stdin")
	}

	fails := []struct {
		name string
		over map[string]string
		img  string
		want string
	}{
		{"entrypoint", map[string]string{"FAKE_ENTRYPOINT": `["bash"]`}, img, "FAIL  entrypoint"},
		{"user", map[string]string{"FAKE_USER": "root"}, img, "FAIL  image user"},
		{"arch", map[string]string{"FAKE_ARCH": "amd64"}, img, "FAIL  architecture"},
		{"no claude label", map[string]string{"FAKE_CLAUDE": ""}, img, "FAIL  no claude_code_version"},
		{"no abi label", map[string]string{"FAKE_ABI": ""}, img, "FAIL  no lever_jail_abi"},
		{"abi mismatch", map[string]string{"LEVER_JAIL_ABI": "4"}, img, "FAIL  lever_jail_abi is 3"},
		{"too big", map[string]string{"LEVER_IMAGE_MAX_MB": "800"}, img, "FAIL  size 900 MB"},
		{"in-image failure", map[string]string{"FAKE_RUN_RC": "1"}, img, "in-image exit 1"},
	}
	for _, f := range fails {
		t.Run(f.name, func(t *testing.T) {
			out, code := run(t, f.img, f.over)
			if code != 1 || !strings.Contains(out, f.want) || !strings.Contains(out, "FAILS") {
				t.Fatalf("exit %d, want 1 with %q:\n%s", code, f.want, out)
			}
		})
	}

	if out, code := run(t, "lever.local/other:arm64", nil); code != 2 {
		t.Fatalf("a missing image: exit %d, want 2\n%s", code, out)
	}
}

// fakeDocker stands in for docker under build-lever-image.sh: it records each
// build's argv as one line, and knows an image when FAKE_EXISTING names it or
// a build tagged it.
const fakeDocker = `#!/bin/sh
log="$FAKE_DIR/docker.log"
known="$FAKE_DIR/known"
touch "$known"
case "$1" in
image)
  grep -qxF "$3" "$known" && { [ "$4" = --format ] && echo "$FAKE_ARCH"; exit 0; }
  for i in $FAKE_EXISTING; do [ "$i" = "$3" ] && exit 0; done
  exit 1
  ;;
build)
  echo "$*" >> "$log"
  prev=
  for a in "$@"; do
    [ "$prev" = -t ] && echo "$a" >> "$known"
    prev=$a
  done
  ;;
esac
`

func TestBuildScriptFlavors(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	type result struct {
		out    string
		code   int
		builds []string
	}
	run := func(t *testing.T, flavor string, existing string, stageSlim bool, slimMachine byte) result {
		t.Helper()
		dir := t.TempDir()
		tree := filepath.Join(dir, "image")
		ctx := filepath.Join(tree, "lever-claude")
		slim := filepath.Join(tree, "scion-slim")
		for _, d := range []string{filepath.Join(ctx, "bin"), filepath.Join(ctx, "scionhook"), slim, filepath.Join(dir, "path")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		script := read(t, filepath.Join("..", "lever-claude", "build-lever-image.sh"))
		if err := os.WriteFile(filepath.Join(ctx, "build-lever-image.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if stageSlim {
			if err := os.MkdirAll(filepath.Join(slim, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			// An ELF header far enough to name its machine (byte 18).
			hdr := make([]byte, 20)
			copy(hdr, "\x7fELF")
			hdr[18] = slimMachine
			for _, b := range []string{"sciontool", "scion"} {
				if err := os.WriteFile(filepath.Join(slim, "bin", b), hdr, 0o755); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "path", "docker"), []byte(fakeDocker), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", filepath.Join(ctx, "build-lever-image.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+filepath.Join(dir, "path")+string(os.PathListSeparator)+os.Getenv("PATH"),
			"FAKE_DIR="+dir, "FAKE_ARCH=arm64", "FAKE_EXISTING="+existing,
			"LEVER_IMAGE_ARCH=arm64", "LEVER_IMAGE_FLAVOR="+flavor, "LEVER_IMAGE_FORCE=",
			"LEVER_IMAGE_REGISTRY=", "SCION_VERSION=65531d7f", "LEVER_JAIL_ABI=3", "LEVER_IMAGE_VERSION=0.37.0")
		out, err := cmd.CombinedOutput()
		r := result{out: string(out)}
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "docker.log")); err == nil {
			r.builds = strings.Split(strings.TrimSpace(string(b)), "\n")
		}
		return r
	}

	t.Run("slim builds its base, then the lever layer from it", func(t *testing.T) {
		r := run(t, "slim", "", true, 183)
		if r.code != 0 || len(r.builds) != 2 {
			t.Fatalf("exit %d, %d builds:\n%s\n%s", r.code, len(r.builds), r.out, strings.Join(r.builds, "\n"))
		}
		base, layer := r.builds[0], r.builds[1]
		for _, w := range []string{"--platform linux/arm64", "SCION_VERSION=65531d7f", "-t lever-scion-slim:arm64", "scion-slim/Dockerfile"} {
			if !strings.Contains(base, w) {
				t.Errorf("base build lacks %q: %s", w, base)
			}
		}
		for _, w := range []string{"--platform linux/arm64", "SCION_BASE=lever-scion-slim:arm64", "LEVER_JAIL_ABI=3", "-t lever-claude-slim:arm64", "-t lever.local/lever-claude-slim:arm64", "lever-claude/Dockerfile"} {
			if !strings.Contains(layer, w) {
				t.Errorf("lever layer build lacks %q: %s", w, layer)
			}
		}
	})
	t.Run("full is unchanged", func(t *testing.T) {
		r := run(t, "", "scion-claude:arm64", false, 0)
		if r.code != 0 || len(r.builds) != 1 {
			t.Fatalf("exit %d, %d builds:\n%s", r.code, len(r.builds), r.out)
		}
		for _, w := range []string{"SCION_BASE=scion-claude:arm64", "-t lever-claude:arm64", "-t lever.local/lever-claude:arm64"} {
			if !strings.Contains(r.builds[0], w) {
				t.Errorf("build lacks %q: %s", w, r.builds[0])
			}
		}
		if strings.Contains(r.builds[0], "--platform") {
			t.Errorf("the full flavor's build gained --platform: %s", r.builds[0])
		}
	})
	t.Run("slim without staged scion binaries", func(t *testing.T) {
		r := run(t, "slim", "", false, 0)
		if r.code == 0 || len(r.builds) != 0 || !strings.Contains(r.out, "make lever-image-slim") {
			t.Fatalf("exit %d, %d builds:\n%s", r.code, len(r.builds), r.out)
		}
	})
	t.Run("slim refuses to overwrite before building anything", func(t *testing.T) {
		for _, existing := range []string{"lever.local/lever-claude-slim:arm64"} {
			r := run(t, "slim", existing, true, 183)
			if r.code == 0 || len(r.builds) != 0 || !strings.Contains(r.out, existing+" already exists") {
				t.Fatalf("%s: exit %d, %d builds:\n%s", existing, r.code, len(r.builds), r.out)
			}
		}
	})
	t.Run("slim with binaries staged for the other arch", func(t *testing.T) {
		r := run(t, "slim", "", true, 62)
		if r.code == 0 || len(r.builds) != 0 || !strings.Contains(r.out, "is amd64, not arm64") {
			t.Fatalf("exit %d, %d builds:\n%s", r.code, len(r.builds), r.out)
		}
	})
	t.Run("unknown flavor", func(t *testing.T) {
		r := run(t, "tiny", "", true, 183)
		if r.code == 0 || len(r.builds) != 0 {
			t.Fatalf("exit %d, %d builds:\n%s", r.code, len(r.builds), r.out)
		}
	})
}
