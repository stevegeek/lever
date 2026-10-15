package host

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/conduit"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

func sshKeyBlob(typ string, rest ...byte) string {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(typ)))
	b = append(b, typ...)
	b = append(b, rest...)
	return base64.StdEncoding.EncodeToString(b)
}

func TestParseSSHHostKey(t *testing.T) {
	good := "ssh-ed25519 " + sshKeyBlob("ssh-ed25519", 0, 0, 0, 32)
	for _, tc := range []struct {
		raw, want string
		err       bool
	}{
		{good, good, false},
		{good + " agent@host", good, false},
		{"  " + good + "\n", good, false},
		{"ssh-ed25519", "", true},
		{"ssh-dss " + sshKeyBlob("ssh-dss"), "", true},
		{"ssh-ed25519 " + sshKeyBlob("ssh-rsa"), "", true},
		{"ssh-ed25519 !!!", "", true},
		{"ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 200, 's'}), "", true},
		{good + "\n* " + good, "", true},
		{good + " a b", "", true},
		{"@cert-authority * " + good, "", true},
	} {
		got, err := parseSSHHostKey(tc.raw)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("%q: = %q, %v; want %q err=%v", tc.raw, got, err, tc.want, tc.err)
		}
	}
}

func TestPinSSHHostKeyReplacesOnlyTheAlias(t *testing.T) {
	f := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(f, []byte("lever.i.manager ssh-ed25519 OLD\nlever.i.w ssh-ed25519 W\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pinSSHHostKey(f, "lever.i.manager", "ssh-ed25519 NEW"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	if string(b) != "lever.i.w ssh-ed25519 W\nlever.i.manager ssh-ed25519 NEW\n" {
		t.Fatalf("known_hosts = %q", b)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestSSHProxyCommandQuotes(t *testing.T) {
	pc, err := sshProxyCommand("/opt/le ver/bin/lever", "/home/o/it's 100%/lever.yaml", "manager")
	if err != nil {
		t.Fatal(err)
	}
	want := `'/opt/le ver/bin/lever' ssh --proxy --config '/home/o/it'\''s 100%%/lever.yaml' -- manager`
	if pc != want {
		t.Fatalf("ProxyCommand = %s\nwant          %s", pc, want)
	}
	for _, bad := range []string{"lever", "/a\nb", "/a\x1bb"} {
		if _, err := sshProxyCommand(bad, "/c/lever.yaml", "m"); err == nil {
			t.Errorf("%q accepted as the lever path", bad)
		}
	}
}

func sshTestApp(t *testing.T) (*config.App, state.State, string) {
	t.Helper()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	app := &config.App{Name: "inst", Tree: tree, SSH: config.SSH{Enabled: true}, Workers: []config.Worker{{Name: "w1"}}}
	return app, state.ForConfig(dir), filepath.Join(dir, "lever.yaml")
}

func TestSSHClientArgvKeepsTheOperatorsMachineOut(t *testing.T) {
	app, st, cfg := sshTestApp(t)
	leverExecutable = func() (string, error) { return "/usr/local/bin/lever", nil }
	defer func() { leverExecutable = defaultLeverExecutable }()
	argv, err := sshClientArgv(app, st, cfg, "w1", []string{"uname", "-a"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"-F\x00none", "ForwardAgent=no", "ForwardX11=no", "ClearAllForwardings=yes", "PermitLocalCommand=no",
		"StrictHostKeyChecking=yes", "UpdateHostKeys=no", "SendEnv=-*", "IdentityAgent=none", "PasswordAuthentication=no", "GlobalKnownHostsFile=/dev/null", "HostKeyAlias=lever.inst.w1",
		"UserKnownHostsFile=\"" + st.SSHKnownHosts() + "\"", "ControlPath=none", "BatchMode=yes",
		"ProxyCommand='/usr/local/bin/lever' ssh --proxy --config '" + cfg + "' -- w1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %q", want, argv)
		}
	}
	if !slices.Equal(argv[len(argv)-3:], []string{"lever.inst.w1", "uname", "-a"}) {
		t.Fatalf("argv tail = %q", argv[len(argv)-3:])
	}
	if _, err := sshClientArgv(app, st, cfg, "w1", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSSHConfigOutput(t *testing.T) {
	app, st, cfg := sshTestApp(t)
	var b strings.Builder
	if err := writeSSHConfig(&b, app, st, "/usr/local/bin/lever", cfg, []string{"inst", "w1"}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "# lever ssh: put these blocks above any `Host *` block") || !strings.Contains(out, "Host * !lever.*") {
		t.Errorf("output does not say where the blocks go:\n%s", out)
	}
	for _, want := range []string{"Host lever.inst.inst\n", "Host lever.inst.w1\n", "  ForwardAgent no\n", "  ForwardX11 no\n",
		"  StrictHostKeyChecking yes\n", "  SendEnv -*\n", "  IdentityAgent none\n", "  PubkeyAuthentication no\n", "  KbdInteractiveAuthentication no\n", "  UserKnownHostsFile \"" + st.SSHKnownHosts() + "\"\n",
		"  ProxyCommand '/usr/local/bin/lever' ssh --proxy --config '" + cfg + "' -- w1\n",
		`#   "remote.SSH.localServerDownload": "always",`, `#   "remote.autoForwardPorts": false,`,
		`#   "remote.SSH.enableAgentForwarding": false,`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if err := writeSSHConfig(&b, app, st, "/usr/local/bin/lever", cfg, []string{"nobody"}); err == nil {
		t.Fatal("an unknown agent got a Host block")
	}
}

func TestSSHAllowed(t *testing.T) {
	app, st, _ := sshTestApp(t)
	if err := sshAllowed(app, st); err != nil {
		t.Fatal(err)
	}
	app.SSH.Enabled = false
	if err := sshAllowed(app, st); err == nil || !strings.Contains(err.Error(), "ssh.enabled") {
		t.Fatalf("ssh off: %v", err)
	}
	app.SSH.Enabled = true
	app.Tree = filepath.Dir(st.Dir)
	if err := sshAllowed(app, st); err == nil || !strings.Contains(err.Error(), "inside the tree") {
		t.Fatalf("state in the tree: %v", err)
	}
}

type fakeConduitScion struct {
	controls bool
	err      error
	hub      conduit.LiveHub
	hubErr   error
}

func (f fakeConduitScion) ConduitControls(context.Context) (bool, error) { return f.controls, f.err }
func (f fakeConduitScion) LiveHub(context.Context) (conduit.LiveHub, error) {
	return f.hub, f.hubErr
}

func TestConduitReadyRefuses(t *testing.T) {
	app, st, _ := sshTestApp(t)
	live := conduit.LiveHub{Running: true, Env: apply.HubEnv("i", "fp", app, true)}
	recordOn := func(on bool) {
		rec := state.HubExperimentsRecord{Off: []string{"hub.artifacts"}}
		if on {
			rec.On = []string{conduitExperiment}
		}
		if err := st.SaveHubExperimentsRecord(rec); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := conduitReady(ctx, app, st, fakeConduitScion{controls: false}); !errors.Is(err, errNoConduitControls) {
		t.Fatalf("no controls: %v", err)
	}
	if err := conduitReady(ctx, app, st, fakeConduitScion{err: conduit.ErrProbe}); err == nil {
		t.Fatal("an unanswered probe passed")
	}
	if err := conduitReady(ctx, app, st, fakeConduitScion{controls: true, hub: live}); err == nil || !strings.Contains(err.Error(), "not turned") {
		t.Fatalf("no record: %v", err)
	}
	recordOn(false)
	if err := conduitReady(ctx, app, st, fakeConduitScion{controls: true, hub: live}); err == nil {
		t.Fatal("conduit recorded off passed")
	}
	recordOn(true)
	weaker := conduit.LiveHub{Running: true, Env: conduit.Policy{DenyPorts: []int{22}, DisableSSHSocketForwarding: true}.HubEnv()}
	for _, h := range []conduit.LiveHub{{}, {Running: true, Env: apply.HubAuthEnv("i", "fp")}, weaker} {
		if err := conduitReady(ctx, app, st, fakeConduitScion{controls: true, hub: h}); err == nil {
			t.Fatalf("hub %+v passed", h)
		}
	}
	if err := conduitReady(ctx, app, st, fakeConduitScion{controls: true, hub: live}); err != nil {
		t.Fatal(err)
	}
}

// imageRunner answers the image policy script per image.
type imageRunner struct {
	proc.Runner
	codes map[string]int
	seen  []string
}

func (r *imageRunner) Run(_ context.Context, _ map[string]string, name string, args ...string) (proc.Result, error) {
	if name != "sh" || len(args) != 4 || args[1] != imagePolicyScript {
		return proc.Result{Code: 1}, errors.New("unexpected call")
	}
	img := args[3]
	r.seen = append(r.seen, img)
	if c := r.codes[img]; c != 0 {
		return proc.Result{Code: c}, errors.New("exit")
	}
	return proc.Result{}, nil
}

func TestSSHGate(t *testing.T) {
	app, _, _ := sshTestApp(t)
	app.Manager.Image = "lever.local/m:1"
	app.Workers = []config.Worker{{Name: "w1"}, {Name: "w2", Image: "lever.local/w:1"}}
	ctx := context.Background()

	g := newSSHGate(ctx, app, fakeConduitScion{controls: false}, &imageRunner{})
	if g.on || g.controls || g.why != errNoConduitControls.Error() {
		t.Fatalf("no controls: %+v", g)
	}
	g = newSSHGate(ctx, app, fakeConduitScion{err: conduit.ErrProbe}, &imageRunner{})
	if g.on || g.controls || !strings.Contains(g.why, "conduit stays off") {
		t.Fatalf("unanswered probe: %+v", g)
	}
	for code, want := range map[int]string{3: "does not hold", 4: "no sciontool", 5: "does not enforce", 1: "could not check"} {
		r := &imageRunner{codes: map[string]int{"lever.local/w:1": code}}
		g = newSSHGate(ctx, app, fakeConduitScion{controls: true}, r)
		if g.on || !g.controls || !strings.Contains(g.why, want) {
			t.Fatalf("image exit %d: %+v", code, g)
		}
	}
	r := &imageRunner{}
	g = newSSHGate(ctx, app, fakeConduitScion{controls: true}, r)
	if !g.on || g.why != "" || !slices.Equal(r.seen, []string{"lever.local/m:1", "lever.local/w:1"}) {
		t.Fatalf("all good: %+v, images %v", g, r.seen)
	}
	app.SSH.Enabled = false
	r = &imageRunner{}
	g = newSSHGate(ctx, app, fakeConduitScion{controls: true}, r)
	if g.on || !g.controls || g.why != "" || len(r.seen) != 0 {
		t.Fatalf("ssh off: %+v, images %v", g, r.seen)
	}
}

func TestImagePolicyScriptShape(t *testing.T) {
	for _, want := range []string{"--network=none", "--pull=never", jail.ScionKeepIDUserns, "grep -qaF " + conduit.AgentPolicyEnv} {
		if !strings.Contains(imagePolicyScript, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}

// A probe that did not answer keeps the policy env of a hub lever started
// with it.
func TestSSHGateKeepsThePolicyWhenTheProbeFails(t *testing.T) {
	app, _, _ := sshTestApp(t)
	app.SSH.Enabled = false
	live := conduit.LiveHub{Running: true, Env: apply.HubEnv("i", "fp", app, true)}
	g := newSSHGate(context.Background(), app, fakeConduitScion{err: conduit.ErrProbe, hub: live}, &imageRunner{})
	if !g.controls || g.on {
		t.Fatalf("gate = %+v", g)
	}
	g = newSSHGate(context.Background(), app, fakeConduitScion{err: conduit.ErrProbe, hub: conduit.LiveHub{Running: true}}, &imageRunner{})
	if g.controls {
		t.Fatalf("a hub without the policy env: %+v", g)
	}
}
