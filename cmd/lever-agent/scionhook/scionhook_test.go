package scionhook

import (
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
