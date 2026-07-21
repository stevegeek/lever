package opsig

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// genKey creates a fresh ed25519 SSH keypair in dir and returns
// (privPath, allowedSignersPath) with principal "operator@testinst".
func genKey(t *testing.T, dir string) (string, string) {
	t.Helper()
	priv := filepath.Join(dir, "opkey")
	out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-C", "op", "-q").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(priv + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(pub)) // type key comment
	as := filepath.Join(dir, "allowed_signers")
	line := "operator@testinst " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(as, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return priv, as
}

func validStatement(now time.Time) Statement {
	return Statement{
		V: 1, Instance: "testinst", DirectiveID: "11111111-2222-4333-8444-555555555555",
		TargetAgent: Target{CN: "kb-manager", Generation: 1},
		IssuedAt:    now.Format(time.RFC3339),
		NotBefore:   now.Add(-time.Minute).Format(time.RFC3339),
		ExpiresAt:   now.Add(10 * time.Minute).Format(time.RFC3339),
		Action:      Action{Kind: "instruction", Text: "hello"},
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	priv, as := genKey(t, t.TempDir())
	msg, _ := json.Marshal(validStatement(time.Now()))
	sig, err := Sign(priv, NamespaceDirective, msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !bytes.Contains(sig, []byte("BEGIN SSH SIGNATURE")) {
		t.Fatalf("not an armored signature: %.60s", sig)
	}
	v := Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(NamespaceDirective, msg, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyRejectsTamperAndWrongNamespaceAndPrincipal(t *testing.T) {
	priv, as := genKey(t, t.TempDir())
	msg, _ := json.Marshal(validStatement(time.Now()))
	sig, err := Sign(priv, NamespaceDirective, msg)
	if err != nil {
		t.Fatal(err)
	}
	v := Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(NamespaceDirective, append(msg, ' '), sig); err == nil {
		t.Fatal("tampered message verified")
	}
	if err := v.Verify(NamespaceAdmin, msg, sig); err == nil {
		t.Fatal("wrong namespace verified")
	}
	if err := (Verifier{AllowedSigners: as, Principal: "other@x"}).Verify(NamespaceDirective, msg, sig); err == nil {
		t.Fatal("wrong principal verified")
	}
}

func TestParseStatementValid(t *testing.T) {
	now := time.Now()
	raw, _ := json.Marshal(validStatement(now))
	st, err := ParseStatement(raw, "testinst", now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.TargetAgent.CN != "kb-manager" || st.Action.Kind != "instruction" {
		t.Fatalf("bad parse: %+v", st)
	}
}

func TestParseStatementRejections(t *testing.T) {
	now := time.Now()
	base := validStatement(now)
	mutate := func(f func(*Statement)) []byte {
		s := base
		f(&s)
		b, _ := json.Marshal(s)
		return b
	}
	cases := map[string][]byte{
		"wrong instance":   mutate(func(s *Statement) { s.Instance = "other" }),
		"bad version":      mutate(func(s *Statement) { s.V = 2 }),
		"empty id":         mutate(func(s *Statement) { s.DirectiveID = "" }),
		"empty cn":         mutate(func(s *Statement) { s.TargetAgent.CN = "" }),
		"zero generation":  mutate(func(s *Statement) { s.TargetAgent.Generation = 0 }),
		"malformed expiry": mutate(func(s *Statement) { s.ExpiresAt = "tomorrow" }),
		"malformed nbf":    mutate(func(s *Statement) { s.NotBefore = "12:00" }),
		"expired":          mutate(func(s *Statement) { s.ExpiresAt = now.Add(-time.Second).Format(time.RFC3339) }),
		"not yet valid":    mutate(func(s *Statement) { s.NotBefore = now.Add(time.Hour).Format(time.RFC3339) }), // beyond the 2-min clockLeeway

		"expiry>24h cap":    mutate(func(s *Statement) { s.ExpiresAt = now.Add(25 * time.Hour).Format(time.RFC3339) }),
		"bad action kind":   mutate(func(s *Statement) { s.Action = Action{Kind: "sudo"} }),
		"tool_call no tool": mutate(func(s *Statement) { s.Action = Action{Kind: "tool_call", ArgBinding: "exact", Uses: 1} }),
		"tool_call bad binding": mutate(func(s *Statement) {
			s.Action = Action{Kind: "tool_call", Tool: "x", Op: "y", ArgBinding: "loose", Uses: 1}
		}),
		"tool_call uses!=1": mutate(func(s *Statement) {
			s.Action = Action{Kind: "tool_call", Tool: "x", Op: "y", ArgBinding: "exact", Uses: 2}
		}),
		"instruction empty text": mutate(func(s *Statement) { s.Action = Action{Kind: "instruction"} }),
		"not json":               []byte("hello"),
	}
	for name, raw := range cases {
		if _, err := ParseStatement(raw, "testinst", now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRejectDuplicateKeys(t *testing.T) {
	dup := []byte(`{"v":1,"v":1,"instance":"testinst"}`)
	if err := RejectDuplicateKeys(dup); err == nil {
		t.Fatal("duplicate top-level key accepted")
	}
	nested := []byte(`{"a":{"x":1,"x":2}}`)
	if err := RejectDuplicateKeys(nested); err == nil {
		t.Fatal("duplicate nested key accepted")
	}
	ok := []byte(`{"a":{"x":1},"b":[{"x":1},{"x":2}]}`)
	if err := RejectDuplicateKeys(ok); err != nil {
		t.Fatalf("clean doc rejected: %v", err)
	}
}

func TestValidateActionExported(t *testing.T) {
	if err := ValidateAction(Action{Kind: "instruction", Text: "hi"}); err != nil {
		t.Fatalf("valid action rejected: %v", err)
	}
	if err := ValidateAction(Action{Kind: "sudo"}); err == nil {
		t.Fatal("invalid action kind accepted")
	}
}

func TestParseEnvelope(t *testing.T) {
	now := time.Now()
	raw, _ := json.Marshal(Envelope{V: 1, Instance: "testinst", Op: "revoke",
		Params: map[string]string{"id": "abc"}, IssuedAt: now.Format(time.RFC3339)})
	if _, err := ParseEnvelope(raw, "testinst", now, 2*time.Minute); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	stale, _ := json.Marshal(Envelope{V: 1, Instance: "testinst", Op: "list",
		IssuedAt: now.Add(-10 * time.Minute).Format(time.RFC3339)})
	if _, err := ParseEnvelope(stale, "testinst", now, 2*time.Minute); err == nil {
		t.Fatal("stale envelope accepted")
	}
}
