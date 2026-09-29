package host

import (
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// checkContactGate refuses a bring-up that would let a contact (a remote
// login with tier contact) reach agents that cannot tell the contact's words
// from lever's.
//
// Who wrote a message is decided only by host records (message_verify); the
// remote proxy no longer filters what a contact types. That is safe only
// while both hold:
//
//   - the host records exist: with the state directory inside the tree there
//     is no chat ledger and no sent ledger, and an agent could not verify a
//     contact's post as a contact's;
//   - every agent's skill is this version's: an older skill trusts a lever
//     marker on a message that fails to verify, and a contact can type one
//     (and make its verify fail by making the agent read it late).
//
// An adopted (owner-customized) skill counts as current only when it was
// adopted at this version.
func checkContactGate(app *config.App, st state.State) error {
	if !app.RemoteEnabled() || len(app.Remote.LoginsWithTier(config.TierContact)) == 0 {
		return nil
	}
	if brokerctl.StateInsideTree(app, st) {
		return fmt.Errorf("remote: contact logins need host records agents cannot write, but the state directory %s is inside the tree %s; "+
			"point `tree:` at a subdirectory that does not contain it, or remove the contact logins", st.Dir, app.Tree)
	}
	results, err := syncSkills(app, st, false, true)
	if err != nil {
		return fmt.Errorf("remote: contact logins need current lever skills, and they cannot be checked: %w", err)
	}
	var stale []string
	for _, r := range results {
		switch {
		case r.Action == skillUnchanged:
		case r.Action == skillAdopted && r.AdoptedVersion == cli.Version:
		default:
			stale = append(stale, r.RelPath+" ("+string(r.Action)+")")
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("remote: contact logins need every agent on lever %s's skills, which verify each message against host records; "+
			"these are not: %s. Run `lever init` (or `lever init --force` over an edited skill), then apply again",
			cli.Version, strings.Join(stale, ", "))
	}
	return nil
}
