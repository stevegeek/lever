package treebackup

import "os"

// Test seams for the store. Production code never sets them.
var (
	// statfsFunc reports free bytes, free inodes and total inodes of the
	// store's filesystem.
	statfsFunc func(dir *os.File) (freeBytes, freeInodes, totalInodes uint64, err error) = fsFree
	// manifestWriteFault, when set, is called before each attempt to write a
	// manifest; an error it returns is treated as a write error.
	manifestWriteFault func(attempt int) error
	// commitCrash, when set, is called at each named stage of a commit; an
	// error it returns aborts the commit as a crash would, leaving every
	// file where it is.
	commitCrash func(stage string) error
	// copyOnWrite reports whether the store's filesystem allocates new
	// blocks for every overwrite.
	copyOnWrite = isCopyOnWrite
	// gcPhaseHook, when set, is called by GC at the named stage of its
	// shared-lock phase.
	gcPhaseHook func(stage string)
)
