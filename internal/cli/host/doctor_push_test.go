package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/webpush"
)

func TestCheckPush(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		app, st := pushApp(t)
		app.Remote.Push.Enabled = false
		if r := checkPush(app, st); !r.ok || r.fix != "" || !strings.HasPrefix(r.detail, "off") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("on, no key yet", func(t *testing.T) {
		app, st := pushApp(t)
		if r := checkPush(app, st); !r.ok || r.fix == "" || !strings.Contains(r.detail, "no key yet") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("on, running", func(t *testing.T) {
		app, st := pushApp(t)
		p, _ := remotePush(app, st, nil, &bytes.Buffer{})
		_ = p
		remoteproxy.WritePushStatus(st.PushDir(), remoteproxy.PushStatus{At: time.Now(), Result: "sent", Status: 201, Host: "fcm.googleapis.com"})
		r := checkPush(app, st)
		if !r.ok || r.fix != "" || !strings.Contains(r.detail, "0 subscription") || !strings.Contains(r.detail, "sent") ||
			!strings.Contains(r.detail, "fcm.googleapis.com") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a proxy on test hosts fails", func(t *testing.T) {
		app, st := pushApp(t)
		remotePush(app, st, nil, &bytes.Buffer{})
		remoteproxy.WritePushStatus(st.PushDir(), remoteproxy.PushStatus{At: time.Now(), Result: "started", TestHosts: true})
		if r := checkPush(app, st); r.ok || !strings.Contains(r.fix, webpush.TestHostsEnv) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("test_hosts in the config fails", func(t *testing.T) {
		app, st := pushApp(t)
		app.Remote.Push.TestHosts = []string{"127.0.0.1:9447"}
		if r := checkPush(app, st); r.ok || !strings.Contains(r.detail, "test_hosts") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("open key fails", func(t *testing.T) {
		app, st := pushApp(t)
		remotePush(app, st, nil, &bytes.Buffer{})
		os.Chmod(filepath.Join(st.PushDir(), "vapid.key"), 0o644)
		if r := checkPush(app, st); r.ok {
			t.Fatalf("%+v", r)
		}
	})
	// A power loss before the writes were synced could leave either file
	// empty: the row names the file and the fix, never "key present".
	t.Run("empty key fails with its fix", func(t *testing.T) {
		app, st := pushApp(t)
		remotePush(app, st, nil, &bytes.Buffer{})
		os.WriteFile(filepath.Join(st.PushDir(), "vapid.key"), nil, 0o600)
		r := checkPush(app, st)
		if r.ok || !strings.Contains(r.detail, "vapid.key") || !strings.Contains(r.fix, "remove") || strings.Contains(r.fix, "chmod") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("empty store fails with its fix", func(t *testing.T) {
		app, st := pushApp(t)
		remotePush(app, st, nil, &bytes.Buffer{})
		os.WriteFile(filepath.Join(st.PushDir(), "subscriptions.json"), nil, 0o600)
		r := checkPush(app, st)
		if r.ok || !strings.Contains(r.detail, "subscriptions.json") || !strings.Contains(r.fix, "remove") || strings.Contains(r.fix, "chmod") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("state inside the tree fails", func(t *testing.T) {
		app, st := pushApp(t)
		app.Tree = filepath.Dir(st.Dir)
		if r := checkPush(app, st); r.ok {
			t.Fatalf("%+v", r)
		}
	})
}
