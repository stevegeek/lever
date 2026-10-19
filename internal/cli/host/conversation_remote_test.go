package host

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/convbackup"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/state"
)

// TestRemoteNewConversation: the chat page's call runs `conversation new
// --by chat` and reads its exit code into the proxy's refusals; none is
// wired with the state directory inside the tree.
func TestRemoteNewConversation(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, "state")}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	app := &config.App{Name: "mgr", Tree: filepath.Join(root, "tree")}
	type call struct {
		by    string
		force bool
	}
	var calls []call
	code := 0
	var runErr error
	old := startConversationNew
	t.Cleanup(func() { startConversationNew = old })
	startConversationNew = func(_ context.Context, _ state.State, cfg, by string, force bool) (int, error) {
		if cfg != "/inst/lever.yaml" {
			t.Errorf("config %q", cfg)
		}
		calls = append(calls, call{by, force})
		return code, runErr
	}
	fn := remoteNewConversation(app, st, "/inst/lever.yaml")
	if fn == nil {
		t.Fatal("not wired")
	}
	for _, tc := range []struct {
		code int
		err  error
		want error
	}{
		{0, nil, nil},
		{exitConversationBusy, nil, remoteproxy.ErrConversationBusy},
		{exitConversationLocked, nil, remoteproxy.ErrConversationLocked},
	} {
		code, runErr = tc.code, tc.err
		if _, err := fn(context.Background(), true); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("code %d: %v", tc.code, err)
		}
	}
	code = 1
	if _, err := fn(context.Background(), false); err == nil || errors.Is(err, remoteproxy.ErrConversationBusy) {
		t.Errorf("failure: %v", err)
	}
	if calls[0] != (call{"chat", true}) || calls[len(calls)-1] != (call{"chat", false}) {
		t.Errorf("calls %v", calls)
	}
	in := &config.App{Name: "mgr", Tree: root}
	if remoteNewConversation(in, st, "/inst/lever.yaml") != nil {
		t.Error("wired with the state directory inside the tree")
	}
}

// TestRemoteNewConversationBackup: a run that succeeds answers with the
// backup it saved, never an older one, another reason's or another agent's.
func TestRemoteNewConversationBackup(t *testing.T) {
	root := t.TempDir()
	st := state.State{Dir: filepath.Join(root, "state")}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	app := &config.App{Name: "mgr", Tree: filepath.Join(root, "tree")}
	store := convbackup.Store{Dir: filepath.Join(st.Dir, convbackup.DirName)}
	save := func(agent, reason string, at time.Time) string {
		t.Helper()
		m, err := store.Save(convbackup.Meta{Agent: agent, Reason: reason, Created: at.UTC()}, func(w io.Writer) (convbackup.Stats, error) {
			_, err := w.Write([]byte("x"))
			return convbackup.Stats{}, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	chat := newConvBy["chat"].Reason
	save("mgr", chat, time.Now().Add(-time.Hour))
	old := startConversationNew
	t.Cleanup(func() { startConversationNew = old })
	var saveIn func()
	startConversationNew = func(context.Context, state.State, string, string, bool) (int, error) {
		if saveIn != nil {
			saveIn()
		}
		return 0, nil
	}
	fn := remoteNewConversation(app, st, "/inst/lever.yaml")
	if got, err := fn(context.Background(), false); err != nil || got != "" {
		t.Fatalf("nothing saved: %q %v", got, err)
	}
	var want string
	saveIn = func() {
		save("other", chat, time.Now())
		want = save("mgr", chat, time.Now())
		save("mgr", "manual", time.Now())
	}
	if got, err := fn(context.Background(), false); err != nil || got != want {
		t.Fatalf("got %q, want %q (%v)", got, want, err)
	}
}
