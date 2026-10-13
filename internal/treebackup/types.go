// Package treebackup keeps host-side snapshots of an instance tree: a
// content-addressed blob store plus one JSON-lines manifest per snapshot,
// with diff, log, restore and garbage collection over them.
//
// Everything read from the tree is agent-written and hostile, including its
// timing: the walker works only through directory fds, never follows a link,
// never opens anything but a regular file, and records what it could not
// capture instead of failing. Readers validate a manifest fully before they
// act on it.
package treebackup

import (
	"bytes"
	"errors"
	"io"
	"time"
)

// EntryType is the file type of a manifest entry.
type EntryType string

const (
	TypeDir     EntryType = "dir"
	TypeFile    EntryType = "file"
	TypeLink    EntryType = "link"
	TypeFIFO    EntryType = "fifo"
	TypeSocket  EntryType = "socket"
	TypeChar    EntryType = "char"
	TypeBlock   EntryType = "block"
	TypeUnknown EntryType = "unknown"
)

// AllTypes is the closed set of entry types, in the order counts are written.
var AllTypes = []EntryType{TypeDir, TypeFile, TypeLink, TypeFIFO, TypeSocket, TypeChar, TypeBlock, TypeUnknown}

// State says what is unusual about an entry. The zero value means the entry
// was captured normally.
type State string

const (
	StateOK              State = ""
	StateUnreadable      State = "unreadable"
	StateChanged         State = "changed"
	StateUnstable        State = "unstable"
	StateListingUnstable State = "listing-unstable"
	StateTooBig          State = "too-big"
	StateSkipped         State = "skipped"
	StateDataless        State = "dataless"
	StateMount           State = "mount"
	StateExcluded        State = "excluded"
	StateSecret          State = "secret"
	StateTruncated       State = "truncated"
)

// AllStates is the closed set of non-empty states, in the order counts are
// written.
var AllStates = []State{StateUnreadable, StateChanged, StateUnstable, StateListingUnstable, StateTooBig,
	StateSkipped, StateDataless, StateMount, StateExcluded, StateSecret, StateTruncated}

// Reason qualifies StateSkipped and StateTruncated, and only those.
type Reason string

const (
	ReasonRead        Reason = "read"
	ReasonTime        Reason = "time"
	ReasonDisk        Reason = "disk"
	ReasonInterrupted Reason = "interrupted"
	ReasonEntries     Reason = "entries"
	ReasonManifest    Reason = "manifest"
	ReasonDirEntries  Reason = "dir-entries"
	ReasonDepth       Reason = "depth"
	ReasonPath        Reason = "path"
)

// SkippedReasons and TruncatedReasons are the reasons each state allows.
var (
	SkippedReasons   = []Reason{ReasonRead, ReasonTime, ReasonDisk, ReasonInterrupted}
	TruncatedReasons = []Reason{ReasonEntries, ReasonManifest, ReasonDirEntries, ReasonDepth, ReasonPath, ReasonTime, ReasonInterrupted}
)

// ListingUnreadable is the only value of Entry.Listing and RootInfo.Listing:
// a directory that could be searched but not listed.
const ListingUnreadable = "unreadable"

// Trigger and quiescence values of the header.
const (
	TriggerStop    = "stop"
	TriggerDestroy = "destroy"
	TriggerKeepVM  = "keep-vm"
	TriggerManual  = "manual"

	QuiescentVMOff     = "vm-off"
	QuiescentSuspended = "suspended"
	QuiescentNone      = "none"
)

