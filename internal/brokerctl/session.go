package brokerctl

import (
	"errors"
	"fmt"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/sessionrec"
	"github.com/stevegeek/lever/internal/skills"
	"github.com/stevegeek/lever/internal/state"
)

// errSessionInsideTree means no session record is kept: the state directory
// is inside the tree, where an agent could write it (and contacts are
// refused anyway, see checkContactGate).
var errSessionInsideTree = errors.New("session record: the state directory is inside the tree; not recorded")

// BeginSession notes the skill on disk for agent now, before lever creates
// the agent, and returns what records that fresh start (sessionrec) once the
// create succeeded. The hash is taken before the create so a skill written
// while the agent boots is never recorded as the one it read. A skill that
// cannot be read is not recorded: the agent then has no record, and the
// remote proxy refuses contacts' posts to it.
func BeginSession(app *config.App, st state.State, version, agent string) (commit func() error) {
	if StateInsideTree(app, st) {
		return func() error { return errSessionInsideTree }
	}
	hash, herr := sessionrec.SkillHash(app, agent)
	return func() error {
		if herr != nil {
			return herr
		}
		return sessionrec.Append(st.Sessions(), sessionrec.Record{
			Agent: agent, SkillHash: hash, Version: version, Started: time.Now().UTC(),
		})
	}
}

// UncheckedSkill is the session record's skill hash for a fresh start lever
// made on its own (lever#202) while the skill on disk was not the one the
// agent's last recorded session started with. Like a restored
// conversation's (lever#198), it matches no skill, so the remote proxy
// refuses contacts' posts to the agent until it next starts fresh at the
// operator's word.
const UncheckedSkill = "unchecked (fresh start lever made on its own)"

// BeginUnattendedSession is BeginSession for a fresh start lever makes on
// its own, not at the operator's word: the fresh start of a manager with no
// conversation to continue (lever#202). The agent can bring that start
// about, and the tree it writes holds the skill, so the start must not
// re-baseline the session on a skill the agent wrote. commit records the
// new session only when the skill on disk now is the one agent's last
// recorded session started with (its content hash, or its full hash in a
// record written before 0.33.1): the record then names the same skill it
// did. Otherwise (a changed or unreadable skill, or a last record that names
// none, as a restored conversation's does) BeginUnattendedSession records,
// right away and before anything is deleted, a session that matches no
// skill (UncheckedSkill): contacts stay refused until `lever up --fresh`,
// even if the old skill text is put back, and commit returns why without
// writing. An agent with no record keeps none (contacts are refused). An
// error (the record cannot be read, or the refusing line cannot be
// written) means lever must not make the start.
func BeginUnattendedSession(app *config.App, st state.State, version, agent string) (commit func() error, err error) {
	if StateInsideTree(app, st) {
		return func() error { return errSessionInsideTree }, nil
	}
	recs, err := sessionrec.Latest(st.Sessions())
	if err != nil {
		return nil, fmt.Errorf("reading the session record: %w", err)
	}
	prev, ok := recs[agent]
	if !ok {
		return func() error {
			return errors.New("lever has no record of its last session, so it has no skill to compare with")
		}, nil
	}
	rel, ok := sessionrec.SkillRel(app, agent)
	if !ok {
		return nil, fmt.Errorf("%q is not the manager or a declared worker", agent)
	}
	b, rerr := fsutil.ReadInTree(app.Tree, rel)
	if rerr == nil && (prev.SkillHash == skills.ContentHash(b) || prev.SkillHash == skills.Hash(b)) {
		hash := skills.ContentHash(b)
		return func() error {
			return sessionrec.Append(st.Sessions(), sessionrec.Record{
				Agent: agent, SkillHash: hash, Version: version, Started: time.Now().UTC(),
			})
		}, nil
	}
	if err := sessionrec.Append(st.Sessions(), sessionrec.Record{
		Agent: agent, SkillHash: UncheckedSkill, Version: version, Started: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}
	why := "its lever skill on disk is not the one its last recorded session started with"
	if rerr != nil {
		why = "its lever skill cannot be read"
	}
	return func() error { return errors.New(why) }, nil
}
