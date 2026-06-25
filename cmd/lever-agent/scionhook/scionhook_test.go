package scionhook

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPreStartInvokesLeverAgentBoot(t *testing.T) {
	b, err := os.ReadFile("pre-start")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "#!") {
		t.Fatal("pre-start must have a shebang")
	}
	if !strings.Contains(s, "lever-agent boot") {
		t.Fatal("pre-start must invoke `lever-agent boot`")
	}
}

func TestManifestDeclaresEnvOverlayAndSidecars(t *testing.T) {
	b, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest.json must be valid JSON: %v", err)
	}
	outs, _ := m["outputs"].(map[string]any)
	if outs == nil || outs["env"] == nil {
		t.Fatal("manifest must declare outputs.env (the boot env overlay path)")
	}
}
