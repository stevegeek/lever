package scion

import "regexp"

// envValueRE matches a container-runtime env argument as scion's runtime
// error echoes it — `-e KEY=VALUE`, `-e=KEY=VALUE`, `--env KEY=VALUE`,
// `--env=KEY=VALUE` — capturing everything up to the value so the key
// survives and the value can be elided. A value runs to the next space; a
// value scion quotes is not something the podman argv echo produces.
var envValueRE = regexp.MustCompile(`(^|\s)(-e|--env)([ =])([A-Za-z_][A-Za-z0-9_]*=)\S*`)

// anthropicTokenRE matches the Anthropic credential shapes lever handles —
// `sk-ant-oat01-…` (a Claude Code OAuth token) and `sk-ant-api03-…` (an API
// key) — wherever one appears, so a token that reached the text outside an
// env argument is masked too.
var anthropicTokenRE = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`)

// RedactSecrets masks credentials in text scion produced (lever#37). scion's
// runtime error repeats the whole `podman run` argv it executed, `-e
// KEY=VALUE` pairs included, and in subscription mode one of those is the
// operator's OAuth token; run folds that stderr into its error, and the
// broker audits the error verbatim. Every env VALUE is elided (the key
// stays, which is what an operator reads), and any Anthropic-shaped token
// is masked on its own — belt and braces, since the argv shape is scion's
// choice and may change. Applied by clean, so every scion error and output
// lever hands on is covered; the broker's audit applies it again as a
// backstop for text that never went through the client.
func RedactSecrets(s string) string {
	s = envValueRE.ReplaceAllString(s, "${1}${2}${3}${4}***")
	return anthropicTokenRE.ReplaceAllString(s, "sk-ant-***")
}
