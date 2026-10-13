package treebackup

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"
)

// DevIno identifies a file by device and inode number.
type DevIno struct{ Dev, Ino uint64 }

// Base is an earlier snapshot of the same tree. Its entries are streamed
// from Open each time they are needed and never held, so a base costs the
// walk no memory per entry. Entries, when set, are used instead of Open.
type Base struct {
	Header  *Header
	Open    OpenManifest
	Entries []Entry
}

// each passes the base's entries to f in manifest order until f returns
// false. f must not keep e or its slices. The manifest was validated in
// full before the walk; a read error now (the store is the host user's
// own) ends the stream early, and the walk does without the rest.
func (b *Base) each(f func(e *Entry) bool) {
	if b.Open == nil {
		for i := range b.Entries {
			if !f(&b.Entries[i]) {
				return
			}
		}
		return
	}
	r, err := b.Open()
	if err != nil {
		return
	}
	defer r.Close()
	for {
		e, err := r.Next()
		if err != nil || !f(e) {
			return
		}
	}
}

// WalkOptions configure one snapshot walk.
type WalkOptions struct {
	// Tree is the operator's path to the tree; it is resolved once.
	Tree   string
	Limits Limits
	// Exclude holds validated operator patterns: without a "/" a pattern
	// matches a name at any depth, with one it is anchored at the root and
	// matched per component.
	Exclude []string
	// WorkerDirs are tree-relative worker directories. Those that exist as
	// directories get their own share of the budgets.
	WorkerDirs [][]byte
	// Known is the newest complete snapshot of this tree; its paths are
	// looked up by name before anything else. Cache is the newest valid
	// one, for the stat cache. Either may be nil.
	Known *Base
	Cache *Base
	// Guard holds the identities of the store and the state directory: a
	// directory in the tree with one of them aborts the walk.
	Guard []DevIno
	// Full re-verifies every stat-cache hit, still only with leftover read
	// budget. Reverify re-verifies hits last verified longer ago (0: never).
	Full     bool
	Reverify time.Duration
	// Start is the snapshot's start time T; Deadline (zero: none) bounds
	// both passes.
	Start    time.Time
	Deadline time.Time
	// Interrupt is closed on the first Ctrl-C: the walk stops as on the
	// deadline, with reason interrupted. ctx is cancelled on the second.
	Interrupt <-chan struct{}
	Sink      BlobSink
	// With SpaceCapped, ManifestSpace is the space the manifest is sure to
	// have when it is written. When it is below Limits.Manifest it is the
	// manifest budget instead, and running out of it truncates with reason
	// disk.
	SpaceCapped   bool
	ManifestSpace int64
}

// WalkResult is one walked tree, ready for the manifest.
type WalkResult struct {
	TreeDev, TreeIno uint64
	Root             RootInfo
	// Entries are in manifest order, without duplicates.
	Entries []Entry
	// Flood is the top 3 directories by names listed in phase B.
	Flood         []Flood
	Badname       int64
	UnreadableNew int64
	Bytes         ByteCounts
	Blobs         BlobCounts
}

var (
	// ErrUnsupported means this platform or kernel cannot walk a tree
	// safely (Linux needs 5.8 or later with openat2).
	ErrUnsupported = errors.New("tree backups: unsupported platform")
	// ErrStateInTree means a directory in the tree is the store or the
	// state directory: walking it would back up the backups and secrets.
	ErrStateInTree = errors.New("tree backups: state directory inside the tree")
)

const (
	// ticketPath is the manager's broker ticket: hashed, never stored.
	ticketPath = ".lever/bootstrap.json"
	ticketMax  = 64 << 10

	dirCacheSize = 64
	listChunk    = 4096
)

// File type bits of st_mode; Linux and darwin share them.
const (
	modeFmt  = 0o170000
	modeDir  = 0o040000
	modeReg  = 0o100000
	modeLnk  = 0o120000
	modeFIFO = 0o010000
	modeSock = 0o140000
	modeChr  = 0o020000
	modeBlk  = 0o060000
)

// sysStat is the platform-neutral part of a stat result.
type sysStat struct {
	Dev, Ino     uint64
	Mode         uint32 // st_mode with the type bits
	Size         int64
	Mtime, Ctime int64
	Nlink        uint64
	Dataless     bool
	MountRoot    bool
}

func (s *sysStat) id() DevIno { return DevIno{s.Dev, s.Ino} }

type errClass int

const (
	errOther errClass = iota
	errGone
	errLoop
	errCrossDev
	errNotDir
	errNoDevice
	errDenied
)

func typeOf(mode uint32) EntryType {
	switch mode & modeFmt {
	case modeDir:
		return TypeDir
	case modeReg:
		return TypeFile
	case modeLnk:
		return TypeLink
	case modeFIFO:
		return TypeFIFO
	case modeSock:
		return TypeSocket
	case modeChr:
		return TypeChar
	case modeBlk:
		return TypeBlock
	default:
		return TypeUnknown
	}
}

