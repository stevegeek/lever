package treebackup

// FreeBytes is the space available on the store's filesystem.
func (s *Store) FreeBytes() (uint64, error) {
	b, _, _, err := fsFree(s.dirf)
	return b, err
}

// ProbeKernel reports whether this host can take tree snapshots (on Linux:
// 5.8 or later, with openat2 allowed).
func ProbeKernel() error { return sysProbeKernel() }