// Entry is one manifest line. Path and Target are raw bytes: names in the
// tree need not be UTF-8, and a Go string through encoding/json would
// replace invalid bytes with U+FFFD and merge distinct names.
type Entry struct {
	// Path is relative to the tree, '/'-separated, with no empty, "." or ".."
	// component.
	Path []byte
	Type EntryType
	// HasMeta reports whether Mode..Nlink were read. They are all absent
	// when the stat itself failed.
	HasMeta bool
	Mode    uint32 // the 12 permission bits, setuid, setgid and sticky included
	Size    int64  // apparent size; for a captured file, the bytes stored
	Mtime   int64  // ns since the epoch
	Ctime   int64  // ns since the epoch
	Dev     uint64
	Ino     uint64
	Nlink   uint64
	// SHA256 is the lowercase hex digest of the content, for files whose
	// content was captured (or only hashed, for StateSecret). Verified is
	// the unix time it was last read from the tree, and is set exactly when
	// SHA256 is.
	SHA256   string
	Verified int64
	// Target is a link's target bytes; HasTarget is false for a link whose
	// target could not be read.
	Target    []byte
	HasTarget bool
	State     State
	Reason    Reason
	Listing   string // "" or ListingUnreadable, directories only
}

// RootInfo describes the tree root itself, which has no entry line.
// State is "", StateUnreadable (it could not be opened: nothing else was
// recorded), StateTruncated (with Reason) or StateListingUnstable.
type RootInfo struct {
	HasMeta bool
	Mode    uint32
	Mtime   int64
	State   State
	Reason  Reason
	Listing string
}

// Limits are the budgets a snapshot ran under. They are written into the
// header, and the validator checks the entries against them.
type Limits struct {
	File       int64         // bytes of one file; larger files are too-big
	Read       int64         // bytes read from files, retries included
	Entries    int64         // names listed in phase B
	Manifest   int64         // JSON-encoded bytes of the entry lines
	DirEntries int64         // names read from one directory
	Depth      int64         // directory depth
	Path       int64         // bytes of one relative path
	Time       time.Duration // walk and content passes; 0 is unlimited
}

// Hard caps: the config may not exceed them, and the validator never
// accepts more, whatever a header claims.
const (
	MaxPathCap      = 4096
	MaxComponentCap = 1024
	MaxTargetCap    = 4096
	MaxManifestCap  = 4 << 30
	MaxEntriesCap   = 10_000_000
	MaxLineBytes    = 128 << 10
	// ManifestSlack is what a manifest may hold beyond Limits.Manifest: the
	// header and the trailer. Every entry line is charged to the budget.
	ManifestSlack = 64 << 20
)

// DefaultLimits are the budgets when the config sets none.
func DefaultLimits() Limits {
	return Limits{
		File:       256 << 20,
		Read:       8 << 30,
		Entries:    1_000_000,
		Manifest:   512 << 20,
		DirEntries: 100_000,
		Depth:      128,
		Path:       4096,
		Time:       5 * time.Minute,
	}
}

// Partial groups the entries in one state and reason that clear Complete.
// At is the first such path in manifest order; HasAt is false for the root.
type Partial struct {
	State  State
	Reason Reason
	At     []byte
	HasAt  bool
	Count  int64
}

// Flood is one of the directories that listed the most names in phase B.
type Flood struct {
	At    []byte
	HasAt bool // false for the root
	Count int64
}

// Counts are the header's tallies; the validator recomputes them.
type Counts struct {
	ByType        map[EntryType]int64
	ByState       map[State]int64
	UnreadableNew int64
	Badname       int64
}

// ByteCounts: Total is the sum of captured file sizes, Read the bytes read
// from the tree (retries included), New the bytes of blobs this snapshot
// added.
type ByteCounts struct {
	Total int64
	Read  int64
	New   int64
}

type BlobCounts struct {
	New      int64
	Repaired int64
}

// Header is the first manifest line.
type Header struct {
	Version      int
	ID           string
	Seq          uint64
	Created      time.Time
	Finished     time.Time
	Trigger      string
	Quiescent    string
	Pinned       bool
	LeverVersion string
	Tree         []byte
	TreeDev      uint64
	TreeIno      uint64
	Root         RootInfo
	CacheBase    string // "" is null
	KnownBase    string // "" is null
	Limits       Limits
	Complete     bool
	Partial      []Partial
	Flood        []Flood
	Counts       Counts
	Bytes        ByteCounts
	Blobs        BlobCounts
}

