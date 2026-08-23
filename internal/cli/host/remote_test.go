package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func TestRemoteCommandWired(t *testing.T) {
	root := newRootWith(defaultFactory)
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "remote" {
			found = true
			subs := map[string]bool{}
			for _, s := range c.Commands() {
				subs[s.Name()] = true
			}
			if !subs["serve"] || !subs["status"] {
				t.Fatalf("remote subcommands = %v, want serve+status", subs)
			}
		}
	}
	if !found {
		t.Fatal("`lever remote` not wired into the host root")
	}
}

func TestRemoteServeDisabledErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, instanceYAML("x", ""))
	t.Chdir(dir)

	cmd := newRemoteServeCmd(defaultFactory)
	_, err := clitest.Exec(t, cmd, p)
	if err == nil {
		t.Fatal("remote serve with remote disabled should error")
	}
	if !strings.Contains(err.Error(), "remote.enabled") {
		t.Errorf("error should mention remote.enabled, got: %v", err)
	}
}

func TestRemoteServeLimaBackendErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, "name: x\nbackend: lima\ntree: workspace\nbroker:\n  llm_auth: subscription\nremote:\n  enabled: true\n")
	t.Chdir(dir)

	cmd := newRemoteServeCmd(defaultFactory)
	_, err := clitest.Exec(t, cmd, p)
	if err == nil {
		t.Fatal("remote serve on the lima backend should error")
	}
	if !strings.Contains(err.Error(), "orbstack") {
		t.Errorf("error should mention orbstack, got: %v", err)
	}
	// The gate survives, its old reason does not: the proxy now dials through
	// the jail, so guest→host forwarding has nothing to do with why Lima is
	// excluded. Repeating the obsolete rationale would send an operator
	// hunting for a forwarding problem that cannot exist.
	if strings.Contains(err.Error(), "forwarding") {
		t.Errorf("error must not cite guest→host forwarding as the reason, got: %v", err)
	}
}

func TestRemoteServeOrbstackEnabledPassesGates(t *testing.T) {
	// This does not exercise the real serve loop (that needs a live jail +
	// signal handling); it only proves the gate checks let a valid
	// orbstack+enabled config through to the point where Serve would be
	// invoked, by using a port that's already bound so Serve fails fast
	// with a bind error rather than blocking forever.
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, instanceYAML("x", "remote:\n  enabled: true\n  port: 1\n  base_url: \"https://mac.tail1234.ts.net\"\n"))
	t.Chdir(dir)

	cmd := newRemoteServeCmd(defaultFactory)
	_, err := clitest.Exec(t, cmd, p)
	if err == nil {
		t.Fatal("expected a bind error on privileged port 1 (proves the gates passed and Serve was reached)")
	}
	if strings.Contains(err.Error(), "remote.enabled") || strings.Contains(err.Error(), "orbstack") {
		t.Fatalf("gate checks should have passed; got a gate error instead of a bind error: %v", err)
	}
}

