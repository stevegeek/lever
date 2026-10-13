// Package sessionrec is the host record of when each agent's session last
// started fresh, and which lever skill text was on disk for that agent then.
//
// A resumed session keeps the skill text it loaded before, whatever is on
// disk now, so only this record says which rules a live session follows.
// Lever appends a line when it creates an agent, never on a resume, and the
// remote proxy lets a contact post to an agent only while the skill hash of
// its last fresh start is the hash of the current skill on disk.
//
// The record is a host record like the ledgers (package hostledger): 0600,
// in a directory only the host user can write, never followed through a
// symlink, outside the tree agents mount.
package sessionrec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/skills"
)

// Record is one fresh session start.
type Record struct {
	// Agent is the app name for the manager, else the worker's name.
	Agent string `json:"agent"`
	// SkillHash is skills.ContentHash of the agent's lever skill as it was on
	// disk when the session was created.
	SkillHash string    `json:"skill_hash"`
	Version   string    `json:"version"`
	Started   time.Time `json:"started"`
}

const label = "session record"

// rotateCap is the size past which the record rotates to <path>.1. An agent
// whose last line was dropped has no record, which only refuses contacts
// until it starts fresh again.
const rotateCap = 1 << 20

// ErrUnsafe means the record can be written by another user
// (hostledger.ErrUnsafe).
var ErrUnsafe = hostledger.ErrUnsafe

// SkillRel is the tree-relative path of agent's lever skill: lever-operator
// for the manager (the app name), lever-agent in a declared worker's dir.
func SkillRel(app *config.App, agent string) (string, bool) {
	if agent == app.Name {
		return ".claude/skills/lever-operator/SKILL.md", true
	}
	for _, g := range app.Workers {
		if g.Name == agent {
			return filepath.ToSlash(filepath.Join(g.Dir, ".claude", "skills", "lever-agent", "SKILL.md")), true
		}
	}
	return "", false
}

// SkillHash is skills.ContentHash of agent's lever skill as it is on disk
// now, without the release stamp, so a release that renders the same skill
// text keeps a session fresh. A skill path the agent replaced with a symlink
// out of the tree is refused.
func SkillHash(app *config.App, agent string) (string, error) {
	rel, ok := SkillRel(app, agent)
	if !ok {
		return "", fmt.Errorf("%s: %q is not the manager or a declared worker", label, agent)
	}
	b, err := fsutil.ReadInTree(app.Tree, rel)
	if err != nil {
		return "", fmt.Errorf("%s: reading %s: %w", label, rel, err)
	}
	return skills.ContentHash(b), nil
}

// Append records r at path. The directory must be safe (hostledger.CheckDir);
// a lock beside the file keeps apply and the broker from both rotating it.
func Append(path string, r Record) error {
	if err := hostledger.CheckDir(filepath.Dir(path), label); err != nil {
		return err
	}
	f := &hostledger.File{Path: path, Label: label, Cap: rotateCap,
		Lock: func() (func(), error) { return hostledger.LockFile(path + ".lock") }}
	return f.Append(r)
}

// Latest returns the last record of each agent, from path and its rotated
// copy. A missing directory or file is no records. An unsafe directory or
// file is ErrUnsafe: its lines could come from anyone.
func Latest(path string) (map[string]Record, error) {
	out := map[string]Record{}
	if err := hostledger.CheckDir(filepath.Dir(path), label); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	for _, p := range []string{path + ".1", path} {
		err := hostledger.ReadFile(p, label, func(line []byte) {
			var r Record
			if json.Unmarshal(line, &r) != nil || r.Agent == "" || r.SkillHash == "" {
				return
			}
			if old, ok := out[r.Agent]; !ok || !r.Started.Before(old.Started) {
				out[r.Agent] = r
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
