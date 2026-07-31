package skills

import (
	"strings"
	"testing"
)

func TestRenderSubstitutesVersionAndFrontmatter(t *testing.T) {
	for name, fn := range map[string]func(string) []byte{"operator": Operator, "agent": Agent} {
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
	for name, fn := range map[string]func(string) []byte{"operator": Operator, "agent": Agent} {
		got := string(fn("0.2.0"))
		for _, want := range []string{"lever-capability", "_capability", "missing capability"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: content must mention %q", name, want)
			}
		}
	}
}

func TestHashStableAndDistinct(t *testing.T) {
	a1, a2 := Hash(Operator("0.2.0")), Hash(Operator("0.2.0"))
	if a1 != a2 {
		t.Fatal("hash not deterministic")
	}
	if Hash(Operator("0.2.0")) == Hash(Operator("0.3.0")) {
		t.Fatal("version change must change the hash")
	}
	if len(a1) != 64 {
		t.Fatalf("want sha256 hex (64 chars), got %d", len(a1))
	}
}

func TestLeverVersion(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"rendered scaffold", string(Operator("9.9.9")), "9.9.9"},
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
