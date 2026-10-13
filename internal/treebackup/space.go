package treebackup

// FreeBytes is the space available on the store's filesystem.
func (s *Store) FreeBytes() (uint64, error) {
	b, _, _, err := fsFree(s.dirf)
	return b, err
}

// ReserveBytes is the space held back for the next manifest: 0 when there
// is no reserve.
func (s *Store) ReserveBytes() int64 {
	fi, err := lstatRegular(s.root, reserveName)
	if err != nil {
		return 0
	}
	return min(fi.Size(), allocated(fi))
}

// ProbeKernel reports whether this host can take tree snapshots (on Linux:
// 5.8 or later, with openat2 allowed).
func ProbeKernel() error { return sysProbeKernel() }
