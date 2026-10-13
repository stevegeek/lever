package brokerctl

import (
	"errors"
	"fmt"
	"time"

	"github.com/stevegeek/lever/internal/apply"
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
// operator's word. Like every record it wins by its start time
// (sessionrec.Latest), so a host clock stepped back past the previous
// record's start lets that one win again; the restore's has the same limit.
const UncheckedSkill = "unchecked (fresh start lever made on its own)"

// BeginUnattendedSession is BeginSession for a fresh start lever makes on
// its own, not at the operator's word: the fresh start of a manager with no
// conversation to continue (lever#202). The agent can bring that start
// about, and the tree it writes holds the skill, so the start must not
// re-baseline the session on a skill the agent wrote.
//
// It is called before the record is deleted. When the skill on disk is not
// the one agent's last recorded session started with (its content hash, or
// its full hash in a record written before 0.33.1), or cannot be read, or
// that record names no skill (a restored conversation's, or an earlier
// unchecked start's), it records at once a session matching no skill
// (UncheckedSkill) and says why in Refused: contacts then stay refused, even
// if the old skill text is put back, until a fresh start at the operator's
// word. Begin runs after the delete, right before the create, when the old
// container can no longer write the skill: it reads the skill again, and
// its commit records that reading only when nothing was refused and it
// still matches; otherwise Begin records UncheckedSkill again (a skill
// changed while the old container stopped) and its commit returns why. An
// agent with no record keeps none (contacts are refused). An error (the
// record cannot be read, or a refusing line cannot be written) means lever
// must not make the start, or, from Begin, the create.
func BeginUnattendedSession(app *config.App, st state.State, version, agent string) (apply.UnattendedSession, error) {
	if StateInsideTree(app, st) {
		return apply.UnattendedSession{Begin: func() (func() error, error) {
			return func() error { return errSessionInsideTree }, nil
		}}, nil
	}
	recs, err := sessionrec.Latest(st.Sessions())
	if err != nil {
		return apply.UnattendedSession{}, fmt.Errorf("reading the session record: %w", err)
	}
	prev, ok := recs[agent]
	if !ok {
		return apply.UnattendedSession{Begin: func() (func() error, error) {
			return func() error {
				return errors.New("lever has no record of its last session, so it has no skill to compare with")
			}, nil
		}}, nil
	}
	rel, ok := sessionrec.SkillRel(app, agent)
	if !ok {
		return apply.UnattendedSession{}, fmt.Errorf("%q is not the manager or a declared worker", agent)
	}
	// check reads the skill now: its content hash when it is the one the
	// last session started with, else why not.
	check := func() (string, error) {
		b, err := fsutil.ReadInTree(app.Tree, rel)
		if err != nil {
			return "", errors.New("its lever skill cannot be read")
		}
		if prev.SkillHash != skills.ContentHash(b) && prev.SkillHash != skills.Hash(b) {
			return "", errors.New("its lever skill on disk is not the one its last recorded session started with")
		}
		return skills.ContentHash(b), nil
	}
	fence := func() error {
		return sessionrec.Append(st.Sessions(), sessionrec.Record{
			Agent: agent, SkillHash: UncheckedSkill, Version: version, Started: time.Now().UTC(),
		})
	}
	_, refused := check()
	if refused != nil {
		if err := fence(); err != nil {
			return apply.UnattendedSession{}, err
		}
	}
	return apply.UnattendedSession{Refused: refused, Begin: func() (func() error, error) {
		if refused != nil {
			return func() error { return refused }, nil
		}
		hash, why := check()
		if why != nil {
			if err := fence(); err != nil {
				return nil, err
			}
			return func() error { return why }, nil
		}
		return func() error {
			return sessionrec.Append(st.Sessions(), sessionrec.Record{
				Agent: agent, SkillHash: hash, Version: version, Started: time.Now().UTC(),
			})
		}, nil
	}}, nil
}
