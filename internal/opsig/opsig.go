// Package opsig implements the operator-directive signature protocol:
// wire types, exact-byte statement parsing with duplicate-key rejection,
// and signing/verification via the system ssh-keygen (-Y sign / -Y verify).
// All cryptography is host-side; agents never see this package.
package opsig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	NamespaceDirective = "lever-operator-directive@lever.dev"
	NamespaceAdmin     = "lever-operator-admin@lever.dev"
	// MaxExpiry is the hard cap on directive lifetime, enforced at parse
	// time on both the CLI and broker sides.
	MaxExpiry = 24 * time.Hour
	// maxStatementBytes bounds submitted statements (defence vs log/memory bloat).
	maxStatementBytes = 64 << 10
	maxInstructionLen = 4 << 10
	// clockLeeway absorbs operator-host vs broker-host skew on the
	// not_before bound only. Expiry stays strict (fail closed).
	clockLeeway = 2 * time.Minute
	// maxJSONDepth bounds walkDupes' recursion. In practice maxStatementBytes
	// (64KiB) already limits how deep a document can nest, but an explicit
	// cap is cheap defence-in-depth against a huge-fanout-free, pure-nesting
	// document (e.g. thousands of "[").
	maxJSONDepth = 200
)

var ErrInvalid = errors.New("opsig: invalid")

