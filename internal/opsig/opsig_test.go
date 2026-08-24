package opsig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	sig, err := Sign(context.Background(), priv, NamespaceDirective, msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !bytes.Contains(sig, []byte("BEGIN SSH SIGNATURE")) {
		t.Fatalf("not an armored signature: %.60s", sig)
	}
	v := Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(context.Background(), NamespaceDirective, msg, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyRejectsTamperAndWrongNamespaceAndPrincipal(t *testing.T) {
	priv, as := genKey(t, t.TempDir())
	msg, _ := json.Marshal(validStatement(time.Now()))
	sig, err := Sign(context.Background(), priv, NamespaceDirective, msg)
	if err != nil {
		t.Fatal(err)
	}
	v := Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(context.Background(), NamespaceDirective, append(msg, ' '), sig); err == nil {
		t.Fatal("tampered message verified")
	}
	if err := v.Verify(context.Background(), NamespaceAdmin, msg, sig); err == nil {
		t.Fatal("wrong namespace verified")
	}
	if err := (Verifier{AllowedSigners: as, Principal: "other@x"}).Verify(context.Background(), NamespaceDirective, msg, sig); err == nil {
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

// TestValidateActionApproval pins the "approval" kind: a well-formed approval
// action (tool+op, exact binding, uses==1, no free text) is accepted, and the
// same tool-shape rejections apply as for tool_call. No other test exercises
// kind "approval" — it guards the validateAction "approval" case-label and,
// once the literal becomes a constant, that KindApproval keeps the wire value.
func TestValidateActionApproval(t *testing.T) {
	now := time.Now()
	base := validStatement(now)
	withAction := func(a Action) []byte {
		s := base
		s.Action = a
		b, _ := json.Marshal(s)
		return b
	}

	good := withAction(Action{Kind: "approval", Tool: "db", Op: "read", ArgBinding: "exact", Uses: 1})
	st, err := ParseStatement(good, "testinst", now)
	if err != nil {
		t.Fatalf("valid approval rejected: %v", err)
	}
	if st.Action.Kind != "approval" {
		t.Fatalf("parsed kind = %q, want approval", st.Action.Kind)
	}

	rejects := map[string]Action{
		"approval no tool":     {Kind: "approval", Op: "read", ArgBinding: "exact", Uses: 1},
		"approval no op":       {Kind: "approval", Tool: "db", ArgBinding: "exact", Uses: 1},
		"approval bad binding": {Kind: "approval", Tool: "db", Op: "read", ArgBinding: "loose", Uses: 1},
		"approval uses!=1":     {Kind: "approval", Tool: "db", Op: "read", ArgBinding: "exact", Uses: 2},
		"approval has text":    {Kind: "approval", Tool: "db", Op: "read", ArgBinding: "exact", Uses: 1, Text: "x"},
	}
	for name, a := range rejects {
		if _, err := ParseStatement(withAction(a), "testinst", now); err == nil {
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

// TestRejectDuplicateKeysDepthCapped feeds pathologically deep (but small,
// well under maxStatementBytes) nesting to prove walkDupes fails closed on
// an explicit depth bound rather than relying solely on the 64KiB size cap.
func TestRejectDuplicateKeysDepthCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteByte('[')
	}
	for i := 0; i < 5000; i++ {
		b.WriteByte(']')
	}
	if err := RejectDuplicateKeys([]byte(b.String())); err == nil {
		t.Fatal("deeply nested JSON accepted without a depth bound")
	}
}

// The next four tests exercise the size cap and duplicate-key rejection
// *through* the parsers (ParseStatement/ParseEnvelope), not via a direct
// RejectDuplicateKeys call. Each input is otherwise valid — it would decode
// and pass field/temporal validation — so ONLY the strict-decode prologue
// step under test can reject it. This pins that the C6 decodeStrict extraction
// keeps both the size cap and the dup-key walk on both parse paths; dropping
// either step would let these inputs through.

func TestParseStatementRejectsOversized(t *testing.T) {
	now := time.Now()
	s := validStatement(now)
	// tool_call carries an arbitrary-size Args (json.RawMessage) — the only
	// field that can make an otherwise-valid statement exceed the cap.
	big := strings.Repeat("a", maxStatementBytes+1)
	s.Action = Action{Kind: "tool_call", Tool: "x", Op: "y", ArgBinding: "exact", Uses: 1,
		Args: json.RawMessage(`"` + big + `"`)}
	raw, _ := json.Marshal(s)
	if len(raw) <= maxStatementBytes {
		t.Fatalf("input not oversized: %d bytes", len(raw))
	}
	if _, err := ParseStatement(raw, "testinst", now); err == nil {
		t.Fatal("oversized statement accepted")
	}
}

func TestParseStatementRejectsDuplicateKey(t *testing.T) {
	now := time.Now()
	raw, _ := json.Marshal(validStatement(now)) // begins {"v":1,...
	// Inject a duplicate top-level "v" key. encoding/json keeps last-wins, so
	// without RejectDuplicateKeys this decodes to the identical valid statement.
	dup := append([]byte(`{"v":1,`), raw[1:]...)
	if _, err := ParseStatement(dup, "testinst", now); err == nil {
		t.Fatal("duplicate-key statement accepted")
	}
}

func TestParseEnvelopeRejectsOversized(t *testing.T) {
	now := time.Now()
	big := strings.Repeat("a", maxStatementBytes+1)
	raw, _ := json.Marshal(Envelope{V: 1, Instance: "testinst", Op: "revoke",
		Params: map[string]string{"id": big}, IssuedAt: now.Format(time.RFC3339)})
	if len(raw) <= maxStatementBytes {
		t.Fatalf("input not oversized: %d bytes", len(raw))
	}
	if _, err := ParseEnvelope(raw, "testinst", now, 2*time.Minute); err == nil {
		t.Fatal("oversized envelope accepted")
	}
}

func TestParseEnvelopeRejectsDuplicateKey(t *testing.T) {
	now := time.Now()
	raw, _ := json.Marshal(Envelope{V: 1, Instance: "testinst", Op: "revoke",
		IssuedAt: now.Format(time.RFC3339)}) // begins {"v":1,...
	dup := append([]byte(`{"v":1,`), raw[1:]...)
	if _, err := ParseEnvelope(dup, "testinst", now, 2*time.Minute); err == nil {
		t.Fatal("duplicate-key envelope accepted")
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

// TestParseEnvelopeNamesTheFailingField: like ParseStatement, each shape
// rejection says which field failed.
func TestParseEnvelopeNamesTheFailingField(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		env  Envelope
		want string
	}{
		{"version", Envelope{V: 2, Instance: "testinst", Op: "list", IssuedAt: now.Format(time.RFC3339)}, "version 2"},
		{"instance", Envelope{V: 1, Instance: "other", Op: "list", IssuedAt: now.Format(time.RFC3339)}, "instance mismatch"},
		{"op", Envelope{V: 1, Instance: "testinst", IssuedAt: now.Format(time.RFC3339)}, "op"},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(c.env)
		_, err := ParseEnvelope(raw, "testinst", now, 2*time.Minute)
		if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrInvalid mentioning %q", c.name, err, c.want)
		}
	}
}

// TestSignVerifyContextCancelled: a cancelled context stops the ssh-keygen
// subprocess instead of letting it run unbounded on the request path.
func TestSignVerifyContextCancelled(t *testing.T) {
	priv, as := genKey(t, t.TempDir())
	msg, _ := json.Marshal(validStatement(time.Now()))
	sig, err := Sign(context.Background(), priv, NamespaceDirective, msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Sign(ctx, priv, NamespaceDirective, msg); err == nil {
		t.Fatal("Sign with a cancelled ctx must fail")
	}
	v := Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(context.Background(), NamespaceDirective, msg, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := v.Verify(ctx, NamespaceDirective, msg, sig); err == nil {
		t.Fatal("Verify with a cancelled ctx must fail")
	}
}
