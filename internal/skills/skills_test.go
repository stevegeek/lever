package skills

import (
	"bytes"
	_ "embed"
	"regexp"
	"strings"
	"testing"
)

func TestRenderSubstitutesVersionAndFrontmatter(t *testing.T) {
	for name, fn := range map[string]func(string) []byte{"operator": func(v string) []byte { return Operator(v, false, false, false) }, "agent": func(v string) []byte { return Agent(v, false, false, false) }} {
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
	for name, fn := range map[string]func(string) []byte{"operator": func(v string) []byte { return Operator(v, false, false, false) }, "agent": func(v string) []byte { return Agent(v, false, false, false) }} {
		got := string(fn("0.2.0"))
		for _, want := range []string{"lever-capability", "_capability", "missing capability"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: content must mention %q", name, want)
			}
		}
	}
}

func TestHashStableAndDistinct(t *testing.T) {
	a1, a2 := Hash(Operator("0.2.0", false, false, false)), Hash(Operator("0.2.0", false, false, false))
	if a1 != a2 {
		t.Fatal("hash not deterministic")
	}
	if Hash(Operator("0.2.0", false, false, false)) == Hash(Operator("0.3.0", false, false, false)) {
		t.Fatal("version change must change the hash")
	}
	if len(a1) != 64 {
		t.Fatalf("want sha256 hex (64 chars), got %d", len(a1))
	}
}

