package brokerctl

import (
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// ChatLedgerPath is the verified-chat ledger both the remote proxy (writer)
// and the broker (reader) use, or "" when verified chat is off. One function
// decides for both, so they can never disagree. It is on only when remote
// access is on, allowed_users is set (otherwise no login is verified), and
// the state directory is outside the tree (a ledger inside it could be
// written from a jail and would prove nothing).
func ChatLedgerPath(app *config.App, st state.State) string {
	if !ChatConfigured(app) || StateInsideTree(app, st) {
		return ""
	}
	return st.ChatLedger()
}

// ChatConfigured is the config half of ChatLedgerPath, for ConfigHash. The
// other half, the state directory's place, does not change for an instance.
func ChatConfigured(app *config.App) bool {
	return app.RemoteEnabled() && len(app.Remote.AllowedUsers) > 0
}

// WebSenders is the envelope sender of every remote sign-in, or none when
// remote access is off. The broker looks their messages up in the chat
// ledger only. With no allowed_users it is the unnamed operator, whose posts
// cannot be verified.
func WebSenders(app *config.App) []string {
	if !app.RemoteEnabled() {
		return nil
	}
	return app.Remote.WebSenders()
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
