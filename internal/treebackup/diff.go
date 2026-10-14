package treebackup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Status letters of a diff line.
const (
	StatusAdded    byte = 'A'
	StatusDeleted  byte = 'D'
	StatusModified byte = 'M'
	StatusType     byte = 'T'
	StatusLink     byte = 'L'
	StatusMode     byte = 'P'
	StatusUnknown  byte = '?'
	// StatusMeta is an mtime-only change, shown only with DiffOptions.Meta.
	StatusMeta byte = '~'
)

// Diff output caps.
const (
	DiffOutputLimit   = 8 << 20
	DiffStatThreshold = 500
	diffTextMax       = 1 << 20
	// absentUnknownCap bounds the per-name "?" lines for names one side
	// did not list; the rest are counted.
	absentUnknownCap = 1000
)

// DiffOptions select what Diff prints.
type DiffOptions struct {
	Paths    [][]byte // limit to these paths and what is below them
	Stat     bool     // the summary only, no text sections
	Meta     bool     // show mtime-only changes (status '~')
	NoLimit  bool     // lift the output cap
	FullDiff bool     // text sections even over DiffStatThreshold changed paths
	Blobs    BlobSource
}

// DiffCounts counts changed entries by status. Entries folded under an A,
// D or T line are counted too.
type DiffCounts struct {
	A, D, M, T, L, P, Unknown int64
	Meta                      int64
}

func (c DiffCounts) total() int64 { return c.A + c.D + c.M + c.T + c.L + c.P + c.Unknown + c.Meta }

func (c *DiffCounts) add(status byte) {
	switch status {
	case StatusAdded:
		c.A++
	case StatusDeleted:
		c.D++
	case StatusModified:
		c.M++
	case StatusType:
		c.T++
	case StatusLink:
		c.L++
	case StatusMode:
		c.P++
	case StatusUnknown:
		c.Unknown++
	case StatusMeta:
		c.Meta++
	}
}

func (c DiffCounts) String() string {
	s := fmt.Sprintf("A %d, D %d, M %d, T %d, L %d, P %d, ? %d", c.A, c.D, c.M, c.T, c.L, c.P, c.Unknown)
	if c.Meta > 0 {
		s += fmt.Sprintf(", ~ %d", c.Meta)
	}
	return s
}

// change is one diff line: a path that is not folded under another line.
type change struct {
	Status byte
	Path   []byte
	Old    *Entry // nil when absent
	New    *Entry
	Note   string // rendered; holds no raw tree bytes
	// Below counts the entries folded under an A, D or T line.
	Below int64
	// Absent marks a '?' for a name one side did not list.
	Absent bool
}

// foldTop is an A, D or T line that later entries fold into.
type foldTop struct {
	c        *change
	selected bool
	// child is the status of the folded entries: A or D, or for a T line,
	// depending on which side holds them.
	child byte
}

// mergeFrame is one ancestor on the merged tree. oldFull and newFull say
// whether a name absent on that side below this point really is absent
// (the nearest present ancestor and all of its ancestors were fully
// listed); oldWhy and newWhy name the cause when not.
type mergeFrame struct {
	path             []byte
	oldFull, newFull bool
	oldWhy, newWhy   string
	fold             *foldTop
}

// mergeSink receives the merge's results. change gets every line in
// manifest order, an A/D/T line once its folded entries are counted. item
// sees every entry, folded or not, when it is selected.
type mergeSink struct {
	change func(c *change) error
	item   func(path []byte, o, n *Entry)
}

type merger struct {
	paths   [][]byte
	meta    bool
	sink    mergeSink
	counts  DiffCounts
	stack   []mergeFrame
	pending *change
}

func rootFull(h *Header) (bool, string) {
	r := h.Root
	switch {
	case r.State == StateTruncated:
		return false, "the tree root is truncated (" + word(r.Reason) + ")"
	case r.State != StateOK:
		return false, "the tree root is " + word(r.State)
	case r.Listing != "":
		return false, "the tree root's listing is " + word(r.Listing)
	}
	return true, ""
}

// mergeManifests streams old and new together in manifest order. It reads
// both to the end, so a nil error also means both validated.
func mergeManifests(oldM, newM OpenManifest, paths [][]byte, meta bool, sink mergeSink) (DiffCounts, error) {
	or, err := oldM()
	if err != nil {
		return DiffCounts{}, err
	}
	defer or.Close()
	nr, err := newM()
	if err != nil {
		return DiffCounts{}, err
	}
	defer nr.Close()
	return mergeReaders(or, nr, paths, meta, sink)
}

