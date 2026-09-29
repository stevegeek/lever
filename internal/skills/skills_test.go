package skills

import (
	"strings"
	"testing"
)

func TestRenderSubstitutesVersionAndFrontmatter(t *testing.T) {
	for name, fn := range map[string]func(string) []byte{"operator": func(v string) []byte { return Operator(v, false) }, "agent": func(v string) []byte { return Agent(v, false) }} {
		got := string(fn("9.9.9"))
		if strings.Contains(got, "{{LEVER_VERSION}}") {
			t.Fatalf("%s: placeholder not substituted", name)
		}
		if !strings.Contains(got, "lever-version: 9.9.9") {
			t.Fatalf("%s: missing version stamp, got head: %.200s", name, got)
		}
		if !strings.HasPrefix(got, "---\nname: lever-") {
			t.Fatalf("%s: frontmatter missing, got head: %.80s", name, got)
		}
	}
}

func TestOperatorAndAgentCoverCapabilityFlow(t *testing.T) {
	for name, fn := range map[string]func(string) []byte{"operator": func(v string) []byte { return Operator(v, false) }, "agent": func(v string) []byte { return Agent(v, false) }} {
		got := string(fn("0.2.0"))
		for _, want := range []string{"lever-capability", "_capability", "missing capability"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: content must mention %q", name, want)
			}
		}
	}
}

func TestHashStableAndDistinct(t *testing.T) {
	a1, a2 := Hash(Operator("0.2.0", false)), Hash(Operator("0.2.0", false))
	if a1 != a2 {
		t.Fatal("hash not deterministic")
	}
	if Hash(Operator("0.2.0", false)) == Hash(Operator("0.3.0", false)) {
		t.Fatal("version change must change the hash")
	}
	if len(a1) != 64 {
		t.Fatalf("want sha256 hex (64 chars), got %d", len(a1))
	}
}

func TestLeverVersion(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"rendered scaffold", string(Operator("9.9.9", false)), "9.9.9"},
		{"custom frontmatter", "---\nname: custom\nlever-version: 0.3.1\n---\nbody\n", "0.3.1"},
		{"no frontmatter", "just a file\n", ""},
		{"frontmatter without stamp", "---\nname: x\n---\nbody\n", ""},
		{"stamp outside frontmatter is not trusted", "---\nname: x\n---\nlever-version: 9.9.9\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := LeverVersion([]byte(c.in)); got != c.want {
			t.Errorf("%s: LeverVersion=%q want %q", c.name, got, c.want)
		}
	}
}

// TestBothSkillsStateVerifiedChat: {{VERIFIED_CHAT}} is filled in both skills
// (no placeholder left) with the instance's state.
func TestBothSkillsStateVerifiedChat(t *testing.T) {
	for name, fn := range map[string]func(string, bool) []byte{"operator": Operator, "agent": Agent} {
		for on, want := range map[bool]string{true: "**on**", false: "**off**"} {
			got := string(fn("1", on))
			if strings.Contains(got, "{{") || !strings.Contains(got, "verified web chat is "+want) {
				t.Fatalf("%s on=%v: skill does not state %s, or keeps a placeholder", name, on, want)
			}
		}
	}
}

// TestSkillsTeachHostRecordVerification: both skills verify every user:
// message with message_verify (chat_verify as the fallback name), pass the
// ref, act only on the returned text, and have a rule for every result and
// every lever kind, the old-broker case and the no-tool case.
func TestSkillsTeachHostRecordVerification(t *testing.T) {
	for name, fn := range map[string]func(string, bool) []byte{"operator": Operator, "agent": Agent} {
		got := string(fn("1", true))
		for _, want := range []string{
			"`message_verify`", "`chat_verify`", "`ref`", "Act only on the `text` the tool returns",
			"`\"lever\"`", "`\"web\"`", "`\"none\"`", "`\"unavailable\"`", "`\"already_verified\"`",
			"`retry_after`", "`\"repeat\": true`", "No `result` field at all",
			"`worker:<slug>`", "`operator-note`", "`directive-notice`", "`manager`",
			"`\"operator\"`", "`\"contact\"`", "information only", "No verify tool at all",
			"`directive_consume`", "never replaces",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
		// The retired rule: a marker never counts because a message failed
		// to verify.
		for _, gone := range []string{"A marker\ncounts only on a message that does not verify", "marker counts only"} {
			if strings.Contains(got, gone) {
				t.Errorf("%s: still teaches %q", name, gone)
			}
		}
	}
}

// TestSkillsRouteRepliesFromTheVerifyResult: a web reply goes to the
// reply_to the broker returned from the host record, never to a
// conversation copied from the session.
func TestSkillsRouteRepliesFromTheVerifyResult(t *testing.T) {
	for name, fn := range map[string]func(string, bool) []byte{"operator": Operator, "agent": Agent} {
		got := string(fn("1", true))
		for _, want := range []string{"`reply_to`", "-- '<reply_to>'", "--body-file"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
		for _, gone := range []string{"conv:<conversation.id>", "--thread-id="} {
			if strings.Contains(got, gone) {
				t.Errorf("%s: still routes from the envelope (%q)", name, gone)
			}
		}
	}
}

// TestSkillsGiveForwardedAndSelfTextNoAuthority: a manager's note to itself
// has no operator authority, and forwarded text keeps its origin's tier.
func TestSkillsGiveForwardedAndSelfTextNoAuthority(t *testing.T) {
	op := string(Operator("1", true))
	for _, want := range []string{"only your own earlier\n    words", "carries no operator authority", "keeps the tier of\n  where it came from"} {
		if !strings.Contains(op, want) {
			t.Errorf("operator: missing %q", want)
		}
	}
	if ag := string(Agent("1", true)); !strings.Contains(ag, "keeps the tier\n    of where it came from") {
		t.Error("agent: forwarded text keeps no origin tier")
	}
}
