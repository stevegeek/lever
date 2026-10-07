package host

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/skills"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
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
//
// The disk is not the whole story: a resumed session keeps the skill text it
// loaded before. That part is checked per agent, per post, by the remote
// proxy (contactSession), because only then is it known which agents exist
// and whether this bring-up started them fresh.
func checkContactGate(app *config.App, st state.State) error {
	if !app.RemoteEnabled() || len(app.Remote.LoginsWithTier(config.TierContact)) == 0 {
		return nil
	}
	if brokerctl.StateInsideTree(app, st) {
		return fmt.Errorf("remote: contact logins need host records agents cannot write, but the state directory %s is inside the tree %s; "+
			"point `tree:` at a subdirectory that does not contain it, or remove the contact logins", st.Dir, app.Tree)
	}
	stale, err := staleSkillList(app, st)
	if err != nil {
		return fmt.Errorf("remote: contact logins need current lever skills, and they cannot be checked: %w", err)
	}
	if len(stale) > 0 {
		return fmt.Errorf("remote: contact logins need every agent on lever %s's skills, which verify each message against host records; "+
			"these are not: %s. Run `lever init` (or `lever init --force` over an edited skill), then apply again",
			cli.Version, strings.Join(stale, ", "))
	}
	return nil
}

// staleSkillList names each agent skill on disk that is not this version's
// (an adopted skill counts when it was adopted at this version).
func staleSkillList(app *config.App, st state.State) ([]string, error) {
	results, err := syncSkills(app, st, false, true)
	if err != nil {
		return nil, err
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
	return stale, nil
}

// staleSkills is staleSkillList for a warning: a check that fails names
// nothing (doctor reports the skills in full).
func staleSkills(app *config.App, st state.State) []string {
	stale, _ := staleSkillList(app, st)
	return stale
}

// contactSession is the remote proxy's check before a contact's post reaches
// agent (remoteproxy.Config.ContactSession). It allows the post only when
// all of these hold:
//
//   - the state directory is outside the tree (the session record is a host
//     record agents cannot write);
//   - agent's lever skill on disk is this version's (or adopted at this
//     version), the same test as checkContactGate;
//   - the record of agent's last fresh session start (package sessionrec)
//     names that same skill text.
//
// The last is what checkContactGate cannot see from the disk: a resumed
// session keeps the skill text it loaded before, and an older skill trusts a
// lever marker a contact can type. A session that started before the skill
// changed, or one lever has no record of starting (created by an older
// lever), is refused until it starts fresh.
func contactSession(app *config.App, st state.State, agent string) error {
	if brokerctl.StateInsideTree(app, st) {
		return errors.New("the state directory is inside the tree")
	}
	rel, ok := sessionrec.SkillRel(app, agent)
	if !ok {
		return fmt.Errorf("%q is not an agent of this instance", agent)
	}
	var want []byte
	for _, t := range skillTargets(app) {
		if t.relPath == rel {
			want = t.content
		}
	}
	onDisk, err := fsutil.ReadInTree(app.Tree, rel)
	if err != nil {
		return errors.New("its lever skill cannot be read")
	}
	hash := skills.Hash(onDisk)
	if hash != skills.Hash(want) {
		adopted, err := loadAdoptedState(st)
		if err != nil || adopted[rel] != hash || skills.LeverVersion(onDisk) != cli.Version {
			return fmt.Errorf("its lever skill is not lever %s's", cli.Version)
		}
	}
	recs, err := sessionrec.Latest(st.Sessions())
	if err != nil {
		return errors.New("the session record cannot be read")
	}
	r, ok := recs[agent]
	switch {
	case !ok:
		return errors.New("lever has no record of its session starting fresh")
	case r.SkillHash != skills.ContentHash(onDisk) && r.SkillHash != hash:
		// ContentHash leaves out the release stamp, so a release that
		// renders the same skill text keeps the session fresh; the full
		// hash is what records written before 0.33.1 hold.
		return staleSessionError{fmt.Sprintf("at %s, lever %s", r.Started.UTC().Format(time.RFC3339), r.Version)}
	}
	return nil
}

// staleSessionError is contactSession's refusal of a session that started
// before the current skill. Its text carries the start time and version;
// reason is the part shared by every such agent, which the bring-up
// warning groups on (after an upgrade every agent has its own start time).
type staleSessionError struct{ detail string }

const staleSessionReason = "its session started before its current skill was written"

func (e staleSessionError) Error() string { return staleSessionReason + " (" + e.detail + ")" }

// contactRefusalReason is err's text with any per-agent detail dropped.
func contactRefusalReason(err error) string {
	var stale staleSessionError
	if errors.As(err, &stale) {
		return staleSessionReason
	}
	return err.Error()
}

// contactAgents is every agent some contact login lists, in config order.
func contactAgents(app *config.App) []string {
	var out []string
	for _, u := range app.Remote.AllowedUsers {
		if u.EffectiveTier() != config.TierContact {
			continue
		}
		for _, a := range u.Agents {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// printContactSessionWarnings names, at every bring-up, each agent a contact
// lists whose session contactSession would refuse, and how to fix it. It is
// a warning, not a refusal: an agent that does not exist yet (a worker not
// dispatched, a manager this bring-up creates) has no record either, and
// the proxy refuses the posts anyway.
//
// With verified chat on and no contact (checkContactGate refuses stale
// skills when there is one), it also warns about stale skills: a 0.27
// manager skill reads the manager's notes to itself, now marked and
// verifying as lever, as data.
func printContactSessionWarnings(cmd *cobra.Command, app *config.App, st state.State) {
	if !app.RemoteEnabled() {
		return
	}
	if brokerctl.ChatConfigured(app) && len(app.Remote.LoginsWithTier(config.TierContact)) == 0 {
		if stale := staleSkills(app, st); len(stale) > 0 {
			cmd.PrintErrf("lever: warning: these skills are not lever %s's: %s; until `lever init` refreshes them, the agents on them treat some lever messages (the manager's notes to itself) as data\n",
				cli.Version, strings.Join(stale, ", "))
		}
	}
	var blocked []blockedContactAgent
	for _, a := range contactAgents(app) {
		if err := contactSession(app, st, a); err != nil {
			blocked = append(blocked, blockedContactAgent{a, contactRefusalReason(err)})
		}
	}
	if line := contactSessionWarning(app.Name, blocked); line != "" {
		cmd.PrintErrln(line)
	}
}

// blockedContactAgent is one agent contactSession refuses, with its reason.
type blockedContactAgent struct{ name, reason string }

// maxWarnedContactAgents bounds the agent names one warning lists.
const maxWarnedContactAgents = 8

// contactSessionWarning words every refused agent as ONE warning line: a
// production box lists many contact agents, and a line per agent at every
// bring-up buried the rest of the output. The agents are grouped by reason,
// at most maxWarnedContactAgents names are listed, and the line ends with
// the fix for each kind present (a worker heals at its next fresh create;
// the manager needs a fresh start). Empty when nothing is blocked. The
// names come from the config and are sanitized anyway: the line reaches
// the terminal raw.
func contactSessionWarning(manager string, blocked []blockedContactAgent) string {
	if len(blocked) == 0 {
		return ""
	}
	var reasons []string
	byReason := map[string][]string{}
	hasManager, hasWorker := false, false
	for i, b := range blocked {
		if b.name == manager {
			hasManager = true
		} else {
			hasWorker = true
		}
		if i >= maxWarnedContactAgents {
			continue
		}
		if _, ok := byReason[b.reason]; !ok {
			reasons = append(reasons, b.reason)
		}
		byReason[b.reason] = append(byReason[b.reason], termsafe.Sanitize(b.name))
	}
	groups := make([]string, 0, len(reasons)+1)
	for _, r := range reasons {
		groups = append(groups, strings.Join(byReason[r], ", ")+" ("+termsafe.Sanitize(r)+")")
	}
	if more := len(blocked) - maxWarnedContactAgents; more > 0 {
		groups = append(groups, fmt.Sprintf("and %d more", more))
	}
	var how []string
	if hasWorker {
		how = append(how, "a worker takes contact messages once the broker next creates it fresh")
	}
	if hasManager {
		how = append(how, fmt.Sprintf("for the manager %s, run `lever up --fresh` (back up the manager's conversation first), or let this bring-up create it",
			termsafe.Sanitize(manager)))
	}
	if len(blocked) == 1 {
		return fmt.Sprintf("lever: warning: remote: contacts cannot post to %s until its session starts fresh; %s",
			groups[0], strings.Join(how, "; "))
	}
	return fmt.Sprintf("lever: warning: remote: contacts cannot post to %d agents until their sessions start fresh: %s; %s",
		len(blocked), strings.Join(groups, "; "), strings.Join(how, "; "))
}