func mergeReaders(or, nr EntryReader, paths [][]byte, meta bool, sink mergeSink) (DiffCounts, error) {
	m := &merger{paths: paths, meta: meta, sink: sink}
	of, ow := rootFull(or.Header())
	nf, nw := rootFull(nr.Header())
	m.stack = append(m.stack, mergeFrame{oldFull: of, newFull: nf, oldWhy: ow, newWhy: nw})

	next := func(r EntryReader, prev []byte) (*Entry, error) {
		e, err := r.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if prev != nil && ComparePaths(prev, e.Path) >= 0 {
			return nil, fmt.Errorf("%w: entry %s out of order", ErrInvalid, Q(e.Path))
		}
		return e, nil
	}
	o, err := next(or, nil)
	if err != nil {
		return m.counts, err
	}
	n, err := next(nr, nil)
	if err != nil {
		return m.counts, err
	}
	for o != nil || n != nil {
		var c int
		switch {
		case o == nil:
			c = 1
		case n == nil:
			c = -1
		default:
			c = ComparePaths(o.Path, n.Path)
		}
		var po, pn *Entry
		var p []byte
		switch {
		case c < 0:
			po, p = o, o.Path
		case c > 0:
			pn, p = n, n.Path
		default:
			po, pn, p = o, n, o.Path
		}
		if err := m.handle(p, po, pn); err != nil {
			return m.counts, err
		}
		if po != nil {
			if o, err = next(or, po.Path); err != nil {
				return m.counts, err
			}
		}
		if pn != nil {
			if n, err = next(nr, pn.Path); err != nil {
				return m.counts, err
			}
		}
	}
	if err := m.flush(); err != nil {
		return m.counts, err
	}
	return m.counts, nil
}

func (m *merger) selected(p []byte) bool {
	if len(m.paths) == 0 {
		return true
	}
	for _, f := range m.paths {
		if bytes.Equal(p, f) || IsUnder(p, f) {
			return true
		}
	}
	return false
}

func (m *merger) flush() error {
	c := m.pending
	m.pending = nil
	if c == nil || m.sink.change == nil {
		return nil
	}
	return m.sink.change(c)
}

func (m *merger) emit(c *change, fold bool) error {
	if err := m.flush(); err != nil {
		return err
	}
	if fold {
		m.pending = c
		return nil
	}
	if m.sink.change == nil {
		return nil
	}
	return m.sink.change(c)
}

func cloneEntry(e *Entry) *Entry {
	if e == nil {
		return nil
	}
	c := *e
	c.Path = bytes.Clone(e.Path)
	c.Target = bytes.Clone(e.Target)
	return &c
}

func (m *merger) handle(p []byte, o, n *Entry) error {
	for len(m.stack) > 1 && !IsUnder(p, m.stack[len(m.stack)-1].path) {
		m.stack = m.stack[:len(m.stack)-1]
	}
	parent := &m.stack[len(m.stack)-1]
	sel := m.selected(p)
	o, n = cloneEntry(o), cloneEntry(n)
	p = bytes.Clone(p)
	frame := mergeFrame{path: p}
	frame.oldFull, frame.oldWhy = childFull(parent.oldFull, parent.oldWhy, o, n, true)
	frame.newFull, frame.newWhy = childFull(parent.newFull, parent.newWhy, n, o, false)

	if sel && m.sink.item != nil {
		m.sink.item(p, o, n)
	}

	if f := parent.fold; f != nil {
		status := f.child
		if f.c.Status == StatusType {
			status = StatusAdded
			if o != nil {
				status = StatusDeleted
			}
		}
		frame.fold = f
		if sel {
			m.counts.add(status)
			if f.selected {
				f.c.Below++
			} else {
				// The line this folds into is outside the selected paths:
				// this entry becomes the line, so the selection still shows
				// it.
				e := n
				if e == nil {
					e = o
				}
				c := &change{Status: status, Path: p, Old: o, New: n,
					Note: entrySummary(e) + ", under " + Q(f.c.Path) + " (" + string(f.c.Status) + ")"}
				frame.fold = &foldTop{c: c, selected: true, child: status}
				if err := m.emit(c, true); err != nil {
					return err
				}
			}
		}
		m.stack = append(m.stack, frame)
		return nil
	}

	var c *change
	fold := false
	switch {
	case o != nil && n != nil:
		c = compareEntries(p, o, n, m.meta)
		if c != nil && c.Status == StatusType {
			fold = true
		}
	case o != nil:
		if parent.newFull {
			c = &change{Status: StatusDeleted, Path: p, Old: o, Note: entrySummary(o)}
			fold = true
		} else {
			c = &change{Status: StatusUnknown, Path: p, Old: o, Absent: true,
				Note: "not seen in new: " + parent.newWhy}
		}
	default:
		if parent.oldFull {
			c = &change{Status: StatusAdded, Path: p, New: n, Note: entrySummary(n)}
			fold = true
		} else {
			c = &change{Status: StatusUnknown, Path: p, New: n, Absent: true,
				Note: "not seen in old: " + parent.oldWhy}
		}
	}
	if c != nil && fold {
		frame.fold = &foldTop{c: c, selected: sel, child: c.Status}
	}
	m.stack = append(m.stack, frame)
	if c == nil || !sel {
		return nil
	}
	m.counts.add(c.Status)
	return m.emit(c, fold)
}

