package guest

import (
	"context"
	"strings"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/exec"
	"testing"

	"gopkg.in/yaml.v3"
)

func testHubLogin() backend.HubLogin {
	return backend.HubLogin{IssuerPort: backend.GuestLoginIssuerPort, HostPort: 8447,
		HostAddress: "host.orb.internal", ClientID: "lever-remote"}
}

// unmarshalSettings decodes a rendered settings file for assertions.
func unmarshalSettings(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse rendered settings: %v\n%s", err, b)
	}
	return m
}

// oidcBlock digs out server.oidc_login.
func oidcBlock(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m := unmarshalSettings(t, b)
	server, ok := m["server"].(map[string]any)
	if !ok {
		t.Fatalf("no `server:` mapping in:\n%s", b)
	}
	block, ok := server["oidc_login"].(map[string]any)
	if !ok {
		t.Fatalf("no server.oidc_login in:\n%s", b)
	}
	return block
}

func TestHubLoginSettingsCreatesTheBlockInAnEmptyFile(t *testing.T) {
	out, changed, err := hubLoginSettings(nil, testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	if !changed {
		t.Fatal("changed = false for a file that had no oidc_login")
	}
	block := oidcBlock(t, out)
	// The issuer MUST be loopback: scion validates it at hub startup and
	// refuses to start on anything else (pkg/hub/oauth.go).
	if block["issuer_url"] != "http://127.0.0.1:8446" {
		t.Fatalf("issuer_url = %v", block["issuer_url"])
	}
	if block["enabled"] != true || block["client_id"] != "lever-remote" {
		t.Fatalf("block = %v", block)
	}
	// No secret is written: lever's provider is a public client.
	if _, ok := block["client_secret"]; ok {
		t.Fatalf("client_secret written into the settings file: %v", block)
	}
}

func TestHubLoginSettingsPreservesEverythingElse(t *testing.T) {
	existing := []byte(`# machine settings, hand annotated
version: 1
server:
  # the hub's own port
  hub:
    port: 8080
  user_access_mode: open
telemetry:
  enabled: false
`)
	out, changed, err := hubLoginSettings(existing, testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	if !changed {
		t.Fatal("changed = false")
	}
	m := unmarshalSettings(t, out)
	if m["version"] != 1 {
		t.Fatalf("version lost: %v", m)
	}
	tel, _ := m["telemetry"].(map[string]any)
	if tel == nil || tel["enabled"] != false {
		t.Fatalf("telemetry lost: %v", m)
	}
	server := m["server"].(map[string]any)
	if server["user_access_mode"] != "open" {
		t.Fatalf("server.user_access_mode lost: %v", server)
	}
	hub, _ := server["hub"].(map[string]any)
	if hub == nil || hub["port"] != 8080 {
		t.Fatalf("server.hub lost: %v", server)
	}
	// Comments survive, because the document is edited rather than rebuilt.
	if !strings.Contains(string(out), "# machine settings, hand annotated") ||
		!strings.Contains(string(out), "# the hub's own port") {
		t.Fatalf("comments were dropped:\n%s", out)
	}
}

// TestHubLoginSettingsIsIdempotent is what keeps a re-apply from restarting
// the hub — and every agent's connection to it — for no reason.
func TestHubLoginSettingsIsIdempotent(t *testing.T) {
	first, changed, err := hubLoginSettings([]byte("server:\n  user_access_mode: open\n"), testHubLogin(), false)
	if err != nil || !changed {
		t.Fatalf("first pass: changed=%v err=%v", changed, err)
	}
	second, changed, err := hubLoginSettings(first, testHubLogin(), false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if changed {
		t.Fatalf("changed = true on an unchanged config — the hub would be restarted on every apply:\n%s", second)
	}
	if string(second) != string(first) {
		t.Fatalf("second pass rewrote the file:\n%s\n---\n%s", first, second)
	}
}

func TestHubLoginSettingsDetectsADifferentPort(t *testing.T) {
	first, _, err := hubLoginSettings(nil, testHubLogin(), false)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	spec := testHubLogin()
	spec.IssuerPort = 9500
	out, changed, err := hubLoginSettings(first, spec, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !changed {
		t.Fatal("a changed provider port did not register as a config change")
	}
	if oidcBlock(t, out)["issuer_url"] != "http://127.0.0.1:9500" {
		t.Fatalf("issuer_url not updated: %v", oidcBlock(t, out))
	}
}

// TestHubLoginSettingsRefusesToStealFromLegacyServerYAML: once settings.yaml
// carries a `server:` key, scion reads the hub's WHOLE server configuration
// from it and ignores server.yaml (pkg/config/hub_config.go). Adding the key
// beside an unmigrated server.yaml would silently drop that file's contents.
func TestHubLoginSettingsRefusesToStealFromLegacyServerYAML(t *testing.T) {
	_, _, err := hubLoginSettings([]byte("version: 1\n"), testHubLogin(), true)
	if err == nil {
		t.Fatal("added a `server:` key beside a legacy server.yaml")
	}
	if !strings.Contains(err.Error(), "server.yaml") {
		t.Fatalf("error = %v", err)
	}
	// With the key already present, scion is ALREADY ignoring server.yaml, so
	// there is nothing left to lose and lever proceeds.
	if _, changed, err := hubLoginSettings([]byte("server:\n  user_access_mode: open\n"), testHubLogin(), true); err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want the edit to proceed", changed, err)
	}
}

func TestHubLoginSettingsRefusesAFileItCannotEdit(t *testing.T) {
	for name, content := range map[string]string{
		"a list":          "- one\n- two\n",
		"a scalar":        "just a string\n",
		"server a scalar": "server: nope\n",
		"broken yaml":     "server:\n\tbad: tab\n",
	} {
		if _, _, err := hubLoginSettings([]byte(content), testHubLogin(), false); err == nil {
			t.Fatalf("%s: no error, want a refusal rather than an overwrite", name)
		}
	}
}

func TestParseScionSettingsRead(t *testing.T) {
	settings, legacy, err := parseScionSettingsRead("LEGACY 1\nserver:\n  a: b\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !legacy {
		t.Fatal("legacy server.yaml not reported")
	}
	if string(settings) != "server:\n  a: b\n" {
		t.Fatalf("settings = %q", settings)
	}
	if _, _, err := parseScionSettingsRead(""); err == nil {
		t.Fatal("empty output accepted — an unreadable guest must not look like an empty config")
	}
	if _, _, err := parseScionSettingsRead("bash: orb: command not found\n"); err == nil {
		t.Fatal("unexpected output accepted")
	}
	// An absent settings.yaml is empty content, not a failure.
	settings, legacy, err = parseScionSettingsRead("LEGACY 0\n")
	if err != nil || legacy || len(settings) != 0 {
		t.Fatalf("absent settings: %q %v %v", settings, legacy, err)
	}
}

func TestLoginForwardScriptUsesAbsolutePathsAndTheRightArgv(t *testing.T) {
	script := loginForwardScript(testHubLogin(), false)
	// Two DIFFERENT ports: the guest listener is mirrored onto the host at its
	// own number, so a forwarder that listened and dialled on one number left
	// the host provider unable to bind (live failure — see
	// backend.GuestLoginIssuerPort).
	want := LoginForwardPath + " -listen 127.0.0.1:8446 -target host.orb.internal:8447"
	if !strings.Contains(script, want) {
		t.Fatalf("script does not start the forwarder as %q:\n%s", want, script)
	}
	// The guest run user's PATH has writable directories ahead of /usr/bin,
	// so every command this script runs must be absolute. bash's /dev/tcp
	// probe is a builtin, which is why it is used instead of netcat.
	for _, cmd := range []string{"pgrep", "pkill", "setsid", "nohup", "seq", "sleep"} {
		if !strings.Contains(script, "/usr/bin/"+cmd) {
			t.Fatalf("%s is not called by absolute path:\n%s", cmd, script)
		}
		if strings.Contains(script, "\n"+cmd+" ") || strings.Contains(script, " "+cmd+" ") {
			t.Fatalf("%s appears as a bare command name:\n%s", cmd, script)
		}
	}
	if !strings.Contains(script, "/dev/tcp/127.0.0.1/8446") {
		t.Fatalf("no liveness probe:\n%s", script)
	}
	// Idempotent: a matching, listening forwarder is left running.
	if !strings.Contains(script, `if [ "$force" != "true" ] && /usr/bin/pgrep`) {
		t.Fatalf("script always restarts the forwarder:\n%s", script)
	}
	if forced := loginForwardScript(testHubLogin(), true); !strings.Contains(forced, "force=true") {
		t.Fatalf("a replaced binary does not force a restart:\n%s", forced)
	}
}

func TestWriteScionSettingsScriptIsAtomicAndUserOwned(t *testing.T) {
	script := writeScionSettingsScript([]string{"orb", "-m", "lever-x"}, "/tmp/staged.yaml")
	for _, want := range []string{
		"set -o pipefail",
		"cat '/tmp/staged.yaml'",
		"'orb' '-m' 'lever-x'",
		".scion/settings.yaml.lever-tmp",
		`mv "$HOME/.scion/settings.yaml.lever-tmp" "$HOME/.scion/settings.yaml"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "root") {
		t.Fatalf("the settings file is the run user's, not root's:\n%s", script)
	}
}

// TestDisableLoginForwardScriptStopsTheBridgeBeforeRemovingIt pins the
// converge-off script. The forwarder is an unauthenticated TCP bridge from
// guest loopback — reachable from every agent netns — to a host loopback port
// beside lever's own broker listeners, so it must not outlive the feature.
func TestDisableLoginForwardScriptStopsTheBridgeBeforeRemovingIt(t *testing.T) {
	script := disableLoginForwardScript
	kill := strings.Index(script, "pkill -f '^"+LoginForwardPath)
	remove := strings.Index(script, "rm -f "+LoginForwardPath)
	switch {
	case kill < 0:
		t.Fatalf("the script never stops the running forwarder:\n%s", script)
	case remove < 0:
		t.Fatalf("the script never removes the forwarder:\n%s", script)
	case kill > remove:
		t.Fatalf("the binary is removed before the process is stopped, leaving the bridge up:\n%s", script)
	}
	// A pkill that is not there at all exits 127. Swallowing that would remove
	// the binary while its process kept bridging — the exact state this exists
	// to prevent — so only "nothing matched" (1) may pass.
	if !strings.Contains(script, "if [ $rc -gt 1 ]") {
		t.Fatalf("the script does not check that the kill actually happened:\n%s", script)
	}
	// Cheap on the common path: every apply of every instance with remote
	// access off runs this, and must do nothing when there is no forwarder.
	if !strings.Contains(script, "if [ ! -e "+LoginForwardPath+" ]; then echo \"FOUND 0\"; exit 0; fi") {
		t.Fatalf("the script does work when there is no forwarder to remove:\n%s", script)
	}
}

func TestHubLoginSettingsWithoutRemovesOnlyTheBlock(t *testing.T) {
	with, _, err := hubLoginSettings([]byte("# top\nversion: 1\nserver:\n  user_access_mode: open\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	out, changed, err := hubLoginSettingsWithout(with)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	m := unmarshalSettings(t, out)
	server := m["server"].(map[string]any)
	if _, ok := server["oidc_login"]; ok {
		t.Fatalf("oidc_login survived:\n%s", out)
	}
	if server["user_access_mode"] != "open" || m["version"] != 1 {
		t.Fatalf("removal took other keys with it:\n%s", out)
	}
	if !strings.Contains(string(out), "# top") {
		t.Fatalf("removal dropped comments:\n%s", out)
	}

	// Idempotent, and every "nothing to do" shape is a quiet no-op: removal is
	// convergence, not an assertion about what was there.
	if _, changed, err := hubLoginSettingsWithout(out); err != nil || changed {
		t.Fatalf("second removal: changed=%v err=%v", changed, err)
	}
	for name, content := range map[string]string{
		"empty":         "",
		"no server key": "version: 1\n",
		"server scalar": "server: nope\n",
		"not a mapping": "- one\n",
		"no oidc_login": "server:\n  user_access_mode: open\n",
	} {
		got, changed, err := hubLoginSettingsWithout([]byte(content))
		if err != nil || changed || string(got) != content {
			t.Fatalf("%s: got %q changed=%v err=%v, want an untouched no-op", name, got, changed, err)
		}
	}
}

// TestDisableHubLoginReportsOnlyWhatTheHubWasServing pins the OFF path's
// restart signal, which is the whole reason DisableHubLogin returns a bool at
// all: internal/apply.Run restarts the hub — dropping every agent's connection
// to it — when this says true.
//
// So the answer has to track ONE thing: did this call remove configuration the
// running hub was started from? The `oidc_login` block is that; the forwarder
// is not, since no hub ever reads it. Getting the second case wrong is not a
// theoretical cost — it is a hub bounce, and every agent reconnecting, on an
// apply that changed nothing the hub can see.
func TestDisableHubLoginReportsOnlyWhatTheHubWasServing(t *testing.T) {
	withBlock, _, err := hubLoginSettings([]byte("version: 1\nserver:\n  user_access_mode: open\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	const clean = "version: 1\nserver:\n  user_access_mode: open\n"

	for _, tc := range []struct {
		name     string
		forwards string // what the guest disable script reports
		settings string // what the guest's settings file holds
		want     bool
		why      string
	}{
		{"block present", "FOUND 0\n", string(withBlock), true,
			"the hub was started from a file that declared a login, and it is still serving it"},
		{"already converged", "FOUND 0\n", clean, false,
			"a re-apply with remote access already off must not bounce the hub"},
		{"forwarder but no block", "FOUND 1\n", clean, false,
			"removing the forwarder changes nothing the hub reads, so it cannot be worth a restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script("orb -u root -m m bash -c", exec.Result{Stdout: tc.forwards})
			f.Script("orb -m m /bin/bash -c", exec.Result{Stdout: "LEGACY 0\n" + tc.settings})
			f.Script("bash -c", exec.Result{})
			g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}

			changed, err := g.DisableHubLogin(context.Background())
			if err != nil {
				t.Fatalf("DisableHubLogin: %v", err)
			}
			if changed != tc.want {
				t.Fatalf("changed = %v, want %v — %s", changed, tc.want, tc.why)
			}
		})
	}
}

// TestDisableHubLoginReportsNoChangeWhenItCannotRead: the read paths are
// fail-soft (an apply told only to turn remote access off should not die on an
// unreadable settings file), and "no change" is the honest answer for them —
// nothing was removed, so nothing the hub is serving changed. Reporting true
// here would restart the hub every apply on any guest whose settings lever
// cannot parse.
func TestDisableHubLoginReportsNoChangeWhenItCannotRead(t *testing.T) {
	for name, read := range map[string]exec.Result{
		"unparseable output": {Stdout: "not the LEGACY header at all\n"},
		"not yaml":           {Stdout: "LEGACY 0\n\tthis: [is not\n"},
	} {
		t.Run(name, func(t *testing.T) {
			f := exec.NewFakeRunner()
			f.Script("orb -u root -m m bash -c", exec.Result{Stdout: "FOUND 1\n"})
			f.Script("orb -m m /bin/bash -c", read)
			g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}

			changed, err := g.DisableHubLogin(context.Background())
			if err != nil {
				t.Fatalf("DisableHubLogin: %v", err)
			}
			if changed {
				t.Fatal("an unreadable settings file reported a change, so every apply would restart the hub")
			}
		})
	}
}

// TestEnsureHubLoginRefusesOnePortForBothHalves: the guest listener is
// mirrored onto the host at the same number, so one number for both halves
// means the host provider cannot bind its own port — the live failure
// backend.GuestLoginIssuerPort exists to prevent. Config keeps the two apart,
// and this is the backstop for any other caller.
func TestEnsureHubLoginRefusesOnePortForBothHalves(t *testing.T) {
	spec := testHubLogin()
	spec.HostPort = spec.IssuerPort
	g := Guest{Machine: "lever-x"}
	if _, err := g.EnsureHubLogin(context.Background(), spec); err == nil {
		t.Fatal("accepted one port for both halves of the bridge")
	}
}

// TestLoginForwardScriptReplacesAForwarderWithStaleArguments is the regression
// test for a live failure: after `remote.login_port` changed, the guest kept
// running a forwarder whose `-target` named the OLD host port, so the hub's
// discovery fetch went guest 8446 -> host 8446, which is the container
// runtime's MIRROR of guest 8446 — the forwarder looping back to itself. The
// browser got a 502 while the apply reported success.
//
// Two decisions were wrong, and this pins both.
func TestLoginForwardScriptReplacesAForwarderWithStaleArguments(t *testing.T) {
	spec := testHubLogin()
	script := loginForwardScript(spec, false)

	// 1. Editing login_port must produce a different desired argv, or nothing
	//    downstream can tell the two apart.
	moved := spec
	moved.HostPort = spec.HostPort + 10
	if want, got := loginForwardScript(moved, false), script; want == got {
		t.Fatal("a changed host port produced an identical script")
	}

	// 2. The stop must match ANY forwarder, whatever argv it carries. Killing
	//    only the exact desired argv left the stale process alive, holding the
	//    listen port, so the replacement could never bind.
	if !strings.Contains(script, "/usr/bin/pkill -f '"+loginForwardMatch+"'") {
		t.Fatalf("the script does not stop a forwarder whose arguments differ:\n%s", script)
	}
	if strings.Contains(script, `pkill -f -x "$want"`) {
		t.Fatalf("the script stops only an exact-argv match, so a stale forwarder survives:\n%s", script)
	}
	stop := strings.Index(script, "/usr/bin/pkill")
	start := strings.Index(script, "/usr/bin/setsid")
	if stop < 0 || start < 0 || stop > start {
		t.Fatalf("the replacement is started before the old one is stopped:\n%s", script)
	}
	// pkill only SIGNALS. A forwarder that has been SIGTERMed still holds the
	// listen port until it exits, so starting the replacement immediately
	// races it for the bind — the same lost bind that made this a silent
	// failure. The script must wait for the old process to be gone.
	between := script[stop:start]
	if !strings.Contains(between, "/usr/bin/pgrep -f '"+loginForwardMatch+"'") {
		t.Fatalf("the script starts the replacement without waiting for the old process to exit:\n%s", between)
	}

	// 3. Success must mean "a forwarder with THESE arguments is running", not
	//    "something answers on the port" — the stale process answered, which is
	//    what made the failure silent.
	final := script[start:]
	if !strings.Contains(final, `/usr/bin/pgrep -f -x "$want"`) {
		t.Fatalf("the script accepts any listener as success, so a stale forwarder reads as a working one:\n%s", final)
	}
	if !strings.Contains(final, "/dev/tcp/127.0.0.1/8446") {
		t.Fatalf("the script never confirms the port answers:\n%s", final)
	}
}

// authBlock digs out server.auth.
func authBlock(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m := unmarshalSettings(t, b)
	server, ok := m["server"].(map[string]any)
	if !ok {
		t.Fatalf("no `server:` mapping in:\n%s", b)
	}
	auth, _ := server["auth"].(map[string]any)
	return auth
}

// TestHubLoginSettingsNamesAnOperator: without a name in `server.auth`, scion's
// system status never reports complete and the SPA bounces every fresh load to
// /onboarding — a setup wizard for a machine lever has already set up.
func TestHubLoginSettingsNamesAnOperator(t *testing.T) {
	out, changed, err := hubLoginSettings(nil, testHubLogin(), false)
	if err != nil || !changed {
		t.Fatalf("hubLoginSettings: changed=%v err=%v", changed, err)
	}
	if got := authBlock(t, out)["display_name"]; got != operatorDisplayName {
		t.Fatalf("server.auth.display_name = %v, want %q:\n%s", got, operatorDisplayName, out)
	}
	// Not an operator's email: allowed_users gives each operator their own hub
	// user, and this field must not compete with that.
	if _, ok := authBlock(t, out)["email"]; ok {
		t.Fatalf("an email was invented:\n%s", out)
	}
}

// TestHubLoginSettingsKeepsAnOperatorsOwnIdentity: this only ever adds. Any one
// of the three fields already naming someone means lever writes nothing, so a
// re-apply cannot rename a user.
func TestHubLoginSettingsKeepsAnOperatorsOwnIdentity(t *testing.T) {
	for _, existing := range []string{
		"server:\n  auth:\n    display_name: Stephen\n",
		"server:\n  auth:\n    email: me@example.com\n",
		"server:\n  auth:\n    username: stephen\n",
	} {
		out, _, err := hubLoginSettings([]byte(existing), testHubLogin(), false)
		if err != nil {
			t.Fatalf("%s: %v", existing, err)
		}
		if got := authBlock(t, out)["display_name"]; got == operatorDisplayName {
			t.Fatalf("lever overwrote an operator's identity:\n%s", out)
		}
	}
	// An `auth` block carrying only unrelated keys still gets named, and keeps
	// them: the block is merged into, never replaced.
	out, _, err := hubLoginSettings([]byte("server:\n  auth:\n    user_access_mode: open\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	auth := authBlock(t, out)
	if auth["display_name"] != operatorDisplayName || auth["user_access_mode"] != "open" {
		t.Fatalf("auth block = %v:\n%s", auth, out)
	}
}

// TestHubLoginSettingsIdentityAloneIsAChange: the two writes share one
// "changed" because scion reads the whole file once at startup, so a settings
// file with a correct oidc_login but no operator must still restart the hub.
func TestHubLoginSettingsIdentityAloneIsAChange(t *testing.T) {
	full, _, err := hubLoginSettings(nil, testHubLogin(), false)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	stripped, changed, err := hubLoginSettingsWithout(full)
	if err != nil || !changed {
		t.Fatalf("strip: changed=%v err=%v", changed, err)
	}
	// Put oidc_login back but leave the operator unnamed.
	withOIDCOnly, _, err := hubLoginSettings(stripped, testHubLogin(), false)
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if authBlock(t, withOIDCOnly)["display_name"] != operatorDisplayName {
		t.Fatalf("the identity was not restored alongside oidc_login:\n%s", withOIDCOnly)
	}
}

// TestHubLoginSettingsWithoutUnnamesOnlyWhatLeverWrote: turning remote access
// off removes lever's own value, and nothing that looks like a person's.
func TestHubLoginSettingsWithoutUnnamesOnlyWhatLeverWrote(t *testing.T) {
	with, _, err := hubLoginSettings([]byte("server:\n  user_access_mode: open\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	out, changed, err := hubLoginSettingsWithout(with)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if auth := authBlock(t, out); auth != nil {
		t.Fatalf("an emptied auth block was left behind: %v\n%s", auth, out)
	}

	// An operator's own identity survives the removal, and so does an unrelated
	// key sitting beside lever's.
	mine, _, err := hubLoginSettings([]byte("server:\n  auth:\n    display_name: Stephen\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	out, _, err = hubLoginSettingsWithout(mine)
	if err != nil {
		t.Fatalf("hubLoginSettingsWithout: %v", err)
	}
	if authBlock(t, out)["display_name"] != "Stephen" {
		t.Fatalf("removal took an operator's identity with it:\n%s", out)
	}

	keep, _, err := hubLoginSettings([]byte("server:\n  auth:\n    user_access_mode: open\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	out, _, err = hubLoginSettingsWithout(keep)
	if err != nil {
		t.Fatalf("hubLoginSettingsWithout: %v", err)
	}
	auth := authBlock(t, out)
	if _, named := auth["display_name"]; named || auth["user_access_mode"] != "open" {
		t.Fatalf("auth block = %v:\n%s", auth, out)
	}
}

// TestEnableMessageBroker covers the two halves of the contract: fill in the
// key scion's chat store is gated on, and never overturn an operator's own
// choice.
func TestEnableMessageBroker(t *testing.T) {
	tests := []struct {
		name        string
		server      string
		wantChanged bool
		wantEnabled string // "" = key must be absent
	}{
		{
			name:        "absent key is filled in",
			server:      "server:\n  auth:\n    display_name: Lever\n",
			wantChanged: true,
			wantEnabled: "true",
		},
		{
			name:        "an explicit false is left alone",
			server:      "server:\n  message_broker:\n    enabled: false\n",
			wantChanged: false,
			wantEnabled: "false",
		},
		{
			name:        "an explicit true is already right, so nothing changes",
			server:      "server:\n  message_broker:\n    enabled: true\n",
			wantChanged: false,
			wantEnabled: "true",
		},
		{
			name:        "a block with other keys but no enabled is filled in",
			server:      "server:\n  message_broker:\n    types: [inprocess]\n",
			wantChanged: true,
			wantEnabled: "true",
		},
		{
			name:        "a shape lever cannot reason about is left for the operator",
			server:      "server:\n  message_broker: \"on\"\n",
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tt.server), &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			server := mapGet(doc.Content[0], "server")
			if server == nil {
				t.Fatal("no server key in the fixture")
			}

			if got := enableMessageBroker(server); got != tt.wantChanged {
				t.Errorf("changed = %v, want %v", got, tt.wantChanged)
			}

			mb := mapGet(server, "message_broker")
			if tt.wantEnabled == "" {
				if mb != nil && mb.Kind == yaml.MappingNode && mapGet(mb, "enabled") != nil {
					t.Error("enabled was written into a shape lever should not touch")
				}
				return
			}
			if mb == nil {
				t.Fatal("message_broker block is missing")
			}
			n := mapGet(mb, "enabled")
			if n == nil {
				t.Fatal("enabled key is missing")
			}
			if n.Value != tt.wantEnabled {
				t.Errorf("enabled = %q, want %q", n.Value, tt.wantEnabled)
			}
		})
	}
}
