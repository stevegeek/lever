package layout

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseSettingsEmptyIsAnEmptyMapping(t *testing.T) {
	for _, in := range []string{"", "  \n\t"} {
		doc, err := ParseSettings([]byte(in))
		if err != nil {
			t.Fatalf("ParseSettings(%q): %v", in, err)
		}
		root := DocumentRoot(doc)
		if root == nil || root.Kind != yaml.MappingNode || len(root.Content) != 0 {
			t.Fatalf("ParseSettings(%q) root = %+v, want an empty mapping", in, root)
		}
	}
}

func TestDocumentRootRefusesNonMappings(t *testing.T) {
	for _, in := range []string{"- a\n- b\n", "just a scalar\n"} {
		doc, err := ParseSettings([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if DocumentRoot(doc) != nil {
			t.Fatalf("DocumentRoot(%q) should be nil", in)
		}
	}
}

func TestParseSettingsReportsBadYAML(t *testing.T) {
	if _, err := ParseSettings([]byte("server: [\n")); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestMapHelpersKeepOrder(t *testing.T) {
	doc, err := ParseSettings([]byte("a: 1\n# keep me\nb: 2\nc: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	root := DocumentRoot(doc)
	if got := MapGet(root, "b"); got == nil || got.Value != "2" {
		t.Fatalf("MapGet(b) = %+v", got)
	}
	if MapGet(root, "zzz") != nil {
		t.Fatal("MapGet of an absent key must be nil")
	}
	MapSet(root, "b", StringNode("two"))
	MapSet(root, "d", BoolNode(true))
	MapDelete(root, "a")
	MapDelete(root, "absent")
	out, err := EncodeSettings(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := "# keep me\nb: two\nc: 3\nd: true\n"
	if string(out) != want {
		t.Fatalf("encoded:\n%s\nwant:\n%s", out, want)
	}
}

func TestEncodeSettingsUsesTwoSpaceIndent(t *testing.T) {
	doc, err := ParseSettings([]byte("server:\n    auth:\n        display_name: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeSettings(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "server:\n  auth:\n    display_name: x\n" {
		t.Fatalf("encoded:\n%s", out)
	}
}

func TestOIDCLoginNodeKeys(t *testing.T) {
	n, err := OIDCLogin{Enabled: true, DisplayName: "Lever", IssuerURL: "http://127.0.0.1:8446", ClientID: "lever"}.Node()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		keys = append(keys, n.Content[i].Value)
	}
	if got := strings.Join(keys, ","); got != "enabled,display_name,issuer_url,client_id" {
		t.Fatalf("keys = %s", got)
	}
	same, err := SameYAML(n, n)
	if err != nil || !same {
		t.Fatalf("SameYAML(n, n) = %v, %v", same, err)
	}
	other, _ := OIDCLogin{Enabled: true}.Node()
	if same, _ := SameYAML(n, other); same {
		t.Fatal("different blocks must not compare equal")
	}
}

func TestPathsHangOffDir(t *testing.T) {
	for _, p := range []string{SettingsRel, ServerYAMLRel, ProjectConfigsRel, TemplatesRel, DevTokenRel} {
		if !strings.HasPrefix(p, Dir+"/") {
			t.Fatalf("%q is not under %s", p, Dir)
		}
	}
	if strings.HasPrefix(WebAssetsSentinel, "/") {
		t.Fatal("WebAssetsSentinel must be relative to the dist root")
	}
}
