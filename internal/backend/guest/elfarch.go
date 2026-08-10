package guest

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
)

// elfMachineForGOARCH maps the Go arch names Guest.GOARCH returns onto the ELF
// machine type a linux binary for that arch must declare.
var elfMachineForGOARCH = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

// verifyELFArch reports whether path is a linux executable for wantGOARCH.
//
// This is the point of accepting a prebuilt binary: it was built somewhere
// else, so an architecture mix-up is the realistic failure. Caught here it is a
// config error naming both arches, raised before anything is written into the
// guest. Uncaught it surfaces inside the guest at manager start as "exec format
// error", which points at nothing.
func verifyELFArch(path, wantGOARCH string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("scion binary %q: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("scion binary %q is not a regular file", path)
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("scion binary %q is not an ELF executable (a linux build is required): %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// A Go PIE build is ET_DYN; an ordinary build is ET_EXEC. This rejects an
	// object file or a core dump. It does NOT separate an executable from a
	// shared library — both are ET_DYN, and only a PT_INTERP segment really
	// tells them apart — so a .so still reaches the guest and fails there.
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("scion binary %q is an ELF %v, not an executable", path, f.Type)
	}
	// The machine alone does not pin the ABI: a 32-bit or big-endian header can
	// still declare EM_X86_64/EM_AARCH64, and either would fail in the guest
	// with the exec-format error this check exists to prevent. Real toolchains
	// do not emit these, which is exactly why they would be baffling.
	if f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("scion binary %q is %v, but the guest needs a 64-bit binary", path, f.Class)
	}
	if f.ByteOrder != binary.LittleEndian {
		return fmt.Errorf("scion binary %q is big-endian, but the guest is little-endian", path)
	}
	want, ok := elfMachineForGOARCH[wantGOARCH]
	if !ok {
		return fmt.Errorf("unsupported guest architecture %q", wantGOARCH)
	}
	if f.Machine != want {
		return fmt.Errorf("scion binary %q is %s, but the guest is %s",
			path, goarchForELFMachine(f.Machine), wantGOARCH)
	}
	return nil
}

// goarchForELFMachine renders an ELF machine as a Go arch name where one is
// known, so a mismatch message reads in the same vocabulary as the config.
func goarchForELFMachine(m elf.Machine) string {
	for goarch, machine := range elfMachineForGOARCH {
		if machine == m {
			return goarch
		}
	}
	return m.String()
}
