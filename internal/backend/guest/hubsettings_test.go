package guest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/types"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion/layout"
	"gopkg.in/yaml.v3"
)

func testHubAuth(remote bool) types.HubAuth {
	return types.HubAuth{PublicKeyPEM: []byte("-----BEGIN PUBLIC KEY-----\nx\n-----END PUBLIC KEY-----\n"),
		Issuer: "lever:abc", Audience: "scion-hub:abc", AdminEmail: "lever-admin@lever.local",
		Header: "X-Lever-Hub-Assertion", RemoteOn: remote}
}

func unmarshalSettings(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse rendered settings: %v\n%s", err, b)
	}
	return m
}

func serverBlock(t *testing.T, b []byte) map[string]any {
	t.Helper()
	server, ok := unmarshalSettings(t, b)["server"].(map[string]any)
	if !ok {
		t.Fatalf("no `server:` mapping in:\n%s", b)
	}
	return server
}

func TestHubAuthSettingsWritesLeversKeysInAnEmptyFile(t *testing.T) {
	out, oidc, changed, err := hubAuthSettingsConverged(nil, testHubAuth(false), false)
	if err != nil || !changed || oidc {
		t.Fatalf("changed=%v oidc=%v err=%v", changed, oidc, err)
	}
	server := serverBlock(t, out)
	auth := server["auth"].(map[string]any)
	if auth["mode"] != "proxy" || auth["user_access_mode"] != "invite_only" {
		t.Fatalf("auth = %v", auth)
	}
	jwt := auth["proxy"].(map[string]any)["jwt"].(map[string]any)
	if jwt["public_key_file"] != HubAuthKeyPath || jwt["issuer"] != "lever:abc" || jwt["audience"] != "scion-hub:abc" || jwt["algorithm"] != "ES256" {
		t.Fatalf("jwt = %v", jwt)
	}
	admins := server["hub"].(map[string]any)["admin_emails"].([]any)
	if len(admins) != 1 || admins[0] != "lever-admin@lever.local" {
		t.Fatalf("admin_emails = %v", admins)
	}
	if _, ok := server["oidc_login"]; ok {
		t.Fatalf("oidc_login written:\n%s", out)
	}
}

func TestHubAuthSettingsIsIdempotentAndIgnoresTheIdentityRouteKeys(t *testing.T) {
	out, _, _, err := hubAuthSettingsConverged(nil, testHubAuth(true), false)
	if err != nil {
		t.Fatal(err)
	}
	again, oidc, changed, err := hubAuthSettingsConverged(out, testHubAuth(true), false)
	if err != nil || changed || oidc || string(again) != string(out) {
		t.Fatalf("second pass changed=%v oidc=%v err=%v", changed, oidc, err)
	}
	// scion's system/identity route writes server.auth.email and display_name;
	// any loopback caller can reach it, so neither may force a rewrite.
	doc, _ := layout.ParseSettings(out)
	auth := layout.MapGet(layout.MapGet(layout.DocumentRoot(doc), "server"), "auth")
	layout.MapSet(auth, "email", layout.StringNode("someone@example.com"))
	layout.MapSet(auth, "display_name", layout.StringNode("Someone"))
	edited, _ := layout.EncodeSettings(doc)
	_, _, changed, err = hubAuthSettingsConverged(edited, testHubAuth(true), false)
	if err != nil || changed {
		t.Fatalf("identity keys forced a change: changed=%v err=%v", changed, err)
	}
}

