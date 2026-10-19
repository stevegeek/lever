package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/remoteproxy"
	"github.com/stevegeek/lever/internal/state"
)

// conversationNewLog collects the output of the runs lever starts itself.
const conversationNewLog = "conversation-new.log"

// remoteNewConversation backs the chat page's new conversation: `lever
// conversation new --by chat` as the host user, the proxy's own authority.
// Nil when the state directory is inside the tree: lever keeps no backup
// there, and makes no reset without one.
func remoteNewConversation(app *config.App, st state.State, configPath string) func(ctx context.Context, force bool) error {
	if brokerctl.StateInsideTree(app, st) {
		return nil
	}
	return func(ctx context.Context, force bool) error {
		code, err := startConversationNew(ctx, st, configPath, "chat", force)
		switch {
		case err != nil:
			return err
		case code == exitConversationBusy:
			return remoteproxy.ErrConversationBusy
		case code == exitConversationLocked:
			return remoteproxy.ErrConversationLocked
		case code != 0:
			return fmt.Errorf("lever conversation new exited %d (see %s)", code, filepath.Join(st.BringupLogDir(), conversationNewLog))
		}
		return nil
	}
}

// unattendedLockWait is how long a new conversation lever starts on its own
// waits for another change to the manager to finish, rather than exit 6: the
// chat page has already told its operator it is starting, and the daily
// reset would otherwise retry only ten minutes later.
var unattendedLockWait = map[string]time.Duration{
	"chat":  3 * time.Minute,
	"daily": 10 * time.Minute,
}

// startConversationNew is a seam: tests must not start a real `lever` process.
var startConversationNew = runConversationNew

// runConversationNew runs `lever conversation new` in its own session, so a
// proxy or broker restart mid-way does not kill it with the manager stopped.
// It returns the exit code, or ctx's error while the run goes on.
func runConversationNew(ctx context.Context, st state.State, configPath, by string, force bool) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return 0, err
	}
	logDir := st.BringupLogDir()
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(filepath.Join(logDir, conversationNewLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND|hostledger.ONoFollow, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	args := []string{"conversation", "new", "--by", by, "--progress", "plain", "--wait", unattendedLockWait[by].String()}
	if force {
		args = append(args, "--force")
	}
	c := exec.Command(self, append(args, "--", configPath)...)
	c.Dir = filepath.Dir(configPath)
	c.Stdin = nil
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = detachedAttr()
	if err := c.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 0, err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