// childFull computes, for one side, whether names absent on that side
// below this entry are really absent. self is this side's entry, other the
// opposite side's (for the "became" wording).
func childFull(parentFull bool, parentWhy string, self, other *Entry, isOld bool) (bool, string) {
	if self == nil {
		return parentFull, parentWhy
	}
	if !parentFull {
		return false, parentWhy
	}
	if self.Type != TypeDir {
		// A T line folds these; an unknown entry hides them.
		return false, Q(self.Path) + " is " + typeWord(self)
	}
	why := ""
	switch {
	case self.State == StateTruncated:
		why = "truncated (" + word(self.Reason) + ")"
	case self.State == StateUnreadable:
		why = "unreadable"
		if !isOld && other != nil && other.State != StateUnreadable {
			why = "became unreadable"
		}
	case self.State != StateOK:
		why = word(self.State)
	case self.Listing != "":
		why = "listing " + word(self.Listing)
		if !isOld && other != nil && other.Listing == "" {
			why = "listing became " + word(self.Listing)
		}
	default:
		return true, ""
	}
	return false, Q(self.Path) + " " + why
}

// word renders a value from one of the manifest's closed sets. The reader
// validated it; it still goes through Q so nothing raw reaches the terminal.
func word[S ~string](v S) string { return qs(string(v)) }

func typeWord(e *Entry) string {
	if e.Type == TypeUnknown && e.State != StateOK {
		return "unknown (" + word(e.State) + ")"
	}
	return word(e.Type)
}

// hidingState reports states under which the entry's content or listing
// was not captured.
func hidingState(s State) bool {
	switch s {
	case StateOK, StateUnstable, StateSecret:
		return false
	}
	return true
}

func stateWord(e *Entry) string {
	switch {
	case e.State == StateOK && e.Listing != "":
		return "listing " + word(e.Listing)
	case e.State == StateOK:
		return "ok"
	case e.Reason != "":
		return word(e.State) + " (" + word(e.Reason) + ")"
	}
	return word(e.State)
}

// becameWord describes a state change for a '?' line.
func becameWord(o, n *Entry) string {
	if n.State == StateUnreadable && o.State != StateUnreadable {
		return "became unreadable"
	}
	if n.Listing != "" && o.Listing == "" && n.State == StateOK {
		return "listing became " + word(n.Listing)
	}
	if o.State == StateUnreadable && n.State != StateUnreadable {
		return "was unreadable in old, now " + stateWord(n)
	}
	return "state " + stateWord(o) + " → " + stateWord(n)
}

