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

// Agent returns the rendered grove skill (lever-agent).
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
