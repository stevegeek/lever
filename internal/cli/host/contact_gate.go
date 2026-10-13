package host

import (
	"context"
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
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/skills"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
)

// checkContactGate refuses a bring-up that would let a contact (a remote login
// with tier contact) reach agents that cannot tell the contact's words from
// lever's.
//
// Who wrote a message is decided only by host records (message_verify); the
// proxy does not filter what a contact types. That is safe only while both
// hold:
//
//   - the host records exist: with the state directory inside the tree there
//     is no chat or sent ledger, and an agent could not verify a contact's
//     post as a contact's;
//   - every agent's skill is this version's (an adopted skill only if adopted
//     at this version): an older skill trusts a lever marker on a message that
//     fails to verify, and a contact can type one.
//
// With remote.agent_messages on, it also refuses a scion.source checkout older
// than config.AgentMessagesScionFloor: the contact filter trusts the sender
// the hub sets, and an older hub lets an agent post as its contact.
//
// A resumed session keeps the skill text it loaded before; the proxy checks
// that per agent, per post (contactSession).
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
	// A scion.source checkout needs git, so it is checked here (config load checks
	// a scion.version). A checkout or shallow clone without the floor commit is
	// refused; a path git cannot answer for is doctor's warning, as a
	// scion.binary is.
	if app.AgentMessagesOn() && app.Scion.Version == "" && app.Scion.Source != "" {
		if bad, note := agentMessagesScion(app, scionSourceHasCommit); bad {
			return fmt.Errorf("remote: agent_messages needs scion %s (2026-08-28) or later, where the hub sets a message's sender itself; %s. "+
				"Update the checkout, or turn remote.agent_messages off", config.AgentMessagesScionFloor[:8], note)
		}
	}
	return nil
}

var scionSourceHasCommit = func(source, commit string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return scionbin.SourceHasCommit(ctx, proc.RealRunner{}, source, commit)
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

// staleSkills is staleSkillList for a warning: a failed check names nothing
// (doctor reports the skills in full).
func staleSkills(app *config.App, st state.State) []string {
	stale, _ := staleSkillList(app, st)
	return stale
}

// contactSession is the proxy's check before a contact's post reaches agent
// (remoteproxy.Config.ContactSession). It allows the post only when all hold:
//
//   - the state directory is outside the tree (the session record is a host
//     record agents cannot write);
//   - agent's skill on disk is this version's, as for checkContactGate;
//   - the record of agent's last fresh session start (package sessionrec)
//     names that same skill text.
//
// The last is what the disk cannot show: a resumed session keeps the skill it
// loaded, and an older skill trusts a lever marker a contact can type. A
// session started before the skill changed, or with no record, is refused
// until it starts fresh.
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
		return errNoSessionRecord
	case r.SkillHash == restoredUnknownSkill:
		return errors.New("its conversation was restored from a backup that does not say which skill it started with")
	case r.SkillHash == brokerctl.UncheckedSkill:
		return errors.New("lever started it fresh on its own while its skill differed from its last session's")
	case r.SkillHash != skills.ContentHash(onDisk) && r.SkillHash != hash:
		// ContentHash leaves out the release stamp, so a release that renders the same
		// skill text keeps the session fresh; older records hold the full hash.
		return staleSessionError{fmt.Sprintf("at %s, lever %s", r.Started.UTC().Format(time.RFC3339), r.Version)}
	}
	return nil
}

var errNoSessionRecord = errors.New("lever has no record of its session starting fresh")

// staleSessionError's text carries the start time and version; reason is the
// part shared by every such agent, which the bring-up warning groups on.
type staleSessionError struct{ detail string }

const staleSessionReason = "its session started before its current skill was written"

func (e staleSessionError) Error() string { return staleSessionReason + " (" + e.detail + ")" }

func contactRefusalReason(err error) string {
	var stale staleSessionError
	if errors.As(err, &stale) {
		return staleSessionReason
	}
	return err.Error()
}

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
// lists whose session contactSession would refuse, and the fix. A warning, not
// a refusal: an agent not created yet has no record either, and the proxy
// refuses the posts anyway.
//
// With verified chat on and no contact, it also warns about stale skills: an
// older manager skill reads the manager's notes to itself, marked and
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

type blockedContactAgent struct{ name, reason string }

const maxWarnedContactAgents = 8

// contactSessionWarning words every refused agent as one line, grouped by
// reason, with at most maxWarnedContactAgents names and the fix for each kind
// present (a worker heals at its next fresh create; the manager needs a fresh
// start). Empty when nothing is blocked. Names are sanitized: the line reaches
// the terminal raw.
func contactSessionWarning(manager string, blocked []blockedContactAgent) string {
	if len(blocked) == 0 {
		return ""
	}
	var reasons []string
	byReason := map[string][]string{}
	hasManager, hasWorker, managerNoRecord := false, false, false
	for i, b := range blocked {
		if b.name == manager {
			hasManager = true
			managerNoRecord = b.reason == errNoSessionRecord.Error()
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
		create := ""
		if managerNoRecord {
			create = ", or let this bring-up create it"
		}
		how = append(how, fmt.Sprintf("for the manager %s, run `lever up --fresh` (it backs the conversation up first; --keep-conversation does not help here, the kept conversation keeps the skill it started with)%s",
			termsafe.Sanitize(manager), create))
	}
	if len(blocked) == 1 {
		return fmt.Sprintf("lever: warning: remote: contacts cannot post to %s until its session starts fresh; %s",
			groups[0], strings.Join(how, "; "))
	}
	return fmt.Sprintf("lever: warning: remote: contacts cannot post to %d agents until their sessions start fresh: %s; %s",
		len(blocked), strings.Join(groups, "; "), strings.Join(how, "; "))
}
