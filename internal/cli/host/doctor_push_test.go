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
	t.Run("test hosts warn", func(t *testing.T) {
		app, st := pushApp(t)
		remotePush(app, st, nil, &bytes.Buffer{})
		remoteproxy.WritePushStatus(st.PushDir(), remoteproxy.PushStatus{At: time.Now(), Result: "started", TestHosts: true})
		if r := checkPush(app, st); !r.ok || r.fix == "" || !strings.Contains(r.detail, webpush.TestHostsEnv) {
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
	t.Run("state inside the tree fails", func(t *testing.T) {
		app, st := pushApp(t)
		app.Tree = filepath.Dir(st.Dir)
		if r := checkPush(app, st); r.ok {
			t.Fatalf("%+v", r)
		}
	})
}
