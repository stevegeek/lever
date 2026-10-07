package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/webpush"
)

func pushApp(t *testing.T) (*config.App, state.State) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	os.MkdirAll(tree, 0o700)
	st := state.ForConfig(root)
	os.MkdirAll(st.Dir, 0o700)
	return &config.App{Name: "boss", Tree: tree, Remote: config.Remote{Enabled: true, BaseURL: "https://mac.ts.net",
		Landing: config.RemoteLandingChat, AllowedUsers: []config.RemoteUser{{Login: "op@x"}},
		Push: config.Push{Enabled: true, Subject: "mailto:op@x.example"}}}, st
}

func TestRemotePushOffBuildsNothing(t *testing.T) {
	app, st := pushApp(t)
	app.Remote.Push.Enabled = false
	p, err := remotePush(app, st, nil, &bytes.Buffer{})
	if p != nil || err != nil {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := os.Stat(st.PushDir()); !os.IsNotExist(err) {
		t.Fatal("push off created the push directory")
	}
}

func TestRemotePushOnCreatesKeyAndStore(t *testing.T) {
	app, st := pushApp(t)
	p, err := remotePush(app, st, nil, &bytes.Buffer{})
	if err != nil || p == nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(st.PushDir(), "vapid.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key %v %v", fi, err)
	}
}

// TestRemotePushNamesAnEmptyFile: an empty key or store (a power loss)
// keeps push off with a warning that names the file and the fix.
func TestRemotePushNamesAnEmptyFile(t *testing.T) {
	for _, file := range []string{"vapid.key", "subscriptions.json"} {
		app, st := pushApp(t)
		if _, err := remotePush(app, st, nil, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(st.PushDir(), file), nil, 0o600)
		var warn bytes.Buffer
		p, err := remotePush(app, st, nil, &warn)
		if p != nil || err != nil {
			t.Fatalf("%s: %v %v", file, p, err)
		}
		if w := warn.String(); !strings.Contains(w, filepath.Join(st.PushDir(), file)) || !strings.Contains(w, "remove") {
			t.Fatalf("%s: %q", file, w)
		}
	}
}

func TestRemotePushRefusesBadTestHosts(t *testing.T) {
	app, st := pushApp(t)
	t.Setenv(webpush.TestHostsEnv, "example.com:443")
	if _, err := remotePush(app, st, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("a non-loopback test host must stop the serve")
	}
	// The environment alone is not enough: the config must name the same
	// addresses, and the config alone is not enough either.
	t.Setenv(webpush.TestHostsEnv, "127.0.0.1:9447")
	if _, err := remotePush(app, st, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "test_hosts") {
		t.Fatalf("environment only: %v", err)
	}
	app.Remote.Push.TestHosts = []string{"127.0.0.1:9448"}
	if _, err := remotePush(app, st, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("different addresses were accepted")
	}
	t.Setenv(webpush.TestHostsEnv, "")
	if _, err := remotePush(app, st, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("config only was accepted")
	}
	app.Remote.Push.TestHosts = []string{"127.0.0.1:9447"}
	t.Setenv(webpush.TestHostsEnv, "127.0.0.1:9447")
	var warn bytes.Buffer
	if p, err := remotePush(app, st, nil, &warn); err != nil || p == nil || !strings.Contains(warn.String(), "TEST ONLY") {
		t.Fatalf("%v %q", err, warn.String())
	}
}

func TestRemotePushStaysOffWithStateInTheTree(t *testing.T) {
	app, st := pushApp(t)
	app.Tree = filepath.Dir(st.Dir)
	var warn bytes.Buffer
	p, err := remotePush(app, st, nil, &warn)
	if p != nil || err != nil || !strings.Contains(warn.String(), "inside the tree") {
		t.Fatalf("%v %v %q", p, err, warn.String())
	}
}