func compareEntries(p []byte, o, n *Entry, meta bool) *change {
	c := &change{Path: p, Old: o, New: n}
	var notes []string
	modeChanged := o.HasMeta && n.HasMeta && o.Mode != n.Mode
	if modeChanged {
		notes = append(notes, fmt.Sprintf("mode %04o → %04o", o.Mode, n.Mode))
	}
	switch {
	case o.Type != n.Type && (o.Type == TypeUnknown || n.Type == TypeUnknown):
		c.Status = StatusUnknown
		notes = append([]string{fmt.Sprintf("type %s → %s", typeWord(o), typeWord(n)), becameWord(o, n)}, notes...)
	case o.Type != n.Type:
		c.Status = StatusType
		notes = []string{word(o.Type) + " → " + word(n.Type)}
	case o.Type == TypeFile:
		switch {
		case o.SHA256 != "" && n.SHA256 != "":
			if o.SHA256 != n.SHA256 {
				c.Status = StatusModified
				notes = append([]string{sizeHashNote(o, n)}, notes...)
				if o.State == StateUnstable || n.State == StateUnstable {
					notes = append(notes, "state "+stateWord(o)+" → "+stateWord(n))
				}
			}
		case o.SHA256 != "" || n.SHA256 != "":
			c.Status = StatusUnknown
			notes = append([]string{becameWord(o, n)}, notes...)
		case o.State != n.State || o.Reason != n.Reason || o.HasMeta != n.HasMeta ||
			(o.HasMeta && (o.Size != n.Size || o.Mtime != n.Mtime || o.Ctime != n.Ctime)):
			c.Status = StatusUnknown
			notes = append([]string{"content not captured: " + becameWord(o, n)}, notes...)
			if o.HasMeta && n.HasMeta && o.Size != n.Size {
				notes = append(notes, fmt.Sprintf("size %d → %d", o.Size, n.Size))
			}
		}
	case o.Type == TypeLink:
		switch {
		case !o.HasTarget || !n.HasTarget:
			c.Status = StatusUnknown
			w := "link target unreadable in old"
			if !n.HasTarget {
				w = "link target unreadable in new"
				if o.HasTarget {
					w = "link target became unreadable"
				}
			}
			notes = append([]string{w}, notes...)
		case !bytes.Equal(o.Target, n.Target):
			c.Status = StatusLink
			notes = append([]string{Q(o.Target) + " → " + Q(n.Target)}, notes...)
		}
	case o.Type == TypeDir:
		oh := hidingState(o.State) || o.Listing != ""
		nh := hidingState(n.State) || n.Listing != ""
		if nh && (!oh || o.State != n.State || o.Listing != n.Listing) {
			c.Status = StatusUnknown
			notes = append([]string{"listing not captured: " + becameWord(o, n)}, notes...)
		}
	}
	if c.Status == 0 && modeChanged {
		c.Status = StatusMode
	}
	if c.Status == 0 && meta && o.HasMeta && n.HasMeta && o.Mtime != n.Mtime {
		c.Status = StatusMeta
		notes = append(notes, "mtime "+fmtTime(o.Mtime)+" → "+fmtTime(n.Mtime))
	}
	if c.Status == 0 {
		return nil
	}
	c.Note = strings.Join(notes, ", ")
	return c
}

