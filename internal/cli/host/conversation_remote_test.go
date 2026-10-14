package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
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
		if err := fn(context.Background(), true); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("code %d: %v", tc.code, err)
		}
	}
	code = 1
	if err := fn(context.Background(), false); err == nil || errors.Is(err, remoteproxy.ErrConversationBusy) {
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