// lineFixed bounds every part of an entry line except the path and target
// strings: each key at its longest spelling and each value at its widest.
var lineFixed = int64(len(`{"path_b64":"","type":"unknown","mode":4294967295,"size":-9223372036854775808,` +
	`"mtime":-9223372036854775808,"ctime":-9223372036854775808,"dev":18446744073709551615,` +
	`"ino":18446744073709551615,"nlink":18446744073709551615,"sha256":"` + strings.Repeat("0", 64) +
	`","verified":-9223372036854775808,"target_b64":"","state":"listing-unstable","reason":"interrupted",` +
	`"listing":"unreadable"}` + "\n"))

// maxLineBytes is an upper bound on the encoded manifest line of an entry
// with this path and target, whatever its other fields become later (a
// state, a hash). The manifest budget is charged with it, so a manifest
// within budget always passes the writer's self-check.
func maxLineBytes(path, target []byte) int64 {
	return lineFixed + maxStringBytes(path) + maxStringBytes(target)
}

// maxStringBytes bounds the JSON string body for b. It follows the
// encoder's rules, and also counts <, >, & and DEL as \u00XX so an encoder
// that escapes them stays within the bound.
func maxStringBytes(b []byte) int64 {
	if !utf8.Valid(b) {
		return int64(base64.StdEncoding.EncodedLen(len(b)))
	}
	var n int64
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c < 0x20, c == 0x7f, c == '<', c == '>', c == '&':
			n += 6
		case c == '"', c == '\\':
			n += 2
		case c == 0xe2 && i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xa8 || b[i+2] == 0xa9):
			n += 6
			i += 2
		default:
			n++
		}
	}
	return n
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func validRel(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for c := range bytes.SplitSeq(p, []byte{'/'}) {
		if !validName(string(c)) {
			return false
		}
	}
	return true
}

func joinRel(dir []byte, name string) []byte {
	if len(dir) == 0 {
		return []byte(name)
	}
	b := make([]byte, 0, len(dir)+1+len(name))
	b = append(b, dir...)
	b = append(b, '/')
	return append(b, name...)
}

type excluder struct {
	names    []string
	anchored [][]string
}

func newExcluder(patterns []string) excluder {
	var x excluder
	for _, p := range patterns {
		if strings.Contains(p, "/") {
			x.anchored = append(x.anchored, strings.Split(p, "/"))
		} else {
			x.names = append(x.names, p)
		}
	}
	return x
}