func TestLeverVersion(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"rendered scaffold", string(Operator("9.9.9", false, false, false)), "9.9.9"},
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
	for name, fn := range map[string]func(string, bool, bool, bool) []byte{"operator": Operator, "agent": Agent} {
		for on, want := range map[bool]string{true: "**on**", false: "**off**"} {
			got := string(fn("1", on, false, false))
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
	for name, got := range map[string]string{
		"operator": string(Operator("1", true, false, false)), "agent": string(Agent("1", true, false, false)),
		"operator, agent messages on": string(Operator("1", true, true, false)), "agent, agent messages on": string(Agent("1", true, true, false)),
	} {
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
	for name, fn := range map[string]func(string, bool, bool, bool) []byte{"operator": Operator, "agent": Agent} {
		got := string(fn("1", true, false, false))
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
	op := string(Operator("1", true, false, false))
	for _, want := range []string{"only your own earlier\n    words", "carries no operator authority", "keeps the tier of\n  where it came from"} {
		if !strings.Contains(op, want) {
			t.Errorf("operator: missing %q", want)
		}
	}
	if ag := string(Agent("1", true, false, false)); !strings.Contains(ag, "keeps the tier\n    of where it came from") {
		t.Error("agent: forwarded text keeps no origin tier")
	}
}

//go:embed testdata/lever-agent.pre.md
var agentPre string

//go:embed testdata/lever-operator.pre.md
var operatorPre string

// Off renders byte-for-byte what lever rendered before agent messages
// existed: an instance that does not turn them on gets no skill change, so
// no agent turns not-fresh for its contacts.
func TestAgentMessagesOffRendersThePreChangeSkill(t *testing.T) {
	for _, chat := range []bool{false, true} {
		if got, want := Agent("9.9.9", chat, false, false), renderChat(agentPre, "9.9.9", chat); !bytes.Equal(got, want) {
			t.Fatalf("lever-agent off (chat=%v) differs from the pre-change render", chat)
		}
		if got, want := Operator("9.9.9", chat, false, false), renderChat(operatorPre, "9.9.9", chat); !bytes.Equal(got, want) {
			t.Fatalf("lever-operator off (chat=%v) differs from the pre-change render", chat)
		}
	}
}

func TestAgentMessagesOnTeachesAuthorizeThenSend(t *testing.T) {
	for name, pair := range map[string][2][]byte{
		"agent":    {Agent("1", true, true, false), Agent("1", true, false, false)},
		"operator": {Operator("1", true, true, false), Operator("1", true, false, false)},
	} {
		s := string(pair[0])
		for _, want := range []string{"contact_message", "contacts()", "reply_to_ref", "body_file", "command", "not-a-contact", "limit", "never secrets"} {
			if !strings.Contains(strings.ToLower(s), strings.ToLower(want)) {
				t.Errorf("%s on: missing %q", name, want)
			}
		}
		if strings.Contains(s, "lever:agent-messages") || strings.Contains(string(pair[1]), "lever:agent-messages") {
			t.Errorf("%s: a marker line survived", name)
		}
		if bytes.Equal(pair[0], pair[1]) {
			t.Errorf("%s: on and off must differ (the freshness gate keys on the hash)", name)
		}
	}
}

// operatorNoRecycle is the manager skill source with the recycle variant
// picked off, the base the other block tests compare with.
var operatorNoRecycle = pickBlocks(operatorSrc, false, recycleOn, recycleOff)

// filesBlocks cuts every files block (on and off) with its markers.
var filesBlocks = regexp.MustCompile(`(?ms)^<!-- lever:files (on|off) -->\n.*?^<!-- /lever:files (on|off) -->\n`)

// Off renders exactly what the source renders with no files blocks at all:
// an instance that does not turn files on gets no skill change.
func TestFilesOffRendersUnchanged(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, am := range []bool{false, true} {
			if got, want := Agent("9.9.9", chat, am, false), renderChat(pick(filesBlocks.ReplaceAllString(agentSrc, ""), am), "9.9.9", chat); !bytes.Equal(got, want) {
				t.Fatalf("lever-agent files off (chat=%v am=%v) changed", chat, am)
			}
			if got, want := Operator("9.9.9", chat, am, false), renderChat(pick(filesBlocks.ReplaceAllString(operatorNoRecycle, ""), am), "9.9.9", chat); !bytes.Equal(got, want) {
				t.Fatalf("lever-operator files off (chat=%v am=%v) changed", chat, am)
			}
		}
	}
}

func TestFilesOnTeachesTheExchange(t *testing.T) {
	for name, pair := range map[string][2][]byte{
		"agent":    {Agent("1", true, false, true), Agent("1", true, false, false)},
		"operator": {Operator("1", true, false, true), Operator("1", true, false, false)},
	} {
		s := string(pair[0])
		for _, want := range []string{"contact_files", "share_file", "sha256", "out_dir", "📎 uploaded", "data from that login",
			"not-a-contact", "bad-path", "symlink", "too-large", "extension", "never another login's uploads",
			"`contact` set to that login", "`hard-link`", "with no `request` mint", "the login it names as the sender is the uploader", "contact-required",
			"belong to that login's conversation only", "never use, quote, summarise or disclose another login's"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s on: missing %q", name, want)
			}
		}
		if strings.Contains(s, "lever:files") || bytes.Equal(pair[0], pair[1]) {
			t.Errorf("%s: a marker survived, or on equals off", name)
		}
	}
	if !strings.Contains(string(Operator("1", true, false, true)), "workers' .lever-files") {
		t.Error("the manager must be told the workers' exchanges are theirs")
	}
	if op := string(Operator("1", true, false, true)); strings.Contains(op, "tell the manager") || !strings.Contains(op, "tell the operator") {
		t.Error("the manager's skill must send it to the operator, not to itself")
	}
}

// The direction blocks render only when a direction is off; with both on
// the files skill is exactly what it was before the blocks existed.
func TestFilesDirectionBlocks(t *testing.T) {
	dirBlocks := regexp.MustCompile(`(?ms)^<!-- lever:(uploads|shares) off -->\n.*?^<!-- /lever:(uploads|shares) off -->\n`)
	for name, pair := range map[string][2]string{"agent": {agentSrc, "a"}, "operator": {operatorNoRecycle, "o"}} {
		render := func(f Files) string {
			if pair[1] == "a" {
				return string(AgentWith("1", true, false, f))
			}
			return string(OperatorWith("1", true, false, f, false))
		}
		want := string(renderChat(pickBlocks(pick(dirBlocks.ReplaceAllString(pair[0], ""), false), true, filesOn, filesOff), "1", true))
		if got := render(Files{On: true}); got != want {
			t.Fatalf("%s: both directions on changed the skill", name)
		}
		if render(Files{On: true}) != map[string]string{"a": string(Agent("1", true, false, true)), "o": string(Operator("1", true, false, true))}[pair[1]] {
			t.Fatalf("%s: AgentWith/OperatorWith with defaults differ from Agent/Operator", name)
		}
		up, sh := render(Files{On: true, NoUploads: true}), render(Files{On: true, NoShares: true})
		if !strings.Contains(up, "`uploads-off`") || strings.Contains(up, "`shares-off`") || !strings.Contains(sh, "`shares-off`") || strings.Contains(sh, "`uploads-off`") {
			t.Fatalf("%s: direction text", name)
		}
		if strings.Contains(up+sh, "lever:uploads") || strings.Contains(up+sh, "lever:shares") {
			t.Fatalf("%s: a marker survived", name)
		}
		if off := render(Files{NoUploads: true, NoShares: true}); strings.Contains(off, "uploads-off") || strings.Contains(off, "shares-off") {
			t.Fatalf("%s: files off renders no direction text", name)
		}
	}
}

func TestContentHashIgnoresOnlyTheStamp(t *testing.T) {
	a := []byte("---\nname: x\nlever-version: 0.32.0\n---\nBody lever-version: 9\n")
	b := []byte("---\nname: x\nlever-version: 0.33.0\n---\nBody lever-version: 9\n")
	if ContentHash(a) != ContentHash(b) {
		t.Fatal("two stamps of the same text hash apart")
	}
	if ContentHash(a) == Hash(a) {
		t.Fatal("the stamp line was not left out")
	}
	c := []byte("---\nname: x\nlever-version: 0.33.0\n---\nOther body\n")
	if ContentHash(c) == ContentHash(b) {
		t.Fatal("a body change was hidden")
	}
	noStamp := []byte("---\nname: x\n---\nBody\n")
	if ContentHash(noStamp) != Hash(noStamp) {
		t.Fatal("a file with no stamp must hash as itself")
	}
	odd := []byte("---\nname: x\nlever-version: 0.33.0 extra words\n---\nBody lever-version: 9\n")
	if ContentHash(odd) == ContentHash(b) {
		t.Fatal("a stamp line with extra text was dropped")
	}
	indented := []byte("---\nname: x\n  lever-version: 0.33.0\n---\nBody lever-version: 9\n")
	if ContentHash(indented) == ContentHash(b) {
		t.Fatal("an indented stamp line was dropped")
	}
	bodyOnly := []byte("lever-version: 1\nno frontmatter\n")
	if ContentHash(bodyOnly) != Hash(bodyOnly) {
		t.Fatal("a stamp outside the frontmatter must not be dropped")
	}
}

// The recycle text renders only for an instance with a recyclable worker; off,
// the skill is byte-for-byte the text before it (the pre-change render).
func TestOperatorRecycleBlocks(t *testing.T) {
	for _, chat := range []bool{false, true} {
		if got, want := OperatorWith("9.9.9", chat, false, Files{}, false), renderChat(operatorPre, "9.9.9", chat); !bytes.Equal(got, want) {
			t.Fatalf("recycle off (chat=%v) differs from the pre-change render", chat)
		}
	}
	off := string(OperatorWith("1", true, true, Files{On: true}, false))
	on := string(OperatorWith("1", true, true, Files{On: true}, true))
	if strings.Contains(off, "agent recycle") {
		t.Error("recycle off teaches agent recycle")
	}
	for _, want := range []string{"lever-manager agent recycle <worker>", "conversation is LOST", "workspace", "recyclable: true", "agent stop <worker>"} {
		if !strings.Contains(on, want) {
			t.Errorf("recycle on: missing %q", want)
		}
	}
	if strings.Contains(on, "NOT create or purge them") || strings.Contains(on, "lever:recycle") || strings.Contains(off, "lever:recycle") {
		t.Error("a recycle variant or marker line survived")
	}
}
