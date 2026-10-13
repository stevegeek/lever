package scion

import "github.com/stevegeek/lever/internal/logging"

// RedactSecrets masks credentials in text scion produced. scion's runtime
// error repeats the whole podman run argv, -e KEY=VALUE pairs included, and in
// subscription mode one is the operator's OAuth token. Every env value is
// elided (the key stays), and any Anthropic-shaped token is masked on its own
// in case the argv shape changes. cleanErr applies it to every scion error;
// the broker's audit applies it again as a backstop.
//
// The rules are logging.Redact's, which every daemon log line also goes
// through.
func RedactSecrets(s string) string {
	return logging.Redact(s)
}