// freeRemotePort claims a loopback port from the kernel and releases it, so a
// serve test binds something free rather than the real 8445/8447 a live
// instance on this host may be holding.
func freeRemotePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestRemoteServeStampsTheConfigItLoaded is the "stale but matching" defect,
// end to end. `lever apply` reuses a live proxy when remote.pid names a live
// process, the port answers, and the state dir's stamp matches the config it
// is applying — but this command is reachable by hand, remote.pid is written
// by every serve, and the stamp used to be written only by apply. The state
// dir is keyed on the config file's DIRECTORY, so a second config beside
// lever.yaml shares both files: serving it used to inherit apply's stamp, and
// apply then reported success while this process enforced the other config's
// allowed_users.
//
// So `remote serve` must stamp what IT loaded, replacing any record it did not
// write.
func TestRemoteServeStampsTheConfigItLoaded(t *testing.T) {
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, instanceYAML("x", fmt.Sprintf(
		"remote:\n  enabled: true\n  port: %d\n  login_port: %d\n  base_url: \"https://mac.tail1234.ts.net\"\n",
		freeRemotePort(t), freeRemotePort(t))))
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The record a previous apply left, for a config this serve knows nothing
	// about. remote.pid names this process because the command under test runs
	// in it — exactly the file a hand-started proxy takes over.
	if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteRemoteStamp("v-previous-apply", "hash-of-another-config"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := newRemoteServeCmd(defaultFactory)
	cmd.SetArgs([]string{p})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	app, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := state.RemoteConfigHash(app)
	deadline := time.Now().Add(5 * time.Second)
	for !st.RemoteStampMatches(cli.VersionString(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("serve never recorded the config it loaded; output: %s", out.String())
		}
		select {
		case err := <-done:
			t.Fatalf("serve exited early (%v); output: %s", err, out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if st.RemoteStampMatches("v-previous-apply", "hash-of-another-config") {
		t.Error("the previous apply's record survived — apply would reuse this proxy as if it were serving that config")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("remote serve returned %v on shutdown; output: %s", err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote serve did not return after ctx cancel")
	}
}

// base_url unset is now reachable only with remote disabled (or unconfigured
// entirely) — `remote.enabled: true` with no base_url is rejected at config
// load (see config.TestRemoteBaseURLRequiredWhenEnabled). `remote status`
// itself doesn't branch on RemoteEnabled(), so a disabled/unconfigured
// instance exercises the exact same "nothing set up yet" fields this test
// checks.
func TestRemoteStatusNotRunningNoConfig(t *testing.T) {
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, instanceYAML("x", ""))
	t.Chdir(dir)

	cmd := newRemoteStatusCmd()
	out, err := clitest.Exec(t, cmd, p)
	if err != nil {
		t.Fatalf("remote status: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("output should report not running, got: %q", out)
	}
	if !strings.Contains(out, "tailscale serve --bg --https=443 http://127.0.0.1:8445") {
		t.Errorf("output should print the tailscale serve command with the effective port, got: %q", out)
	}
	if !strings.Contains(out, "base_url") {
		t.Errorf("output should mention base_url guidance when unset, got: %q", out)
	}
	if !strings.Contains(out, "PAT: absent") {
		t.Errorf("output should report PAT absent, got: %q", out)
	}
}

func TestRemoteStatusShowsBaseURLWhenSet(t *testing.T) {
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, instanceYAML("x", "remote:\n  enabled: true\n  base_url: \"https://mac.tail1234.ts.net\"\n"))
	t.Chdir(dir)

	cmd := newRemoteStatusCmd()
	out, err := clitest.Exec(t, cmd, p)
	if err != nil {
		t.Fatalf("remote status: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "https://mac.tail1234.ts.net") {
		t.Errorf("output should print the configured base_url, got: %q", out)
	}
}

func TestRemoteStatusNeverPrintsPATValue(t *testing.T) {
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, instanceYAML("x", "remote:\n  enabled: true\n  base_url: \"https://mac.tail1234.ts.net\"\n"))
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := "scion_pat_super_secret_value"
	if err := st.SaveRemotePAT(secret); err != nil {
		t.Fatal(err)
	}

	cmd := newRemoteStatusCmd()
	out, err := clitest.Exec(t, cmd, p)
	if err != nil {
		t.Fatalf("remote status: %v\noutput: %s", err, out)
	}
	if strings.Contains(out, secret) {
		t.Fatal("remote status output must never contain the PAT value")
	}
	if !strings.Contains(out, "PAT: present") {
		t.Errorf("output should report PAT present, got: %q", out)
	}
}

func TestRemoteStatusReportsLivePidAndListener(t *testing.T) {
	dir := t.TempDir()
	p := writeInstanceInto(t, dir, instanceYAML("x", "remote:\n  enabled: true\n  port: 0\n  base_url: \"https://mac.tail1234.ts.net\"\n"))
	t.Chdir(dir)

	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Record THIS test process's own pid (always alive) so the liveness
	// check reports alive without needing a real proxy process.
	if err := os.WriteFile(st.RemotePID(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRemoteStatusCmd()
	out, err := clitest.Exec(t, cmd, p)
	if err != nil {
		t.Fatalf("remote status: %v\noutput: %s", err, out)
	}
	if strings.Contains(out, "not running") {
		t.Errorf("output should not report not-running when the pid is alive, got: %q", out)
	}
	if !strings.Contains(out, strconv.Itoa(os.Getpid())) {
		t.Errorf("output should include the recorded pid, got: %q", out)
	}
}

// TestJailPrefixFnResolvesAndCaches: the proxy dials the hub through this
// instance's jail, so it needs the backend's transport prefix — with the
// machine's own run user in it, which is what makes two remote-enabled
// instances on one host reach different hubs. Resolving costs commands into
// the machine, so a success must be reused rather than re-probed per
// connection.
func TestJailPrefixFnResolvesAndCaches(t *testing.T) {
	sb := &stubBackend{}
	var calls int
	bf := func(_, machine string) (backend.Backend, error) {
		calls++
		if machine != "lever-x" {
			t.Errorf("factory got machine %q, want lever-x", machine)
		}
		return sb, nil
	}
	fn := jailPrefixFn(bf, "orbstack", "lever-x", io.Discard)

	want := []string{"orb", "-m", "lever-x", "-u", "stub"}
	if got := fn(); !slices.Equal(got, want) {
		t.Fatalf("prefix = %v, want %v", got, want)
	}
	if got := fn(); !slices.Equal(got, want) {
		t.Fatalf("second prefix = %v, want %v", got, want)
	}
	if calls != 1 {
		t.Fatalf("backend resolved %d times, want 1 (a cached prefix, not a probe per dial)", calls)
	}
}

// TestJailPrefixFnRetriesAfterFailure: a jail that is down when the proxy
// starts must not wedge it permanently. The failed resolve returns nil (which
// JailDial turns into an actionable dial error), warns once rather than once
// per connection, and the next dial after the jail comes up succeeds.
func TestJailPrefixFnRetriesAfterFailure(t *testing.T) {
	sb := &stubBackend{resolveRunUserErr: errors.New("machine \"lever-x\" is not running")}
	var warn bytes.Buffer
	fn := jailPrefixFn(func(string, string) (backend.Backend, error) { return sb, nil }, "orbstack", "lever-x", &warn)

	if got := fn(); got != nil {
		t.Fatalf("prefix with the jail down = %v, want nil", got)
	}
	if got := fn(); got != nil {
		t.Fatalf("prefix with the jail still down = %v, want nil", got)
	}
	if n := strings.Count(warn.String(), "not running"); n != 1 {
		t.Fatalf("warned %d times about the same failure, want 1 (remote.log must not flood)", n)
	}
	if !strings.Contains(warn.String(), "lever-x") {
		t.Errorf("warning should name the machine, got: %q", warn.String())
	}

	sb.resolveRunUserErr = nil
	if got := fn(); !slices.Equal(got, []string{"orb", "-m", "lever-x", "-u", "stub"}) {
		t.Fatalf("prefix after the jail came up = %v, want a resolved prefix", got)
	}
}

// TestJailPrefixFnUnknownBackendFails: an unresolvable backend must fail the
// dial, never fall back to a half-built argv that would exec something
// unintended on the host.
func TestJailPrefixFnUnknownBackendFails(t *testing.T) {
	var warn bytes.Buffer
	fn := jailPrefixFn(func(string, string) (backend.Backend, error) { return &stubBackend{}, nil }, "nope", "lever-x", &warn)
	if got := fn(); got != nil {
		t.Fatalf("prefix for an unknown backend = %v, want nil", got)
	}
	if !strings.Contains(warn.String(), "unknown backend") {
		t.Errorf("warning should name the cause, got: %q", warn.String())
	}
}

// TestJailPrefixFnConcurrentDialsShareOneAttempt: a browser opens several
// connections at once. If each queued behind the previous one's probe, a
// wedged machine would hold the second caller for two resolve timeouts, the
// third for three — the opposite of the fail-the-dial-promptly behavior
// jailResolveTimeout exists to guarantee. They must share one attempt.
func TestJailPrefixFnConcurrentDialsShareOneAttempt(t *testing.T) {
	release := make(chan struct{})
	var probes atomic.Int32
	blocking := &blockingBackend{onResolve: func() {
		probes.Add(1)
		<-release
	}}
	fn := jailPrefixFn(func(string, string) (backend.Backend, error) { return blocking, nil }, "orbstack", "lever-x", io.Discard)

	const dials = 8
	got := make(chan []string, dials)
	for range dials {
		go func() { got <- fn() }()
	}
	// Let the first caller reach the (blocked) probe before releasing, so the
	// others genuinely arrive while it is in flight.
	for probes.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)

	want := []string{"orb", "-m", "lever-x", "-u", "stub"}
	for range dials {
		select {
		case prefix := <-got:
			if !slices.Equal(prefix, want) {
				t.Fatalf("prefix = %v, want %v", prefix, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a concurrent dial never got an answer")
		}
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("probed the machine %d times for %d concurrent dials, want 1", n, dials)
	}
}

// blockingBackend lets a test hold ResolveRunUser open, standing in for a
// machine that answers slowly or not at all.
type blockingBackend struct {
	stubBackend
	onResolve func()
}

func (b *blockingBackend) ResolveRunUser(context.Context) error {
	b.onResolve()
	return nil
}