// ManifestVersion is the only Header.Version this code reads or writes.
const ManifestVersion = 1

// EntryReader streams a manifest's entries in order. Next returns io.EOF
// only after the trailer and the header counts have been verified; any
// other error means the manifest is invalid and nothing read from it may be
// trusted. Callers that must not act on an invalid manifest read it through
// once (a validating pass) before they act.
type EntryReader interface {
	Header() *Header
	Next() (*Entry, error)
	Close() error
}

// OpenManifest opens a fresh reader over the same manifest, so a caller can
// make a validating pass and then an acting pass.
type OpenManifest func() (EntryReader, error)

// BlobSource opens a stored blob by its sha256 hex digest. The caller still
// verifies the hash of what it reads.
type BlobSource interface {
	OpenBlob(sum string) (io.ReadCloser, int64, error)
}

// BlobSink is where the walker puts content.
type BlobSink interface {
	// NewBlob starts a blob. The writer hashes what it is given.
	NewBlob() (BlobWriter, error)
	// HasBlob reports whether a blob with that digest and size is stored.
	HasBlob(sum string, size int64) bool
	// CheckSpace returns ErrDiskLow when free space or inodes on the store's
	// filesystem are below the configured floor.
	CheckSpace() error
}

// BlobWriter receives one blob's bytes.
type BlobWriter interface {
	io.Writer
	// Commit stores the blob under the digest of the bytes written.
	Commit() (BlobResult, error)
	Abort()
}

// BlobResult is what a committed blob did to the store. New is false for a
// dedup hit; Repaired reports that an existing blob of that name was
// replaced because it was damaged.
type BlobResult struct {
	Sum      string
	Size     int64
	New      bool
	Repaired bool
}

var (
	// ErrDiskLow means the store's filesystem is below min_free.
	ErrDiskLow = errors.New("tree backups: free space below min_free")
	// ErrUnsafe means the store or its placement is not private to the host
	// user, or it is inside the tree.
	ErrUnsafe = errors.New("tree backups: the store is not safe to use")
	// ErrBusy means a lock was not obtained in time.
	ErrBusy = errors.New("tree backups: the store is busy")
	// ErrInvalid wraps every manifest validation failure.
	ErrInvalid = errors.New("invalid tree backup manifest")
)

// ComparePaths orders paths component by component, each by raw bytes, so
// a directory sorts directly before its contents: "a", "a/b", "a.b". This
// is the walker's pre-order and the manifest order.
func ComparePaths(a, b []byte) int {
	for {
		ia := bytes.IndexByte(a, '/')
		ib := bytes.IndexByte(b, '/')
		ca, cb := a, b
		if ia >= 0 {
			ca = a[:ia]
		}
		if ib >= 0 {
			cb = b[:ib]
		}
		if c := bytes.Compare(ca, cb); c != 0 {
			return c
		}
		switch {
		case ia < 0 && ib < 0:
			return 0
		case ia < 0:
			return -1
		case ib < 0:
			return 1
		}
		a, b = a[ia+1:], b[ib+1:]
	}
}

// IsUnder reports whether p is strictly below dir ("" is the root, above
// everything).
func IsUnder(p, dir []byte) bool {
	if len(dir) == 0 {
		return len(p) > 0
	}
	return len(p) > len(dir) && p[len(dir)] == '/' && bytes.Equal(p[:len(dir)], dir)
}

// ParentOf returns the parent path of p ("" for a top-level entry).
func ParentOf(p []byte) []byte {
	if i := bytes.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return nil
}

// BaseName returns the last component of p.
func BaseName(p []byte) []byte {
	if i := bytes.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Depth returns the number of components of p.
func Depth(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	return bytes.Count(p, []byte{'/'}) + 1
}
