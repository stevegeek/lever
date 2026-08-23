package scionbin

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	emX8664   = 62  // elf.EM_X86_64
	emAArch64 = 183 // elf.EM_AARCH64
	etExec    = 2
	etDyn     = 3
)

// writeELF64 writes a minimal but structurally valid 64-bit little-endian ELF
// header for the given machine and object type. Program and section header
// counts are zero, so debug/elf parses the header and stops — enough to
// exercise the arch check without carrying a real 158MB binary as test data.
func writeELF64(t *testing.T, dir string, machine uint16, etype uint16) string {
	t.Helper()
	h := make([]byte, 64)
	copy(h, []byte{0x7f, 'E', 'L', 'F'})
	h[4] = 2 // EI_CLASS: 64-bit
	h[5] = 1 // EI_DATA: little-endian
	h[6] = 1 // EI_VERSION
	binary.LittleEndian.PutUint16(h[16:], etype)
	binary.LittleEndian.PutUint16(h[18:], machine)
	binary.LittleEndian.PutUint32(h[20:], 1)  // e_version
	binary.LittleEndian.PutUint16(h[52:], 64) // e_ehsize
	path := filepath.Join(dir, "scion")
	if err := os.WriteFile(path, h, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyELFArchAcceptsMatchingArch(t *testing.T) {
	for _, tc := range []struct {
		goarch  string
		machine uint16
	}{
		{"amd64", emX8664},
		{"arm64", emAArch64},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			p := writeELF64(t, t.TempDir(), tc.machine, etExec)
			if err := VerifyELFArch(p, tc.goarch); err != nil {
				t.Fatalf("VerifyELFArch: %v", err)
			}
		})
	}
}

func TestVerifyELFArchAcceptsPIE(t *testing.T) {
	// A Go PIE build is ET_DYN, not ET_EXEC. Rejecting it would be wrong.
	p := writeELF64(t, t.TempDir(), emAArch64, etDyn)
	if err := VerifyELFArch(p, "arm64"); err != nil {
		t.Fatalf("a PIE build must be accepted: %v", err)
	}
}

func TestVerifyELFArchRejectsMismatchNamingBothArches(t *testing.T) {
	// The failure this exists to catch: a workstation-built binary for the wrong
	// arch. The message must say which is which, or the operator cannot tell
	// whether to rebuild the binary or fix the config.
	p := writeELF64(t, t.TempDir(), emAArch64, etExec)
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
	p := writeELF64(t, t.TempDir(), emAArch64, etExec)
	if err := VerifyELFArch(p, "riscv64"); err == nil {
		t.Error("an arch lever cannot map must be rejected, not silently accepted")
	}
}
