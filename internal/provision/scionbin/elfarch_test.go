package scionbin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend/backendtest"
)

func TestVerifyELFArchAcceptsMatchingArch(t *testing.T) {
	for _, tc := range []struct {
		goarch  string
		machine uint16
	}{
		{"amd64", backendtest.EMX8664},
		{"arm64", backendtest.EMAArch64},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			p := backendtest.WriteELF64(t, t.TempDir(), tc.machine, backendtest.ETExec)
			if err := VerifyELFArch(p, tc.goarch); err != nil {
				t.Fatalf("VerifyELFArch: %v", err)
			}
		})
	}
}

func TestVerifyELFArchAcceptsPIE(t *testing.T) {
	// A Go PIE build is ET_DYN, not ET_EXEC. Rejecting it would be wrong.
	p := backendtest.WriteELF64(t, t.TempDir(), backendtest.EMAArch64, backendtest.ETDyn)
	if err := VerifyELFArch(p, "arm64"); err != nil {
		t.Fatalf("a PIE build must be accepted: %v", err)
	}
}

func TestVerifyELFArchRejectsMismatchNamingBothArches(t *testing.T) {
	// The failure this exists to catch: a workstation-built binary for the wrong
	// arch. The message must say which is which, or the operator cannot tell
	// whether to rebuild the binary or fix the config.
	p := backendtest.WriteELF64(t, t.TempDir(), backendtest.EMAArch64, backendtest.ETExec)
	err := VerifyELFArch(p, "amd64")
	if err == nil {
		t.Fatal("expected an arch mismatch error")
	}
	for _, want := range []string{"arm64", "amd64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %q", err, want)
		}
	}
}

func TestVerifyELFArchRejectsNonELF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "scion")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := VerifyELFArch(p, "arm64")
	if err == nil || !strings.Contains(err.Error(), "ELF") {
		t.Fatalf("a non-ELF file must be rejected as such, got %v", err)
	}
}

func TestVerifyELFArchRejectsMissingAndNonRegular(t *testing.T) {
	dir := t.TempDir()
	if err := VerifyELFArch(filepath.Join(dir, "absent"), "arm64"); err == nil {
		t.Error("a missing file must be rejected")
	}
	if err := VerifyELFArch(dir, "arm64"); err == nil {
		t.Error("a directory must be rejected")
	}
}

func TestVerifyELFArchRejectsUnknownGuestArch(t *testing.T) {
	p := backendtest.WriteELF64(t, t.TempDir(), backendtest.EMAArch64, backendtest.ETExec)
	if err := VerifyELFArch(p, "riscv64"); err == nil {
		t.Error("an arch lever cannot map must be rejected, not silently accepted")
	}
}