type Action struct {
	Kind       string          `json:"kind"`
	Tool       string          `json:"tool,omitempty"`
	Op         string          `json:"op,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	ArgBinding string          `json:"arg_binding,omitempty"`
	Uses       int             `json:"uses,omitempty"`
	Text       string          `json:"text,omitempty"`
}

type Target struct {
	CN         string `json:"cn"`
	Generation int    `json:"generation"`
}

type Statement struct {
	V           int    `json:"v"`
	Instance    string `json:"instance"`
	DirectiveID string `json:"directive_id"`
	TargetAgent Target `json:"target_agent"`
	IssuedAt    string `json:"issued_at"`
	NotBefore   string `json:"not_before"`
	ExpiresAt   string `json:"expires_at"`
	Action      Action `json:"action"`
}

type Envelope struct {
	V        int               `json:"v"`
	Instance string            `json:"instance"`
	Op       string            `json:"op"`
	Params   map[string]string `json:"params,omitempty"`
	IssuedAt string            `json:"issued_at"`
}

// RejectDuplicateKeys walks raw as a JSON token stream and fails on any
// object containing the same key twice (encoding/json silently keeps the
// last value, a differential-parsing hazard for signed documents).
func RejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return walkDupes(dec, 0)
}

func walkDupes(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("%w: nesting too deep", ErrInvalid)
	}
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	switch d := t.(type) {
	case json.Delim:
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return fmt.Errorf("%w: %v", ErrInvalid, err)
				}
				key := kt.(string)
				if seen[key] {
					return fmt.Errorf("%w: duplicate key %q", ErrInvalid, key)
				}
				seen[key] = true
				if err := walkDupes(dec, depth+1); err != nil {
					return err
				}
			}
			_, err = dec.Token() // consume '}'
			return err
		case '[':
			for dec.More() {
				if err := walkDupes(dec, depth+1); err != nil {
					return err
				}
			}
			_, err = dec.Token() // consume ']'
			return err
		}
	}
	return nil
}

// parseTime parses an RFC3339 timestamp, failing closed on any malformation.
func parseTime(field, v string) (time.Time, error) {
	ts, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s: %v", ErrInvalid, field, err)
	}
	return ts, nil
}

// ParseStatement validates raw as a directive statement for instance at now.
// raw MUST be the exact bytes the signature was verified over; the caller
// verifies the signature FIRST, then parses the same bytes here.
func ParseStatement(raw []byte, instance string, now time.Time) (Statement, error) {
	if len(raw) > maxStatementBytes {
		return Statement{}, fmt.Errorf("%w: statement too large", ErrInvalid)
	}
	if err := RejectDuplicateKeys(raw); err != nil {
		return Statement{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var st Statement
	if err := dec.Decode(&st); err != nil {
		return Statement{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if st.V != 1 {
		return Statement{}, fmt.Errorf("%w: version %d", ErrInvalid, st.V)
	}
	if st.Instance != instance {
		return Statement{}, fmt.Errorf("%w: instance mismatch", ErrInvalid)
	}
	if st.DirectiveID == "" || len(st.DirectiveID) > 64 {
		return Statement{}, fmt.Errorf("%w: directive_id", ErrInvalid)
	}
	if st.TargetAgent.CN == "" || st.TargetAgent.Generation < 1 {
		return Statement{}, fmt.Errorf("%w: target_agent", ErrInvalid)
	}
	nbf, err := parseTime("not_before", st.NotBefore)
	if err != nil {
		return Statement{}, err
	}
	exp, err := parseTime("expires_at", st.ExpiresAt)
	if err != nil {
		return Statement{}, err
	}
	if _, err := parseTime("issued_at", st.IssuedAt); err != nil {
		return Statement{}, err
	}
	if now.Add(clockLeeway).Before(nbf) {
		return Statement{}, fmt.Errorf("%w: not yet valid", ErrInvalid)
	}
	if !now.Before(exp) {
		return Statement{}, fmt.Errorf("%w: expired", ErrInvalid)
	}
	if exp.Sub(now) > MaxExpiry {
		return Statement{}, fmt.Errorf("%w: expiry beyond %s cap", ErrInvalid, MaxExpiry)
	}
	if err := validateAction(st.Action); err != nil {
		return Statement{}, err
	}
	return st, nil
}

// ValidateAction exports validateAction's action-shape rules for callers
// outside this package (the CLI) that construct an Action client-side and
// want to fail fast — before building/signing a Statement around it — rather
// than discover a malformed action only after the broker rejects the send.
func ValidateAction(a Action) error { return validateAction(a) }

func validateAction(a Action) error {
	switch a.Kind {
	case "tool_call", "approval":
		if a.Tool == "" || a.Op == "" {
			return fmt.Errorf("%w: action needs tool+op", ErrInvalid)
		}
		if a.ArgBinding != "exact" {
			return fmt.Errorf("%w: arg_binding must be \"exact\"", ErrInvalid)
		}
		if a.Uses != 1 {
			return fmt.Errorf("%w: uses must be 1", ErrInvalid)
		}
		if a.Text != "" {
			return fmt.Errorf("%w: bound action carries no free text", ErrInvalid)
		}
	case "instruction":
		if a.Text == "" || len(a.Text) > maxInstructionLen {
			return fmt.Errorf("%w: instruction text", ErrInvalid)
		}
		if a.Tool != "" || a.Op != "" || a.Args != nil {
			return fmt.Errorf("%w: instruction carries no action fields", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown action kind %q", ErrInvalid, a.Kind)
	}
	return nil
}

// ParseEnvelope validates a signed admin-op envelope (list/revoke) with a
// freshness window of ±maxSkew around now.
func ParseEnvelope(raw []byte, instance string, now time.Time, maxSkew time.Duration) (Envelope, error) {
	if len(raw) > maxStatementBytes {
		return Envelope{}, fmt.Errorf("%w: envelope too large", ErrInvalid)
	}
	if err := RejectDuplicateKeys(raw); err != nil {
		return Envelope{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var e Envelope
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if e.V != 1 || e.Instance != instance || e.Op == "" {
		return Envelope{}, fmt.Errorf("%w: envelope fields", ErrInvalid)
	}
	ts, err := parseTime("issued_at", e.IssuedAt)
	if err != nil {
		return Envelope{}, err
	}
	if d := now.Sub(ts); d > maxSkew || d < -maxSkew {
		return Envelope{}, fmt.Errorf("%w: envelope not fresh", ErrInvalid)
	}
	return e, nil
}

// Sign signs msg with the SSH private key at keyPath under namespace using
// the system ssh-keygen. msg goes to stdin; the armored signature comes
// back on stdout. Fixed argv — no caller-controlled flags.
func Sign(keyPath, namespace string, msg []byte) ([]byte, error) {
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-f", keyPath, "-n", namespace, "-q", "-")
	cmd.Stdin = bytes.NewReader(msg)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("opsig: ssh-keygen sign: %v: %s", err, errb.String())
	}
	return out.Bytes(), nil
}

// Verifier verifies operator signatures against an allowed_signers file
// with a fixed expected principal. Only exit status 0 is accepted.
type Verifier struct {
	AllowedSigners string
	Principal      string
}

// Verify checks sig over msg under namespace. The signature is written to a
// private temp file (ssh-keygen requires -s to be a file); msg goes to stdin.
func (v Verifier) Verify(namespace string, msg, sig []byte) error {
	dir, err := os.MkdirTemp("", "lever-opsig-*")
	if err != nil {
		return fmt.Errorf("opsig: %v", err)
	}
	defer os.RemoveAll(dir)
	sigFile := filepath.Join(dir, "d.sig")
	if err := os.WriteFile(sigFile, sig, 0o600); err != nil {
		return fmt.Errorf("opsig: %v", err)
	}
	cmd := exec.Command("ssh-keygen", "-Y", "verify",
		"-f", v.AllowedSigners, "-I", v.Principal, "-n", namespace, "-s", sigFile)
	cmd.Stdin = bytes.NewReader(msg)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &errb, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: signature: %s", ErrInvalid, errb.String())
	}
	return nil
}
