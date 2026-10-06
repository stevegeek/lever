package brokerctl

import (
	"slices"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// Files is remote.files as the broker enforces it: the tree and each
// agent's workspace, the limits, shares off, who may receive files from
// which agent (never a login with files: false),
// and the ledger directory ("" when the state directory is inside the tree,
// where an agent could write a record).
func Files(app *config.App, st state.State) broker.FilesConfig {
	if !app.FilesOn() {
		return broker.FilesConfig{}
	}
	c := broker.FilesConfig{Enabled: true, Tree: app.Tree, Workspaces: app.AgentWorkspaces(),
		MaxBytes: app.EffectiveFilesMaxBytes(), Extensions: app.EffectiveFilesExtensions(),
		NoShares: !app.FilesSharesOn()}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierOperator && u.FilesAllowed() {
			c.Operators = append(c.Operators, u.Login)
		}
	}
	if !StateInsideTree(app, st) {
		c.LedgerDir = st.FilesLedger()
	}
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() == config.TierContact && u.FilesAllowed() {
			c.Contacts = append(c.Contacts, broker.FileContactEntry{Login: u.Login, Agents: slices.Clone(u.Agents)})
		}
	}
	return c
}
