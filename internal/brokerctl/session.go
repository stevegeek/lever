package brokerctl

import (
	"errors"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/sessionrec"
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