func fmtTime(ns int64) string {
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

// shortHash renders the first 12 hex digits of a digest the reader
// validated; anything else is quoted.
func shortHash(sum string) string {
	if sum == "" {
		return "-"
	}
	if len(sum) > 12 {
		sum = sum[:12]
	}
	return qs(sum) + "…"
}

func sizeHashNote(o, n *Entry) string {
	return fmt.Sprintf("size %d → %d, sha256 %s → %s", o.Size, n.Size, shortHash(o.SHA256), shortHash(n.SHA256))
}

// entrySummary is the note of an A or D line.
func entrySummary(e *Entry) string {
	var parts []string
	parts = append(parts, typeWord(e))
	switch e.Type {
	case TypeFile:
		if e.HasMeta {
			parts = append(parts, fmt.Sprintf("size %d", e.Size))
		}
		if e.SHA256 != "" {
			parts = append(parts, "sha256 "+shortHash(e.SHA256))
		}
	case TypeLink:
		if e.HasTarget {
			parts = append(parts, "→ "+Q(e.Target))
		}
	}
	if e.HasMeta && e.Mode&0o7000 != 0 {
		parts = append(parts, fmt.Sprintf("mode %04o", e.Mode))
	}
	if e.Type != TypeUnknown && (e.State != StateOK || e.Listing != "") {
		parts = append(parts, "state "+stateWord(e))
	}
	return strings.Join(parts, ", ")
}

// outWriter writes whole lines and stops at the output cap with a marker.
type outWriter struct {
	w       io.Writer
	n       int64
	limit   int64 // 0 is unlimited
	wrap    bool  // print in rows (see rows)
	stopped bool
	err     error
}

const outLimitMarker = "output limit reached; narrow with `-- <path>`"

// line writes s and a newline. It returns false once output must stop.
func (o *outWriter) line(s string) bool {
	if o.stopped || o.err != nil {
		return false
	}
	if o.limit > 0 && o.n+int64(len(s))+1 > o.limit-int64(len(outLimitMarker))-1 {
		o.stopped = true
		_, o.err = io.WriteString(o.w, outLimitMarker+"\n")
		return false
	}
	rs := []string{s}
	if o.wrap {
		rs = rows(s)
	}
	for _, row := range rs {
		n, err := io.WriteString(o.w, row+"\n")
		o.n += int64(n)
		if err != nil {
			o.err = err
			return false
		}
	}
	return true
}

func headerLine(label string, h *Header) string {
	s := label + " " + qs(h.ID)
	if h.Complete {
		return s + " (complete)"
	}
	return s + " (INCOMPLETE: " + partialText(h) + ")"
}

func partialText(h *Header) string {
	var parts []string
	if h.Root.State == StateUnreadable {
		parts = append(parts, "the tree root is unreadable")
	}
	if h.Root.Listing != "" {
		parts = append(parts, "the tree root's listing is "+word(h.Root.Listing))
	}
	for _, p := range h.Partial {
		at := "the tree root"
		if p.HasAt {
			at = Q(p.At)
		}
		parts = append(parts, fmt.Sprintf("%s %s at %s (%d)", word(p.State), word(p.Reason), at, p.Count))
	}
	if len(parts) == 0 {
		parts = append(parts, "no reason recorded")
	}
	return strings.Join(parts, "; ")
}

// Diff writes the changes from oldM to newM: a summary first (counts, then
// one line per changed path), then text sections for modified text files.
// Both manifests are read through once before anything is written, so an
// invalid manifest produces an error and no output.
func Diff(w io.Writer, oldM, newM OpenManifest, o DiffOptions) (DiffCounts, error) {
	var lines int64
	counts, err := mergeManifests(oldM, newM, o.Paths, o.Meta, mergeSink{
		change: func(*change) error { lines++; return nil },
	})
	if err != nil {
		return DiffCounts{}, err
	}
	oh, nh, err := headersOf(oldM, newM)
	if err != nil {
		return DiffCounts{}, err
	}

	out := &outWriter{w: w, wrap: true}
	if !o.NoLimit {
		out.limit = DiffOutputLimit
	}
	out.line(headerLine("old", oh))
	out.line(headerLine("new", nh))
	if counts.total() == 0 {
		out.line("no changes")
		return counts, out.err
	}
	out.line("changes: " + counts.String())

	stat := o.Stat || o.Blobs == nil
	if !o.Stat && !o.FullDiff && lines > DiffStatThreshold {
		stat = true
		out.line(fmt.Sprintf("more than %d changed paths: showing the summary only (--full-diff shows text sections)", DiffStatThreshold))
	}

	var blobErrs int
	var absent int64
	_, err = mergeManifests(oldM, newM, o.Paths, o.Meta, mergeSink{
		change: func(c *change) error {
			if c.Absent {
				absent++
				if absent > absentUnknownCap {
					return nil
				}
			}
			kind := ""
			if c.Status == StatusModified && !stat && !isSecret(c) {
				k, err := textKind(o.Blobs, c.Old, c.New)
				if err != nil {
					blobErrs++
					k = "content unavailable: " + safeText(err.Error())
				}
				if k != "text" {
					kind = k
				}
			}
			if !out.line(changeLine(c, kind)) {
				return errStopOutput
			}
			return nil
		},
	})
	if err == nil && absent > absentUnknownCap {
		out.line(fmt.Sprintf("? … %d more names not seen on one side, under directories not fully listed", absent-absentUnknownCap))
	}
	if err != nil && !errors.Is(err, errStopOutput) {
		return counts, err
	}
	if out.stopped || out.err != nil || stat {
		return counts, finishErr(out.err, blobErrs)
	}

	_, err = mergeManifests(oldM, newM, o.Paths, o.Meta, mergeSink{
		change: func(c *change) error {
			if c.Status != StatusModified || isSecret(c) {
				return nil
			}
			oldData, newData, ok, err := textPair(o.Blobs, c.Old, c.New)
			if err != nil || !ok {
				return nil
			}
			if !writeSection(out, c.Path, oldData, newData) {
				return errStopOutput
			}
			return nil
		},
	})
	if err != nil && !errors.Is(err, errStopOutput) {
		return counts, err
	}
	return counts, finishErr(out.err, blobErrs)
}

var errStopOutput = errors.New("output stopped")

func finishErr(err error, blobErrs int) error {
	if err != nil {
		return err
	}
	if blobErrs > 0 {
		return fmt.Errorf("%d modified files have missing or damaged blobs; run lever tree verify", blobErrs)
	}
	return nil
}

// changeLine renders "<status> <Q(path)>  [kind ](<note>)". kind is
// "binary" or a lever message; "secret" is added for the ticket.
func changeLine(c *change, kind string) string {
	note := c.Note
	if c.Below > 0 {
		note = joinNote(note, fmt.Sprintf("%d entries below", c.Below))
	}
	if c.Status == StatusModified && isSecret(c) {
		kind = "secret"
	}
	s := string(c.Status) + " " + Q(c.Path)
	switch {
	case kind != "" && note != "":
		s += "  " + kind + " (" + note + ")"
	case kind != "":
		s += "  " + kind
	case note != "":
		s += "  (" + note + ")"
	}
	return s
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + ", " + b
}

func isSecret(c *change) bool {
	return (c.Old != nil && c.Old.State == StateSecret) || (c.New != nil && c.New.State == StateSecret)
}

func headersOf(oldM, newM OpenManifest) (*Header, *Header, error) {
	oh, err := headerOf(oldM)
	if err != nil {
		return nil, nil, err
	}
	nh, err := headerOf(newM)
	if err != nil {
		return nil, nil, err
	}
	return oh, nh, nil
}

func headerOf(m OpenManifest) (*Header, error) {
	r, err := m()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	h := *r.Header()
	return &h, nil
}

// readBlob reads a blob of a known digest and size, at most diffTextMax
// bytes, and verifies both.
func readBlob(src BlobSource, sum string, size int64) ([]byte, error) {
	rc, n, err := src.OpenBlob(sum)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", shortHash(sum), err)
	}
	defer rc.Close()
	if n != size {
		return nil, fmt.Errorf("blob %s has size %d, want %d", shortHash(sum), n, size)
	}
	data, err := io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", shortHash(sum), err)
	}
	got := sha256.Sum256(data)
	if int64(len(data)) != size || hex.EncodeToString(got[:]) != sum {
		return nil, fmt.Errorf("blob %s does not match its digest", shortHash(sum))
	}
	return data, nil
}

