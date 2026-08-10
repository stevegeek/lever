package guest

import (
	"debug/elf"
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

	// A Go PIE build is ET_DYN; an ordinary build is ET_EXEC. Anything else (an
	// object file, a core dump) is not something the guest can run.
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("scion binary %q is an ELF %v, not an executable", path, f.Type)
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
