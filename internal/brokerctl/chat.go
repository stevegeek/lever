package brokerctl

import (
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// ChatLedgerPath is the verified-chat ledger both the remote proxy (writer)
// and the broker (reader) use, or "" when verified chat is off. One function
// decides for both, so the two can never disagree.
//
// It is on only when all of these hold:
//   - remote access is on (the web chat exists only behind the proxy);
//   - allowed_users is set (otherwise the proxy verifies no login);
//   - the state directory is outside the tree. Agents mount the tree, so a
//     ledger inside it could be written from a jail and would prove nothing.
func ChatLedgerPath(app *config.App, st state.State) string {
	if !chatConfigured(app) || StateInsideTree(app, st) {
		return ""
	}
	return st.ChatLedger()
}

// chatConfigured is the config half of ChatLedgerPath, for ConfigHash (which
// has no state.State). The other half, the state directory's place relative
// to the tree, does not change for an instance.
func chatConfigured(app *config.App) bool {
	return app.RemoteEnabled() && len(app.Remote.AllowedUsers) > 0
}

// StateInsideTree reports whether the state directory is the tree or inside
// it, comparing real paths where they resolve.
func StateInsideTree(app *config.App, st state.State) bool {
	tree, dir := realPath(app.Tree), realPath(st.Dir)
	rel, err := filepath.Rel(tree, dir)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	// The state directory may not exist yet: resolve its parent instead.
	if r, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(r, filepath.Base(abs))
	}
	return abs
}