func TestHubAuthSettingsRemovesOIDCLoginAndDevAuthAndReportsIt(t *testing.T) {
	existing := []byte("# keep me\nversion: 1\nserver:\n  oidc_login:\n    enabled: true\n    issuer_url: http://127.0.0.1:8446\n  auth:\n    dev_mode: true\n    mode: dev\n    user_access_mode: open\n  hub:\n    admin_emails: [someone@example.com]\n  broker:\n    port: 1\n")
	out, oidc, changed, err := hubAuthSettingsConverged(existing, testHubAuth(false), false)
	if err != nil || !changed || !oidc {
		t.Fatalf("changed=%v oidc=%v err=%v", changed, oidc, err)
	}
	server := serverBlock(t, out)
	if _, ok := server["oidc_login"]; ok {
		t.Fatalf("oidc_login kept:\n%s", out)
	}
	auth := server["auth"].(map[string]any)
	if _, ok := auth["dev_mode"]; ok || auth["mode"] != "proxy" || auth["user_access_mode"] != "invite_only" {
		t.Fatalf("auth = %v", auth)
	}
	if a := server["hub"].(map[string]any)["admin_emails"].([]any); len(a) != 1 || a[0] != "lever-admin@lever.local" {
		t.Fatalf("admin_emails = %v", a)
	}
	if server["broker"].(map[string]any)["port"] != 1 || !strings.Contains(string(out), "# keep me") {
		t.Fatalf("other content lost:\n%s", out)
	}
}

func TestHubAuthSettingsRefusesWhatItCannotEdit(t *testing.T) {
	if _, _, _, err := hubAuthSettingsConverged([]byte("version: 1\n"), testHubAuth(false), true); err == nil {
		t.Fatal("added a server key while a legacy server.yaml exists")
	}
	for _, bad := range []string{"- a\n", "server: 3\n", "server:\n  auth: x\n", "server:\n  hub: [1]\n"} {
		if _, _, _, err := hubAuthSettingsConverged([]byte(bad), testHubAuth(false), false); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestHubAuthSettingsWebOnlyKeysFollowRemote(t *testing.T) {
	on, _, _, err := hubAuthSettingsConverged(nil, testHubAuth(true), false)
	if err != nil {
		t.Fatal(err)
	}
	server := serverBlock(t, on)
	if server["auth"].(map[string]any)["display_name"] != operatorDisplayName || server["message_broker"].(map[string]any)["enabled"] != true {
		t.Fatalf("remote on:\n%s", on)
	}
	off, _, changed, err := hubAuthSettingsConverged(on, testHubAuth(false), false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	server = serverBlock(t, off)
	if _, named := server["auth"].(map[string]any)["display_name"]; named {
		t.Fatalf("lever's display name kept with remote off:\n%s", off)
	}
	mine, _, _, _ := hubAuthSettingsConverged([]byte("server:\n  auth:\n    display_name: Dev User\n"), testHubAuth(false), false)
	if serverBlock(t, mine)["auth"].(map[string]any)["display_name"] != "Dev User" {
		t.Fatalf("an operator's display name was removed:\n%s", mine)
	}
}

func TestInstallHubAuthKeyScriptIsRootOwnedAtomicAndRefusesAPlantedDir(t *testing.T) {
	s := installHubAuthKeyScript
	for _, want := range []string{"-L \"$d\"", "stat -c %u \"$d\"", "install -d -o root -g root -m 0755", "mktemp", "chown root:root", "chmod 0644", "mv -f \"$tmp\" \"$f\"", "echo SAME", "echo CHANGED"} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q:\n%s", want, s)
		}
	}
	if !strings.HasPrefix(HubAuthKeyPath, "/etc/") {
		t.Fatalf("key path %s is not outside every home", HubAuthKeyPath)
	}
}

func TestEnsureHubAuthRemovesTheForwarderInstallsTheKeyAndReportsARestart(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script(strings.TrimSpace("orb -u root -m lever-x bash -c "+removeLoginForwardScript), proc.Result{})
	f.Script(strings.TrimSpace("orb -u root -m lever-x bash -c "+installHubAuthKeyScript), proc.Result{Stdout: "CHANGED\n"})
	f.Script(strings.TrimSpace("orb -m lever-x /bin/bash -c "+readScionSettingsScript), proc.Result{Stdout: "LEGACY 0\n"})
	f.Script("orb -m lever-x bash -c", proc.Result{})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-x"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-x"}}
	restart, err := g.EnsureHubAuth(context.Background(), testHubAuth(false))
	if err != nil || !restart {
		t.Fatalf("restart=%v err=%v", restart, err)
	}
	var keyCall *proc.Call
	for i, c := range f.Calls {
		if strings.Contains(strings.Join(c.Args, " "), "hub-auth.pub") {
			keyCall = &f.Calls[i]
		}
	}
	if keyCall == nil || keyCall.Stdin != string(testHubAuth(false).PublicKeyPEM) || keyCall.Name != "orb" || keyCall.Args[0] != "-u" {
		t.Fatalf("key not installed as root over stdin: %+v", f.Calls)
	}
	if _, err := g.EnsureHubAuth(context.Background(), types.HubAuth{}); err == nil {
		t.Fatal("an empty spec was accepted")
	}
}

