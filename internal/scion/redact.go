package scion

import "github.com/stevegeek/lever/internal/logging"

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
//
// The rules are lever's one redaction rule set, logging.Redact, which every
// daemon log line also goes through.
func RedactSecrets(s string) string {
	return logging.Redact(s)
}