func isText(b []byte) bool {
	return bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}

// textPair returns both sides' content when the change is a text diff:
// valid UTF-8, no NUL, at most diffTextMax bytes on both sides.
func textPair(src BlobSource, o, n *Entry) (old, new []byte, ok bool, err error) {
	if src == nil || o == nil || n == nil || o.Size > diffTextMax || n.Size > diffTextMax {
		return nil, nil, false, nil
	}
	if old, err = readBlob(src, o.SHA256, o.Size); err != nil {
		return nil, nil, false, err
	}
	if !isText(old) {
		return nil, nil, false, nil
	}
	if new, err = readBlob(src, n.SHA256, n.Size); err != nil {
		return nil, nil, false, err
	}
	if !isText(new) {
		return nil, nil, false, nil
	}
	return old, new, true, nil
}

func textKind(src BlobSource, o, n *Entry) (string, error) {
	_, _, ok, err := textPair(src, o, n)
	if err != nil {
		return "", err
	}
	if ok {
		return "text", nil
	}
	return "binary", nil
}

func writeSection(out *outWriter, p, old, new []byte) bool {
	if !out.line("== M " + Q(p)) {
		return false
	}
	for _, h := range lineDiff(old, new) {
		if !out.line(fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount)) {
			return false
		}
		for _, l := range h.Lines {
			text, eol := strings.CutSuffix(l.Text, "\n")
			if !out.line(contentLine(l.Op, []byte(text))) {
				return false
			}
			// Starts with '\', which no content line does.
			if !eol && !out.line(`\ no newline at end of file`) {
				return false
			}
		}
	}
	return true
}