func TestRemoveLoginForwardScriptStopsTheBridgeBeforeRemovingIt(t *testing.T) {
	s := removeLoginForwardScript
	kill, rm := strings.Index(s, "pkill"), strings.Index(s, "rm -f")
	if kill < 0 || rm < 0 || kill > rm || !strings.Contains(s, "exit 1") {
		t.Fatalf("script must stop the forwarder (and fail if it cannot) before removing it:\n%s", s)
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

func TestWriteScionSettingsIsAtomicAndUserOwned(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb -m lever-x bash -c", proc.Result{})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-x"}, RootPrefix: []string{"orb", "-u", "root", "-m", "lever-x"}}
	if err := g.writeScionSettings(context.Background(), []byte("server: {}\n")); err != nil {
		t.Fatalf("writeScionSettings: %v", err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want one call, got %+v", f.Calls)
	}
	c := f.Calls[0]
	// The content travels on stdin of `<userPrefix> bash -c <script>`: argv
	// only, as the run user (the file is theirs, not root's).
	if c.Name != "orb" || !reflect.DeepEqual(c.Args[:4], []string{"-m", "lever-x", "bash", "-c"}) {
		t.Fatalf("argv = %s %v", c.Name, c.Args)
	}
	if c.Stdin != "server: {}\n" {
		t.Fatalf("stdin = %q", c.Stdin)
	}
	script := c.Args[len(c.Args)-1]
	for _, want := range []string{
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

// TestDisableLoginForwardScriptStopsTheBridgeBeforeRemovingIt: the forwarder
// is an unauthenticated TCP bridge from guest loopback, reachable from every
// agent netns, to a host loopback port, so it must not outlive the feature.
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

// TestHubLoginSettingsNamesAnOperator: without a name in `server.auth`, the
// SPA bounces every fresh load to /onboarding.
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
			server := layout.MapGet(doc.Content[0], "server")
			if server == nil {
				t.Fatal("no server key in the fixture")
			}

			if got := enableMessageBroker(server); got != tt.wantChanged {
				t.Errorf("changed = %v, want %v", got, tt.wantChanged)
			}

			mb := layout.MapGet(server, "message_broker")
			if tt.wantEnabled == "" {
				if mb != nil && mb.Kind == yaml.MappingNode && layout.MapGet(mb, "enabled") != nil {
					t.Error("enabled was written into a shape lever should not touch")
				}
				return
			}
			if mb == nil {
				t.Fatal("message_broker block is missing")
			}
			n := layout.MapGet(mb, "enabled")
			if n == nil {
				t.Fatal("enabled key is missing")
			}
			if n.Value != tt.wantEnabled {
				t.Errorf("enabled = %q, want %q", n.Value, tt.wantEnabled)
			}
		})
	}
}

// writtenSettings returns the settings content the guest was sent (the stdin
// of the write script) and how many writes were attempted.
func writtenSettings(f *proc.FakeRunner) (written string, writes int) {
	for _, c := range f.Calls {
		if len(c.Args) > 0 && c.Args[len(c.Args)-1] == writeScionSettingsScript {
			writes++
			written = c.Stdin
		}
	}
	return written, writes
}
