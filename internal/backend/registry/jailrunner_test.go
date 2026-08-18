package registry

import (
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/exec"
)

func TestJailRunnerKnownAndUnknown(t *testing.T) {
	if jr, err := JailRunner("orbstack", exec.RealRunner{}, "lever-x", "u", "501"); err != nil || jr == nil {
		t.Fatalf("JailRunner(orbstack) = %v, %v", jr, err)
	}
	if jr, err := JailRunner("", exec.RealRunner{}, "lever-x", "u", "501"); err != nil || jr == nil {
		t.Fatalf("JailRunner(\"\") should use the default backend, got %v, %v", jr, err)
	}
	if _, err := JailRunner("nope", exec.RealRunner{}, "m", "u", "1"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("JailRunner(nope) err = %v, want unknown-backend error", err)
	}
}

func TestJailRunnerCoversAllCandidates(t *testing.T) {
	for _, c := range backend.Candidates {
		if _, err := JailRunner(c.Name, exec.RealRunner{}, "m", "u", "1"); err != nil {
			t.Errorf("JailRunner(%q): %v — every candidate must have a transport", c.Name, err)
		}
	}
}

func TestJailArgvKnownAndUnknown(t *testing.T) {
	got, err := JailArgv("orbstack", "lever-x", "stephen")
	if err != nil {
		t.Fatalf("JailArgv(orbstack): %v", err)
	}
	if !slices.Equal(got, []string{"orb", "-m", "lever-x", "-u", "stephen"}) {
		t.Fatalf("JailArgv(orbstack) = %v", got)
	}
	if got, err := JailArgv("lima", "lever-x", "ignored"); err != nil ||
		!slices.Equal(got, []string{"limactl", "shell", "lever-x"}) {
		t.Fatalf("JailArgv(lima) = %v, %v", got, err)
	}
	if got, err := JailArgv("", "lever-x", "stephen"); err != nil || len(got) == 0 {
		t.Fatalf("JailArgv(\"\") should use the default backend, got %v, %v", got, err)
	}
	if _, err := JailArgv("nope", "m", "u"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("JailArgv(nope) err = %v, want unknown-backend error", err)
	}
}

func TestJailArgvCoversAllCandidates(t *testing.T) {
	for _, c := range backend.Candidates {
		argv, err := JailArgv(c.Name, "m", "u")
		if err != nil || len(argv) == 0 {
			t.Errorf("JailArgv(%q) = %v, %v — every candidate must have a transport prefix", c.Name, argv, err)
		}
	}
}
