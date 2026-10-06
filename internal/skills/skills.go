// Package skills holds the framework-authored SKILL.md files scaffolded into
// instance trees by `lever init`. Content is embedded; the only templating is
// the {{LEVER_VERSION}} frontmatter stamp (the version is passed IN by the
// caller — this package must not import internal/cli) and both skills'
// {{VERIFIED_CHAT}} on/off.
package skills

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"regexp"
	"strings"
)

//go:embed lever-operator/SKILL.md
var operatorSrc string

//go:embed lever-agent/SKILL.md
var agentSrc string

// Operator returns the rendered manager skill (lever-operator).
// verifiedChat is whether the instance has verified web chat on; it fills
// {{VERIFIED_CHAT}}, so an agent image with no verify tool reads every user:
// message as data only where verified chat (and so a contact) can exist.
// agentMessages (remote.agent_messages) keeps the "agent-messages on" blocks,
// else the "off" ones; files (remote.files) does the same for the "files"
// blocks. Off renders the text as it was before those blocks.
func Operator(version string, verifiedChat, agentMessages, files bool) []byte {
	return renderChat(pickBlocks(pick(operatorSrc, agentMessages), files, filesOn, filesOff), version, verifiedChat)
}

// Agent returns the rendered worker skill (lever-agent), with
// {{VERIFIED_CHAT}} and the blocks as in Operator.
func Agent(version string, verifiedChat, agentMessages, files bool) []byte {
	return renderChat(pickBlocks(pick(agentSrc, agentMessages), files, filesOn, filesOff), version, verifiedChat)
}

var (
	amOn     = block("agent-messages", "on")
	amOff    = block("agent-messages", "off")
	filesOn  = block("files", "on")
	filesOff = block("files", "off")
)

// block matches one marked block of a skill, markers included.
func block(name, state string) *regexp.Regexp {
	return regexp.MustCompile(`(?ms)^<!-- lever:` + name + ` ` + state + ` -->\n(.*?)^<!-- /lever:` + name + ` ` + state + ` -->\n`)
}

// pick keeps the agent-messages variant on (or off).
func pick(src string, on bool) string { return pickBlocks(src, on, amOn, amOff) }

// pickBlocks keeps one variant of each block pair and drops the other
// variant and every marker line.
func pickBlocks(src string, on bool, onRE, offRE *regexp.Regexp) string {
	keep, drop := onRE, offRE
	if !on {
		keep, drop = offRE, onRE
	}
	return keep.ReplaceAllString(drop.ReplaceAllString(src, ""), "$1")
}

func renderChat(src, version string, verifiedChat bool) []byte {
	state := "off"
	if verifiedChat {
		state = "on"
	}
	return []byte(strings.ReplaceAll(string(render(src, version)), "{{VERIFIED_CHAT}}", state))
}

func render(src, version string) []byte {
	return []byte(strings.ReplaceAll(src, "{{LEVER_VERSION}}", version))
}

// Hash is the digest used for scaffold hash-guarding (recorded in
// .lever-state/skills.json and compared by init/doctor).
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// LeverVersion extracts the `lever-version:` frontmatter stamp from a
// scaffolded (or adopted) SKILL.md. Empty when the stamp is absent — a
// pre-frontmatter or hand-built file, which callers treat as an unknown
// (stale) baseline. Only the frontmatter block (up to the second `---`) is
// scanned, so body text mentioning the key cannot spoof it.
func LeverVersion(b []byte) string {
	inFrontmatter := false
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if inFrontmatter {
				return "" // frontmatter closed without the stamp
			}
			inFrontmatter = true
			continue
		}
		if !inFrontmatter {
			continue
		}
		if v, ok := strings.CutPrefix(trimmed, "lever-version:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
