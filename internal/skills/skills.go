// Package skills holds the framework-authored SKILL.md files scaffolded into
// instance trees by `lever init`. Content is embedded; the only templating is
// the {{LEVER_VERSION}} frontmatter stamp (the version is passed IN by the
// caller — this package must not import internal/cli).
package skills

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"strings"
)

//go:embed lever-operator/SKILL.md
var operatorSrc string

//go:embed lever-agent/SKILL.md
var agentSrc string

// Operator returns the rendered manager skill (lever-operator).
func Operator(version string) []byte { return render(operatorSrc, version) }

// Agent returns the rendered worker skill (lever-agent).
func Agent(version string) []byte { return render(agentSrc, version) }

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