// match is called for every entry, and an excluded directory is never
// entered, so only the last component needs the name patterns.
func (x excluder) match(rel []byte) bool {
	name := string(BaseName(rel))
	for _, p := range x.names {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	if len(x.anchored) == 0 {
		return false
	}
	depth := Depth(rel)
	var comps []string
	for _, a := range x.anchored {
		if len(a) != depth {
			continue
		}
		if comps == nil {
			comps = strings.Split(string(rel), "/")
		}
		all := true
		for i := range a {
			if ok, _ := path.Match(a[i], comps[i]); !ok {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

type node struct {
	e Entry
	// walkable: a directory opened and matched by (dev, ino); its children
	// may be looked up.
	walkable bool
	// listed: phase B read its whole listing. attempted: phase B began to,
	// and set the state that explains any shortfall.
	listed    bool
	attempted bool
	// knownContent: the known base captured this path's content.
	// cacheHit: the stat cache supplied e.SHA256 and e.Verified.
	knownContent bool
	cacheHit     bool
	// listedMtime and listedCtime are the directory's times right after its listing,
	// for the recheck once every listing is done.
	listedMtime, listedCtime int64
	// listedNames counts the names phase B read from this directory.
	listedNames int64
	// kids are the walkable child directories.
	kids []*node
}

// writerRoot is the tree root or a worker directory: each gets a share of
// the phase-B and read budgets, so one writer cannot starve another.
type writerRoot struct {
	path         []byte
	known        int64
	stop         Reason
	eLeft, mLeft int64
	eUsed, mUsed int64
}

type walker struct {
	ctx  context.Context
	o    WalkOptions
	t    time.Time
	sink BlobSink

	rootfd   int
	dev0     uint64
	root     RootInfo
	rootNode *node
	// nodes is keyed by pathKey(e.Path), which shares the path's bytes.
	nodes   map[string]*node
	all     []*node
	visited map[DevIno]bool
	guard   map[DevIno]bool
	dirs    *dirCache
	excl    excluder

	pathMax  int
	manifest int64 // left, shared by phase A, worker roots and phase B
	// manifestReason is what running out of manifest truncates with: disk
	// when the space for the manifest, not the limit, set the budget.
	manifestReason Reason
	entries        int64 // left for phase B
	stop           Reason

	roots       []*writerRoot
	rootsByPath map[string]*writerRoot
	badname     int64

	pass2   bool
	diskLow bool
	linkBuf []byte
	readBuf []byte
	bytes   ByteCounts
	blobs   BlobCounts
}

// probe is the outcome of one lookup before it is added: a walkable
// directory carries its open fd.
type probe struct {
	e  Entry
	fd int
}

func (p *probe) release() {
	if p.fd >= 0 {
		sysClose(p.fd)
		p.fd = -1
	}
}

// Walk snapshots the tree's metadata and content into the sink. It fails
// only for faults outside the agent's control: an unsupported platform, a
// cancelled ctx, a store error other than a full disk, and a store or state
// directory found inside the tree. Everything else the tree does is recorded
// in the result.
func Walk(ctx context.Context, o WalkOptions) (*WalkResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := sysProbeKernel(); err != nil {
		return nil, err
	}
	treeReal, err := filepath.EvalSymlinks(o.Tree)
	if err != nil {
		return nil, fmt.Errorf("tree backups: resolve the tree: %w", err)
	}
	w := newWalker(ctx, o)
	defer w.close()
	res := &WalkResult{}

	fd, openErr := sysOpenRoot(treeReal)
	if openErr != nil {
		st, err := sysLstat(treeReal)
		if err != nil {
			return nil, fmt.Errorf("tree backups: open the tree: %w", openErr)
		}
		res.TreeDev, res.TreeIno = st.Dev, st.Ino
		res.Root = RootInfo{HasMeta: true, Mode: st.Mode & 0o7777, Mtime: st.Mtime, State: StateUnreadable}
		return res, nil
	}
	w.rootfd = fd
	st, err := sysFstat(fd)
	if err != nil {
		return nil, fmt.Errorf("tree backups: stat the tree: %w", err)
	}
	if w.guard[st.id()] {
		return nil, fmt.Errorf("%w: the tree is the state directory", ErrStateInTree)
	}
	res.TreeDev, res.TreeIno = st.Dev, st.Ino
	w.dev0 = st.Dev
	w.root = RootInfo{HasMeta: true, Mode: st.Mode & 0o7777, Mtime: st.Mtime}
	w.rootNode = &node{e: Entry{Type: TypeDir, HasMeta: true, Dev: st.Dev, Ino: st.Ino}, walkable: true}
	w.visited[st.id()] = true
	if err := sysProbeRoot(fd); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil, err
		}
		w.root.State = StateUnreadable
		res.Root = w.root
		return res, nil
	}

	if err := w.phaseA(); err != nil {
		return nil, err
	}
	if err := w.setupRoots(); err != nil {
		return nil, err
	}
	if err := w.phaseB(); err != nil {
		return nil, err
	}
	w.recheckListings()
	w.finishPass1()
	if err := w.pass2Content(); err != nil {
		return nil, err
	}
	w.fillResult(res)
	return res, nil
}

func newWalker(ctx context.Context, o WalkOptions) *walker {
	w := &walker{
		ctx:            ctx,
		o:              o,
		t:              o.Start,
		sink:           o.Sink,
		rootfd:         -1,
		nodes:          make(map[string]*node),
		visited:        make(map[DevIno]bool),
		guard:          make(map[DevIno]bool),
		excl:           newExcluder(o.Exclude),
		pathMax:        int(min(max(o.Limits.Path, 0), MaxPathCap)),
		manifest:       o.Limits.Manifest,
		manifestReason: ReasonManifest,
		entries:        o.Limits.Entries,
		rootsByPath:    make(map[string]*writerRoot),
		linkBuf:        make([]byte, MaxTargetCap),
	}
	if w.t.IsZero() {
		w.t = time.Now()
	}
	if o.SpaceCapped && o.ManifestSpace < w.manifest {
		w.manifest, w.manifestReason = o.ManifestSpace, ReasonDisk
	}
	for _, g := range o.Guard {
		w.guard[g] = true
	}
	w.dirs = &dirCache{w: w, ll: list.New(), m: make(map[string]*list.Element), bad: make(map[string]bool)}
	return w
}

func (w *walker) close() {
	w.dirs.closeAll()
	if w.rootfd >= 0 {
		sysClose(w.rootfd)
	}
}

// halted reports why the walk must stop now (time or interrupted), or an
// error when ctx was cancelled. Once set, the reason stays.
func (w *walker) halted() (Reason, error) {
	if err := w.ctx.Err(); err != nil {
		return "", err
	}
	if w.stop != "" {
		return w.stop, nil
	}
	select {
	case <-w.o.Interrupt:
		w.stop = ReasonInterrupted
		return w.stop, nil
	default:
	}
	if !w.o.Deadline.IsZero() && !time.Now().Before(w.o.Deadline) {
		w.stop = ReasonTime
	}
	return w.stop, nil
}

func (w *walker) nodeAt(p []byte) *node {
	if len(p) == 0 {
		return w.rootNode
	}
	return w.nodes[string(p)]
}

// pathFits applies the path limit and the component cap; ancestors were
// checked when they were recorded.
func (w *walker) pathFits(rel []byte) bool {
	return len(rel) <= w.pathMax && len(BaseName(rel)) <= MaxComponentCap
}

// truncate marks a directory as not fully listed for a budget. It never
// replaces a state that says more (changed, mount, ...); the first reason
// stays.
func (w *walker) truncate(n *node, r Reason) {
	if n == w.rootNode {
		if w.root.State == StateOK || w.root.State == StateListingUnstable {
			w.root.State, w.root.Reason = StateTruncated, r
		}
		return
	}
	if n.e.State == StateOK || n.e.State == StateListingUnstable {
		n.e.State, n.e.Reason = StateTruncated, r
	}
}

func (w *walker) listingUnstable(n *node) {
	if n == w.rootNode {
		if w.root.State == StateOK {
			w.root.State = StateListingUnstable
		}
		return
	}
	if n.e.State == StateOK {
		n.e.State = StateListingUnstable
	}
}

func (w *walker) listingUnreadable(n *node) {
	if n == w.rootNode {
		w.root.Listing = ListingUnreadable
		return
	}
	n.e.Listing = ListingUnreadable
}

// lost marks a recorded directory that no longer matches its identity, or
// could not be reopened: its children are not looked at again.
func (w *walker) lost(n *node, s State) {
	n.walkable = false
	if n == w.rootNode {
		return
	}
	if n.e.State == StateOK || n.e.State == StateListingUnstable {
		n.e.State, n.e.Reason = s, ""
	}
}

func entryFromStat(rel []byte, st *sysStat) Entry {
	e := Entry{
		Path:    rel,
		Type:    typeOf(st.Mode),
		HasMeta: true,
		Mode:    st.Mode & 0o7777,
		Size:    st.Size,
		Mtime:   st.Mtime,
		Ctime:   st.Ctime,
		Dev:     st.Dev,
		Ino:     st.Ino,
		Nlink:   st.Nlink,
	}
	// A file type outside the known set (a darwin whiteout) is never
	// opened; "unknown" always carries "unreadable".
	if e.Type == TypeUnknown {
		e.State = StateUnreadable
	}
	return e
}

// lookup records one name of the directory dirfd, without adding it. gone
// means the name no longer exists. The only error is the walker guard.
func (w *walker) lookup(dirfd int, rel []byte, name string) (probe, bool, error) {
	p := probe{fd: -1}
	st, err := sysFstatat(dirfd, name)
	if err != nil {
		if classify(err) == errGone {
			return p, true, nil
		}
		p.e = Entry{Path: rel, Type: TypeUnknown, State: StateUnreadable}
		return p, false, nil
	}
	if walkSeams.stat != nil {
		walkSeams.stat(rel, &st)
	}
	p.e = entryFromStat(rel, &st)
	if p.e.Type == TypeDir && w.guard[st.id()] {
		return p, false, fmt.Errorf("%w: %s", ErrStateInTree, Q(rel))
	}
	if st.MountRoot {
		p.e.State = StateMount
		return p, false, nil
	}
	if p.e.Type == TypeFile && st.Size <= ticketMax && string(rel) == ticketPath {
		p.e.State = StateSecret
		return p, false, nil
	}
	if w.excl.match(rel) {
		p.e.State = StateExcluded
		return p, false, nil
	}
	if st.Dataless {
		p.e.State = StateDataless
		return p, false, nil
	}
	switch p.e.Type {
	case TypeDir:
		w.lookupDir(&p, dirfd, name, &st)
	case TypeLink:
		n, err := sysReadlinkat(dirfd, name, w.linkBuf)
		if err != nil || n >= len(w.linkBuf) {
			p.e.State = StateUnreadable
			break
		}
		// A link swapped between the stat and the readlink would pair one
		// object's metadata with another's target.
		again, err := sysFstatat(dirfd, name)
		if err != nil || again.id() != st.id() || again.Ctime != st.Ctime || typeOf(again.Mode) != TypeLink {
			p.e.State = StateChanged
			break
		}
		p.e.Target = bytes.Clone(w.linkBuf[:n])
		p.e.HasTarget = true
	}
	return p, false, nil
}

func (w *walker) lookupDir(p *probe, dirfd int, name string, st *sysStat) {
	if int64(Depth(p.e.Path)) >= w.o.Limits.Depth {
		p.e.State, p.e.Reason = StateTruncated, ReasonDepth
		return
	}
	if platformChecksDev && st.Dev != w.dev0 {
		p.e.State = StateMount
		return
	}
	if walkSeams.beforeOpen != nil {
		walkSeams.beforeOpen(p.e.Path)
	}
	fd, err := sysOpenDir(dirfd, name)
	if err != nil {
		switch classify(err) {
		case errCrossDev:
			p.e.State = StateMount
		case errLoop, errNotDir, errGone:
			p.e.State = StateChanged
		default:
			p.e.State = StateUnreadable
		}
		return
	}
	fst, err := sysFstat(fd)
	switch {
	case err != nil:
		p.e.State = StateUnreadable
	case fst.id() != st.id():
		p.e.State = StateChanged
	case w.visited[fst.id()]:
		// Moved here from a place already walked.
		p.e.State = StateChanged
	default:
		path := p.e.Path
		p.e = entryFromStat(path, &fst)
		p.fd = fd
		return
	}
	sysClose(fd)
}

// pathKey is p as a map key without a copy of its bytes. The paths of
// recorded entries are never written after they are made, which is what
// makes sharing them sound; each path is then held once, not once more per
// map.
func pathKey(p []byte) string { return unsafe.String(unsafe.SliceData(p), len(p)) }

// add records a probe. Its path must not be written afterwards (pathKey).
func (w *walker) add(p probe) *node {
	n := &node{e: p.e}
	w.nodes[pathKey(p.e.Path)] = n
	w.all = append(w.all, n)
	if p.fd >= 0 {
		n.walkable = true
		w.visited[DevIno{p.e.Dev, p.e.Ino}] = true
		w.dirs.put(string(p.e.Path), p.fd, DevIno{p.e.Dev, p.e.Ino})
		parent := w.nodeAt(ParentOf(p.e.Path))
		parent.kids = append(parent.kids, n)
	}
	return n
}

// phaseA looks up every path of the known base by name, before any budget
// for new names is spent. It is charged to the manifest budget: an agent
// can grow every known line (a 4 KiB target of control bytes encodes to
// about 24 KiB), so an uncharged phase A could produce a manifest the
// validator rejects.
//
// Each known path's line without a target is reserved up front. It fit when
// the base was written under the same limits, so only the growth an agent
// adds (link targets) can run out, and then only that entry is dropped and
// its parent truncated: every other known path is still looked up.
func (w *walker) phaseA() error {
	if w.o.Known == nil {
		return nil
	}
	var reserved int64
	w.o.Known.each(func(ke *Entry) bool {
		reserved += maxLineBytes(ke.Path, nil)
		return true
	})
	reserve := reserved <= w.manifest
	if reserve {
		w.manifest -= reserved
		defer func() { w.manifest += reserved }()
	}
	var err error
	w.o.Known.each(func(ke *Entry) bool {
		var r Reason
		if r, err = w.halted(); err != nil || r != "" {
			return false
		}
		pp := ParentOf(ke.Path)
		pn := w.nodeAt(pp)
		if pn == nil || !pn.walkable || w.nodes[string(ke.Path)] != nil {
			return true
		}
		if !w.pathFits(ke.Path) {
			w.truncate(pn, ReasonPath)
			return true
		}
		dfd, ok := w.dirs.get(pp)
		if !ok {
			return true
		}
		p, gone, lerr := w.lookup(dfd, bytes.Clone(ke.Path), string(BaseName(ke.Path)))
		if lerr != nil {
			err = lerr
			return false
		}
		if gone {
			return true
		}
		cost := maxLineBytes(p.e.Path, p.e.Target)
		var held int64
		if reserve {
			held = maxLineBytes(ke.Path, nil)
		}
		if cost-held > w.manifest {
			p.release()
			w.truncate(pn, w.manifestReason)
			return true
		}
		w.manifest -= cost - held
		reserved -= held
		w.add(p)
		return true
	})
	return err
}

func (w *walker) setupRoots() error {
	tree := &writerRoot{}
	w.roots = []*writerRoot{tree}
	w.rootsByPath[""] = tree
	for _, wd := range w.o.WorkerDirs {
		if !validRel(wd) || w.rootsByPath[string(wd)] != nil {
			continue
		}
		ok, err := w.ensureDir(wd)
		if err != nil {
			return err
		}
		if ok {
			r := &writerRoot{path: bytes.Clone(wd)}
			w.roots = append(w.roots, r)
			w.rootsByPath[string(wd)] = r
		}
	}
	if w.o.Known != nil && len(w.roots) > 1 {
		w.o.Known.each(func(e *Entry) bool {
			w.rootOf(e.Path).known++
			return true
		})
	}
	sort.SliceStable(w.roots, func(i, j int) bool {
		if w.roots[i].known != w.roots[j].known {
			return w.roots[i].known < w.roots[j].known
		}
		return ComparePaths(w.roots[i].path, w.roots[j].path) < 0
	})
	return nil
}

func (w *walker) ensureDir(p []byte) (bool, error) {
	for i := 0; i <= len(p); i++ {
		if i < len(p) && p[i] != '/' {
			continue
		}
		prefix := p[:i]
		n := w.nodes[string(prefix)]
		if n == nil {
			pp := ParentOf(prefix)
			if pn := w.nodeAt(pp); pn == nil || !pn.walkable || !w.pathFits(prefix) {
				return false, nil
			}
			if r, err := w.halted(); err != nil || r != "" {
				return false, err
			}
			dfd, ok := w.dirs.get(pp)
			if !ok {
				return false, nil
			}
			pr, gone, err := w.lookup(dfd, bytes.Clone(prefix), string(BaseName(prefix)))
			if err != nil || gone {
				return false, err
			}
			cost := maxLineBytes(pr.e.Path, pr.e.Target)
			if cost > w.manifest {
				pr.release()
				return false, nil
			}
			w.manifest -= cost
			n = w.add(pr)
		}
		if !n.walkable {
			return false, nil
		}
	}
	return true, nil
}

// rootOf returns the writer root whose walk lists the name p: the deepest
// root strictly above p.
func (w *walker) rootOf(p []byte) *writerRoot {
	for a := ParentOf(p); ; a = ParentOf(a) {
		if r := w.rootsByPath[string(a)]; r != nil {
			return r
		}
		if len(a) == 0 {
			return w.rootsByPath[""]
		}
	}
}

func (w *walker) listerOf(d []byte) *writerRoot {
	if r := w.rootsByPath[string(d)]; r != nil {
		return r
	}
	return w.rootOf(d)
}

// phaseB lists directories for new names, one writer root at a time. Each
// root gets an equal part of what is left, so a root's unused share flows on
// to the later (larger) roots.
func (w *walker) phaseB() error {
	for i, r := range w.roots {
		left := int64(len(w.roots) - i)
		r.eLeft = max(w.entries, 0) / left
		r.mLeft = max(w.manifest, 0) / left
		err := w.walkRoot(r)
		w.entries -= r.eUsed
		w.manifest -= r.mUsed
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) walkRoot(r *writerRoot) error {
	if reason, err := w.halted(); err != nil {
		return err
	} else if reason != "" {
		r.stop = reason
		return nil
	}
	n := w.nodeAt(r.path)
	if n == nil || !n.walkable {
		return nil
	}
	if len(r.path) == 0 {
		return w.walkDir(r, n, w.rootfd)
	}
	pfd, ok := w.dirs.get(ParentOf(r.path))
	if !ok {
		return nil
	}
	fd, ok := w.openChild(pfd, n)
	if !ok {
		return nil
	}
	defer sysClose(fd)
	return w.walkDir(r, n, fd)
}

// openChild reopens a recorded directory; it is used only if it is still the
// object recorded.
func (w *walker) openChild(pfd int, n *node) (int, bool) {
	fd, err := sysOpenDir(pfd, string(BaseName(n.e.Path)))
	if err != nil {
		// Even a mount that appeared since this directory was recorded is
		// "changed", not "mount": the directory may already hold recorded
		// children, and a mount is a leaf.
		w.lost(n, StateChanged)
		return -1, false
	}
	st, err := sysFstat(fd)
	if err != nil || st.id() != (DevIno{n.e.Dev, n.e.Ino}) {
		sysClose(fd)
		w.lost(n, StateChanged)
		return -1, false
	}
	return fd, true
}

func (w *walker) walkDir(r *writerRoot, n *node, fd int) error {
	if err := w.listDir(r, n, fd); err != nil || r.stop != "" {
		return err
	}
	kids := n.kids
	slices.SortFunc(kids, func(a, b *node) int { return bytes.Compare(BaseName(a.e.Path), BaseName(b.e.Path)) })
	kids = slices.Compact(kids)
	n.kids = kids
	for _, kn := range kids {
		if reason, err := w.halted(); err != nil {
			return err
		} else if reason != "" {
			r.stop = reason
			return nil
		}
		if !kn.walkable || kn.listed || w.rootsByPath[string(kn.e.Path)] != nil {
			continue
		}
		cfd, ok := w.openChild(fd, kn)
		if !ok {
			continue
		}
		err := w.walkDir(r, kn, cfd)
		sysClose(cfd)
		if err != nil || r.stop != "" {
			return err
		}
	}
	return nil
}

// listDir reads one directory and records its new names. Each name is
// charged to entries, dir_entries and (when it will get a line) manifest as
// soon as it is read, so held listings never outgrow the budgets.
func (w *walker) listDir(r *writerRoot, n *node, fd int) error {
	dir := n.e.Path
	n.attempted = true
	if walkSeams.beforeList != nil {
		walkSeams.beforeList(dir)
	}
	t0 := time.Now().UnixNano()
	lfd, err := sysOpenListing(fd)
	if err != nil {
		w.listingUnreadable(n)
		return nil
	}
	f := os.NewFile(uintptr(lfd), "listing")
	defer f.Close()
	before, err := sysFstat(lfd)
	if err != nil {
		w.listingUnreadable(n)
		return nil
	}

	var names []string
	var count int64
	var buf []byte
	full := true
read:
	for chunk := 0; ; chunk++ {
		if reason, err := w.halted(); err != nil {
			return err
		} else if reason != "" {
			r.stop = reason
			w.truncate(n, reason)
			return nil
		}
		batch, rerr := f.Readdirnames(listChunk)
		if walkSeams.listed != nil {
			batch = walkSeams.listed(dir, chunk, batch)
		}
		for _, name := range batch {
			if r.eLeft <= 0 {
				r.stop = ReasonEntries
				full = false
				break read
			}
			r.eLeft--
			r.eUsed++
			n.listedNames++
			if count >= w.o.Limits.DirEntries {
				w.truncate(n, ReasonDirEntries)
				full = false
				break read
			}
			count++
			if !validName(name) {
				w.badname++
				continue
			}
			// The path is built in a reused buffer: only the names that get
			// an entry get their own copy, in the lookup below.
			buf = append(append(append(buf[:0], dir...), '/'), name...)
			if len(dir) == 0 {
				buf = append(buf[:0], name...)
			}
			if w.nodes[string(buf)] != nil {
				continue
			}
			if !w.pathFits(buf) {
				w.truncate(n, ReasonPath)
				continue
			}
			cost := maxLineBytes(buf, nil)
			if cost > r.mLeft {
				r.stop = w.manifestReason
				full = false
				break read
			}
			r.mLeft -= cost
			r.mUsed += cost
			names = append(names, name)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			w.listingUnreadable(n)
			full = false
			break
		}
	}

	slices.Sort(names)
	kept := names[:0]
	for i, name := range names {
		if i > 0 && name == names[i-1] {
			w.badname++
			continue
		}
		kept = append(kept, name)
	}
	unstable := false
	for i, name := range kept {
		if i%listChunk == listChunk-1 {
			if reason, err := w.halted(); err != nil {
				return err
			} else if reason != "" {
				r.stop = reason
				full = false
				break
			}
		}
		rel := joinRel(dir, name)
		p, gone, err := w.lookup(fd, rel, name)
		if err != nil {
			return err
		}
		if gone {
			// Listed but gone: a rename race may have hidden a name too.
			unstable = true
			continue
		}
		if p.e.HasTarget {
			extra := maxLineBytes(rel, p.e.Target) - maxLineBytes(rel, nil)
			if extra > r.mLeft {
				p.release()
				r.stop = w.manifestReason
				full = false
				break
			}
			r.mLeft -= extra
			r.mUsed += extra
		}
		w.add(p)
	}
	// Equal timestamps prove nothing when the change fell in the same
	// timestamp tick as the first fstat, so a directory changed that
	// recently counts as unstable too. ctime cannot be set by the agent.
	after, err := sysFstat(lfd)
	if err != nil || after.Mtime != before.Mtime || after.Ctime != before.Ctime ||
		after.Ctime >= t0-listingWindow.Nanoseconds() {
		unstable = true
	}
	if unstable {
		w.listingUnstable(n)
	}
	if r.stop != "" {
		w.truncate(n, r.stop)
	}
	if full && n.e.Listing == "" && (n != w.rootNode || w.root.Listing == "") {
		n.listed = true
		if err == nil {
			n.listedMtime, n.listedCtime = after.Mtime, after.Ctime
		}
	}
	return nil
}

// recheckListings stats every fully listed directory again once phase B
// is over. A name moved into a directory after its listing, out of one not
// listed yet, is in neither listing; the second directory's own check
// catches the removal only if it is listed later, so the first must be
// checked again here. A directory that cannot be checked counts as changed.
func (w *walker) recheckListings() {
	var listed []*node
	if w.rootNode != nil && w.rootNode.listed {
		listed = append(listed, w.rootNode)
	}
	for _, n := range w.all {
		if n.listed && n != w.rootNode {
			listed = append(listed, n)
		}
	}
	slices.SortFunc(listed, func(a, b *node) int { return ComparePaths(a.e.Path, b.e.Path) })
	for _, n := range listed {
		var p []byte
		if n != w.rootNode {
			p = n.e.Path
		}
		fd, ok := w.dirs.get(p)
		if !ok {
			w.listingUnstable(n)
			continue
		}
		st, err := sysFstat(fd)
		if err != nil || st.Mtime != n.listedMtime || st.Ctime != n.listedCtime {
			w.listingUnstable(n)
		}
	}
}

// finishPass1 sorts the entries and marks every walkable directory phase B
// did not list completely. Only a phase-B listing proves which names a
// directory holds; without the mark, a name missing from such a directory
// would read as deleted.
func (w *walker) finishPass1() {
	slices.SortFunc(w.all, func(a, b *node) int { return ComparePaths(a.e.Path, b.e.Path) })
	mark := func(n *node, d []byte) {
		if !n.walkable || n.attempted || n.e.Listing != "" {
			return
		}
		if n == w.rootNode && w.root.Listing != "" {
			return
		}
		reason := w.listerOf(d).stop
		if reason == "" {
			reason = w.stop
		}
		switch {
		case reason != "":
			w.truncate(n, reason)
		case n == w.rootNode:
			// The tree root's walk always lists the root unless it stopped.
			w.truncate(n, ReasonTime)
		default:
			// Not reached by a walk that did not stop: an ancestor was
			// lost after phase A recorded this directory. Its files keep
			// their own (dev, ino) checks in pass 2.
			if n.e.State == StateOK || n.e.State == StateListingUnstable {
				n.e.State = StateChanged
			}
		}
	}
	mark(w.rootNode, nil)
	for _, n := range w.all {
		if n.e.Type == TypeDir {
			mark(n, n.e.Path)
		}
	}
}

func (w *walker) fillResult(res *WalkResult) {
	res.Flood = w.flood()
	// Every node becomes garbage as its entry is copied out (a parent comes
	// before its children, and the maps are dropped first), so the nodes
	// and the entries are not both held in full.
	w.nodes, w.visited = nil, nil
	w.rootNode.kids = nil
	res.Root = w.root
	res.Entries = make([]Entry, len(w.all))
	for i, n := range w.all {
		res.Entries[i] = n.e
		n.kids = nil
		w.all[i] = nil
	}
	res.Badname = w.badname
	res.Bytes = w.bytes
	res.Blobs = w.blobs
	for i := range res.Entries {
		e := &res.Entries[i]
		if e.SHA256 != "" && e.State != StateSecret {
			res.Bytes.Total += e.Size
		}
	}
	res.UnreadableNew = w.unreadableNew(res.Entries)
}

// flood returns the three directories phase B read the most names from.
func (w *walker) flood() []Flood {
	var top []*node
	better := func(a, b *node) bool {
		if a.listedNames != b.listedNames {
			return a.listedNames > b.listedNames
		}
		return ComparePaths(a.e.Path, b.e.Path) < 0
	}
	consider := func(n *node) {
		if n.listedNames == 0 {
			return
		}
		top = append(top, n)
		for k := len(top) - 1; k > 0 && better(top[k], top[k-1]); k-- {
			top[k], top[k-1] = top[k-1], top[k]
		}
		if len(top) > 3 {
			top = top[:3]
		}
	}
	consider(w.rootNode)
	for _, n := range w.all {
		consider(n)
	}
	var out []Flood
	for _, n := range top {
		out = append(out, Flood{At: n.e.Path, HasAt: n != w.rootNode, Count: n.listedNames})
	}
	return out
}

// unreadableNew counts unreadable entries whose path was not unreadable in
// the cache base. Both are in manifest order.
func (w *walker) unreadableNew(es []Entry) int64 {
	var n int64
	i := 0
	countUntil := func(p []byte) {
		for ; i < len(es) && (p == nil || ComparePaths(es[i].Path, p) < 0); i++ {
			if es[i].State == StateUnreadable {
				n++
			}
		}
	}
	if w.o.Cache != nil {
		w.o.Cache.each(func(b *Entry) bool {
			countUntil(b.Path)
			if i < len(es) && bytes.Equal(es[i].Path, b.Path) {
				if es[i].State == StateUnreadable && b.State != StateUnreadable {
					n++
				}
				i++
			}
			return i < len(es)
		})
	}
	countUntil(nil)
	return n
}

// dirCache holds up to dirCacheSize directory fds by path. A miss reopens
// the chain from the deepest cached ancestor, each step checked against the
// (dev, ino) this walk recorded, so a directory swapped since is never
// used.
type dirCache struct {
	w   *walker
	ll  *list.List
	m   map[string]*list.Element
	bad map[string]bool
}

type cachedDir struct {
	key string
	fd  int
}

func (c *dirCache) put(key string, fd int, _ DevIno) {
	if el, ok := c.m[key]; ok {
		sysClose(el.Value.(*cachedDir).fd)
		c.ll.Remove(el)
	}
	c.m[key] = c.ll.PushFront(&cachedDir{key: key, fd: fd})
	// The front entries are the ones in use (just added or just returned
	// by get), so evicting from the back never closes them.
	for c.ll.Len() > dirCacheSize {
		el := c.ll.Back()
		cd := el.Value.(*cachedDir)
		sysClose(cd.fd)
		delete(c.m, cd.key)
		c.ll.Remove(el)
	}
}

func (c *dirCache) closeAll() {
	for el := c.ll.Front(); el != nil; el = el.Next() {
		sysClose(el.Value.(*cachedDir).fd)
	}
	c.ll.Init()
	clear(c.m)
}

// get returns a cached fd for the recorded directory p. The fd stays owned
// by the cache and is valid until the next put.
func (c *dirCache) get(p []byte) (int, bool) {
	w := c.w
	if len(p) == 0 {
		return w.rootfd, true
	}
	if el, ok := c.m[string(p)]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cachedDir).fd, true
	}
	if c.bad[string(p)] {
		return -1, false
	}
	start := ParentOf(p)
	cur := w.rootfd
	for len(start) > 0 {
		if el, ok := c.m[string(start)]; ok {
			c.ll.MoveToFront(el)
			cur = el.Value.(*cachedDir).fd
			break
		}
		start = ParentOf(start)
	}
	i := len(start)
	if i > 0 {
		i++
	}
	for i <= len(p) {
		j := bytes.IndexByte(p[i:], '/')
		end := len(p)
		if j >= 0 {
			end = i + j
		}
		prefix := p[:end]
		n := w.nodes[string(prefix)]
		if n == nil || !n.walkable || n.e.Type != TypeDir {
			c.bad[string(p)] = true
			return -1, false
		}
		fd, err := sysOpenDir(cur, string(p[i:end]))
		var st sysStat
		if err == nil {
			st, err = sysFstat(fd)
			if err == nil && st.id() != (DevIno{n.e.Dev, n.e.Ino}) {
				err = errors.New("identity changed")
			}
			if err != nil {
				sysClose(fd)
			}
		}
		if err != nil {
			c.bad[string(prefix)] = true
			c.bad[string(p)] = true
			if !w.pass2 {
				w.lost(n, StateChanged)
			}
			return -1, false
		}
		c.put(string(prefix), fd, st.id())
		cur = fd
		i = end + 1
	}
	return cur, true
}
