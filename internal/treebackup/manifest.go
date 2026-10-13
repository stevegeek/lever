package treebackup

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"
)

// idRE is a snapshot id: the UTC start time, with a counter for two in one
// second. It is safe as a file name and on a terminal.
var idRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[0-9]{1,3})?$`)

// ValidID reports whether id has the shape of a snapshot id.
func ValidID(id string) bool { return idRE.MatchString(id) }

const trailerPrefix = `{"sha256":"`

// appendJSONString appends b, which must be valid UTF-8, as a JSON string.
// Only '"', '\\', C0 controls and U+2028/U+2029 are escaped, so the encoded
// length is predictable and equal byte strings always encode the same.
func appendJSONString(buf, b []byte) []byte {
	const hexdig = "0123456789abcdef"
	buf = append(buf, '"')
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '"' || c == '\\':
			buf = append(buf, '\\', c)
			i++
		case c < 0x20:
			buf = append(buf, '\\', 'u', '0', '0', hexdig[c>>4], hexdig[c&0xf])
			i++
		case c == 0xe2 && i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xa8 || b[i+2] == 0xa9):
			buf = append(buf, `\u202`...)
			buf = append(buf, hexdig[b[i+2]&0xf])
			i += 3
		default:
			buf = append(buf, c)
			i++
		}
	}
	return append(buf, '"')
}

// appendBytesField writes b under key when it is valid UTF-8 and under
// key_b64 otherwise: a decoder must never have to replace invalid bytes,
// which would merge distinct names.
func appendBytesField(buf []byte, key string, b []byte) []byte {
	if utf8.Valid(b) {
		buf = append(buf, '"')
		buf = append(buf, key...)
		buf = append(buf, `":`...)
		return appendJSONString(buf, b)
	}
	buf = append(buf, '"')
	buf = append(buf, key...)
	buf = append(buf, `_b64":"`...)
	buf = base64.StdEncoding.AppendEncode(buf, b)
	return append(buf, '"')
}

func appendKey(buf []byte, comma bool, key string) []byte {
	if comma {
		buf = append(buf, ',')
	}
	buf = append(buf, '"')
	buf = append(buf, key...)
	return append(buf, `":`...)
}

func appendUintField(buf []byte, key string, v uint64) []byte {
	return strconv.AppendUint(appendKey(buf, true, key), v, 10)
}

func appendIntField(buf []byte, key string, v int64) []byte {
	return strconv.AppendInt(appendKey(buf, true, key), v, 10)
}

func appendStrField(buf []byte, key, v string) []byte {
	return appendJSONString(appendKey(buf, true, key), []byte(v))
}

func appendBoolField(buf []byte, key string, v bool) []byte {
	return strconv.AppendBool(appendKey(buf, true, key), v)
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// EncodeHeader appends the header line, newline included, to buf.
func EncodeHeader(buf []byte, h *Header) []byte {
	buf = append(buf, `{"version":`...)
	buf = strconv.AppendInt(buf, int64(h.Version), 10)
	buf = appendStrField(buf, "id", h.ID)
	buf = appendUintField(buf, "seq", h.Seq)
	buf = appendStrField(buf, "created", formatTime(h.Created))
	buf = appendStrField(buf, "finished", formatTime(h.Finished))
	buf = appendStrField(buf, "trigger", h.Trigger)
	buf = appendStrField(buf, "quiescent", h.Quiescent)
	buf = appendBoolField(buf, "pinned", h.Pinned)
	buf = appendStrField(buf, "lever_version", h.LeverVersion)
	buf = append(buf, ',')
	buf = appendBytesField(buf, "tree", h.Tree)
	buf = appendUintField(buf, "tree_dev", h.TreeDev)
	buf = appendUintField(buf, "tree_ino", h.TreeIno)

	buf = appendKey(buf, true, "root")
	buf = append(buf, '{')
	comma := false
	if h.Root.HasMeta {
		buf = strconv.AppendUint(appendKey(buf, false, "mode"), uint64(h.Root.Mode), 10)
		buf = appendIntField(buf, "mtime", h.Root.Mtime)
		comma = true
	}
	if h.Root.State != StateOK {
		buf = appendJSONString(appendKey(buf, comma, "state"), []byte(h.Root.State))
		comma = true
	}
	if h.Root.Reason != "" {
		buf = appendJSONString(appendKey(buf, comma, "reason"), []byte(h.Root.Reason))
		comma = true
	}
	if h.Root.Listing != "" {
		buf = appendJSONString(appendKey(buf, comma, "listing"), []byte(h.Root.Listing))
	}
	buf = append(buf, '}')

	buf = appendNullableID(buf, "cache_base", h.CacheBase)
	buf = appendNullableID(buf, "known_base", h.KnownBase)

	l := h.Limits
	buf = appendKey(buf, true, "limits")
	buf = append(buf, `{"file":`...)
	buf = strconv.AppendInt(buf, l.File, 10)
	buf = appendIntField(buf, "read", l.Read)
	buf = appendIntField(buf, "new", l.New)
	buf = appendIntField(buf, "entries", l.Entries)
	buf = appendIntField(buf, "manifest", l.Manifest)
	buf = appendIntField(buf, "dir_entries", l.DirEntries)
	buf = appendIntField(buf, "depth", l.Depth)
	buf = appendIntField(buf, "path", l.Path)
	buf = appendIntField(buf, "time", int64(l.Time))
	buf = append(buf, '}')

	buf = appendBoolField(buf, "complete", h.Complete)

	buf = appendKey(buf, true, "partial")
	buf = append(buf, '[')
	for i, p := range h.Partial {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, `{"state":`...)
		buf = appendJSONString(buf, []byte(p.State))
		buf = appendStrField(buf, "reason", string(p.Reason))
		if p.HasAt {
			buf = append(buf, ',')
			buf = appendBytesField(buf, "at", p.At)
		}
		buf = appendIntField(buf, "count", p.Count)
		buf = append(buf, '}')
	}
	buf = append(buf, ']')

	buf = appendKey(buf, true, "flood")
	buf = append(buf, '[')
	for i, f := range h.Flood {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '{')
		if f.HasAt {
			buf = appendBytesField(buf, "at", f.At)
			buf = append(buf, ',')
		}
		buf = append(buf, `"count":`...)
		buf = strconv.AppendInt(buf, f.Count, 10)
		buf = append(buf, '}')
	}
	buf = append(buf, ']')

	buf = appendKey(buf, true, "counts")
	buf = append(buf, `{"by_type":{`...)
	for i, t := range AllTypes {
		buf = strconv.AppendInt(appendKey(buf, i > 0, string(t)), h.Counts.ByType[t], 10)
	}
	buf = append(buf, `},"by_state":{`...)
	for i, s := range AllStates {
		buf = strconv.AppendInt(appendKey(buf, i > 0, string(s)), h.Counts.ByState[s], 10)
	}
	buf = append(buf, '}')
	buf = appendIntField(buf, "unreadable_new", h.Counts.UnreadableNew)
	buf = appendIntField(buf, "badname", h.Counts.Badname)
	buf = append(buf, '}')

	buf = appendKey(buf, true, "bytes")
	buf = append(buf, `{"total":`...)
	buf = strconv.AppendInt(buf, h.Bytes.Total, 10)
	buf = appendIntField(buf, "read", h.Bytes.Read)
	buf = appendIntField(buf, "new", h.Bytes.New)
	buf = append(buf, '}')

	buf = appendKey(buf, true, "blobs")
	buf = append(buf, `{"new":`...)
	buf = strconv.AppendInt(buf, h.Blobs.New, 10)
	buf = appendIntField(buf, "repaired", h.Blobs.Repaired)
	return append(buf, "}}\n"...)
}

func appendNullableID(buf []byte, key, id string) []byte {
	buf = appendKey(buf, true, key)
	if id == "" {
		return append(buf, "null"...)
	}
	return appendJSONString(buf, []byte(id))
}

// EncodeEntry appends one entry line, newline included, to buf. The length
// it adds is what the manifest budget charges for the entry.
func EncodeEntry(buf []byte, e *Entry) []byte {
	buf = append(buf, '{')
	buf = appendBytesField(buf, "path", e.Path)
	buf = appendStrField(buf, "type", string(e.Type))
	if e.HasMeta {
		buf = appendUintField(buf, "mode", uint64(e.Mode))
		buf = appendIntField(buf, "size", e.Size)
		buf = appendIntField(buf, "mtime", e.Mtime)
		buf = appendIntField(buf, "ctime", e.Ctime)
		buf = appendUintField(buf, "dev", e.Dev)
		buf = appendUintField(buf, "ino", e.Ino)
		buf = appendUintField(buf, "nlink", e.Nlink)
	}
	if e.SHA256 != "" {
		buf = appendStrField(buf, "sha256", e.SHA256)
		buf = appendIntField(buf, "verified", e.Verified)
	}
	if e.HasTarget {
		buf = append(buf, ',')
		buf = appendBytesField(buf, "target", e.Target)
	}
	if e.State != StateOK {
		buf = appendStrField(buf, "state", string(e.State))
	}
	if e.Reason != "" {
		buf = appendStrField(buf, "reason", string(e.Reason))
	}
	if e.Listing != "" {
		buf = appendStrField(buf, "listing", e.Listing)
	}
	return append(buf, "}\n"...)
}

func clearsComplete(s State) bool { return s == StateTruncated || s == StateSkipped }

// summary accumulates what Summarize derives, so the writer and the
// validator compute it the same way.
type summary struct {
	byType   map[EntryType]int64
	byState  map[State]int64
	partial  []Partial
	complete bool
}

func newSummary(root *RootInfo) *summary {
	s := &summary{
		byType:   make(map[EntryType]int64, len(AllTypes)),
		byState:  make(map[State]int64, len(AllStates)),
		complete: root.State != StateUnreadable && root.State != StateTruncated && root.Listing == "",
	}
	for _, t := range AllTypes {
		s.byType[t] = 0
	}
	for _, st := range AllStates {
		s.byState[st] = 0
	}
	if root.State == StateTruncated {
		s.partial = append(s.partial, Partial{State: StateTruncated, Reason: root.Reason, Count: 1})
	}
	return s
}

func (s *summary) add(e *Entry) {
	s.byType[e.Type]++
	if e.State != StateOK {
		s.byState[e.State]++
	}
	if !clearsComplete(e.State) {
		return
	}
	s.complete = false
	for i := range s.partial {
		if s.partial[i].State == e.State && s.partial[i].Reason == e.Reason {
			s.partial[i].Count++
			return
		}
	}
	s.partial = append(s.partial, Partial{State: e.State, Reason: e.Reason, At: bytes.Clone(e.Path), HasAt: true, Count: 1})
}

// Summarize sets h.Counts.ByType, h.Counts.ByState, h.Partial and
// h.Complete from h.Root and entries, which must be in manifest order. The
// other counts are the walker's to set.
func Summarize(h *Header, entries []Entry) {
	s := newSummary(&h.Root)
	for i := range entries {
		s.add(&entries[i])
	}
	h.Counts.ByType = s.byType
	h.Counts.ByState = s.byState
	h.Partial = s.partial
	h.Complete = s.complete
}

// WriteManifest writes the header, the entries in the order given and the
// trailer.
func WriteManifest(w io.Writer, h *Header, entries []Entry) error {
	sum := sha256.New()
	mw := io.MultiWriter(w, sum)
	buf := EncodeHeader(make([]byte, 0, 4096), h)
	if _, err := mw.Write(buf); err != nil {
		return err
	}
	for i := range entries {
		buf = EncodeEntry(buf[:0], &entries[i])
		if _, err := mw.Write(buf); err != nil {
			return err
		}
	}
	buf = append(buf[:0], trailerPrefix...)
	buf = hex.AppendEncode(buf, sum.Sum(nil))
	buf = append(buf, "\"}\n"...)
	_, err := w.Write(buf)
	return err
}

// manifestCap is the most bytes a manifest with these limits may hold.
func manifestCap(l *Limits) int64 {
	c := int64(MaxManifestCap)
	if l.Manifest < c-ManifestSlack {
		c = l.Manifest + ManifestSlack
	}
	return c
}

// reader is the streaming, validating EntryReader.
type reader struct {
	br      *bufio.Reader
	closer  io.Closer
	id      string
	h       *Header
	line    int
	read    int64
	limit   int64
	lbuf    []byte
	sum     hash.Hash
	sm      *summary
	prev    []byte
	stack   [][]byte
	sealed  []bool // the walker never enters a mount or an excluded dir
	maxPath int
	err     error
	done    bool
}

// NewReader reads and validates the header of the manifest in r, whose
// file is called wantID+".jsonl" and is size bytes long. The entries are
// validated as Next returns them, and the whole manifest only when Next
// returns io.EOF. Close closes r when it is an io.Closer.
func NewReader(r io.Reader, size int64, wantID string) (EntryReader, error) {
	rd := &reader{
		br:  bufio.NewReaderSize(r, 64<<10),
		id:  wantID,
		sum: sha256.New(),
	}
	if c, ok := r.(io.Closer); ok {
		rd.closer = c
	}
	if err := rd.open(size); err != nil {
		return nil, err
	}
	return rd, nil
}

func (r *reader) invalid(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if r.line > 0 {
		return fmt.Errorf("%w %s: line %d: %s", ErrInvalid, r.id, r.line, msg)
	}
	return fmt.Errorf("%w %s: %s", ErrInvalid, r.id, msg)
}

// maxHeaderBytes caps the header line. It is above MaxLineBytes because the
// header can quote up to 14 agent-chosen paths (partial and flood), each of
// which may grow sixfold when escaped; a header the walker produces must
// never fail the self-check.
const maxHeaderBytes = 512 << 10

// readLine returns the next line with its newline, at most max bytes. The
// slice is valid until the next call.
func (r *reader) readLine(max int) ([]byte, error) {
	r.line++
	r.lbuf = r.lbuf[:0]
	for {
		chunk, err := r.br.ReadSlice('\n')
		if len(r.lbuf)+len(chunk) > max {
			return nil, r.invalid("line longer than %d bytes", max)
		}
		switch {
		case err == nil:
			line := chunk
			if len(r.lbuf) > 0 {
				r.lbuf = append(r.lbuf, chunk...)
				line = r.lbuf
			}
			r.read += int64(len(line))
			if r.read > r.limit {
				return nil, r.invalid("manifest larger than %d bytes", r.limit)
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			r.lbuf = append(r.lbuf, chunk...)
		case errors.Is(err, io.EOF):
			if len(r.lbuf)+len(chunk) == 0 {
				return nil, io.EOF
			}
			return nil, r.invalid("last line has no newline")
		default:
			return nil, err
		}
	}
}

func (r *reader) open(size int64) error {
	if !ValidID(r.id) {
		return r.invalid("not a snapshot id")
	}
	if size > MaxManifestCap {
		return r.invalid("manifest larger than %d bytes", int64(MaxManifestCap))
	}
	r.limit = MaxManifestCap
	line, err := r.readLine(maxHeaderBytes)
	if errors.Is(err, io.EOF) {
		return r.invalid("empty manifest")
	}
	if err != nil {
		return err
	}
	r.sum.Write(line)
	h, err := decodeHeader(line[:len(line)-1])
	if err != nil {
		return r.invalid("header: %v", err)
	}
	if h.ID != r.id {
		return r.invalid("header id %q does not match the file name", h.ID)
	}
	r.limit = manifestCap(&h.Limits)
	if size > r.limit || r.read > r.limit {
		return r.invalid("manifest larger than %d bytes", r.limit)
	}
	r.maxPath = int(min(h.Limits.Path, MaxPathCap))
	r.h = h
	r.sm = newSummary(&h.Root)
	return nil
}

func (r *reader) Header() *Header { return r.h }

func (r *reader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

func (r *reader) Next() (*Entry, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.done {
		return nil, io.EOF
	}
	e, err := r.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.done = true
		} else {
			r.err = err
		}
		return nil, err
	}
	return e, nil
}

func (r *reader) next() (*Entry, error) {
	line, err := r.readLine(MaxLineBytes)
	if errors.Is(err, io.EOF) {
		return nil, r.invalid("no trailer")
	}
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(line, []byte(trailerPrefix)) {
		return nil, r.finish(line)
	}
	r.sum.Write(line)
	e, err := decodeEntry(line[:len(line)-1])
	if err != nil {
		return nil, r.invalid("%v", err)
	}
	if err := r.check(e); err != nil {
		return nil, r.invalid("%s: %v", Q(e.Path), err)
	}
	r.sm.add(e)
	return e, nil
}

func (r *reader) check(e *Entry) error {
	if err := checkPath(e.Path, r.maxPath); err != nil {
		return err
	}
	if r.h.Root.State == StateUnreadable {
		return errors.New("entry under an unreadable root")
	}
	if r.prev != nil && ComparePaths(r.prev, e.Path) >= 0 {
		return errors.New("out of order or duplicate")
	}
	r.prev = append(r.prev[:0], e.Path...)
	parent := ParentOf(e.Path)
	for len(r.stack) > 0 && !IsUnder(e.Path, r.stack[len(r.stack)-1]) {
		r.stack = r.stack[:len(r.stack)-1]
		r.sealed = r.sealed[:len(r.sealed)-1]
	}
	var top []byte
	if len(r.stack) > 0 {
		top = r.stack[len(r.stack)-1]
	}
	if !bytes.Equal(top, parent) {
		return errors.New("parent is not a directory entry")
	}
	if len(r.stack) > 0 && r.sealed[len(r.sealed)-1] {
		return errors.New("entry under a directory that was never opened")
	}
	if err := checkEntryFields(e); err != nil {
		return err
	}
	if e.Type == TypeDir {
		r.stack = append(r.stack, bytes.Clone(e.Path))
		r.sealed = append(r.sealed, e.State == StateMount || e.State == StateExcluded)
	}
	return nil
}

func (r *reader) finish(line []byte) error {
	want := append([]byte(trailerPrefix), hex.AppendEncode(nil, r.sum.Sum(nil))...)
	want = append(want, "\"}\n"...)
	if !bytes.Equal(line, want) {
		return r.invalid("trailer does not match")
	}
	if _, err := r.br.ReadByte(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return r.invalid("data after the trailer")
	}
	h, s := r.h, r.sm
	for _, t := range AllTypes {
		if h.Counts.ByType[t] != s.byType[t] {
			return r.invalid("header counts %d %s entries, found %d", h.Counts.ByType[t], t, s.byType[t])
		}
	}
	for _, st := range AllStates {
		if h.Counts.ByState[st] != s.byState[st] {
			return r.invalid("header counts %d %s entries, found %d", h.Counts.ByState[st], st, s.byState[st])
		}
	}
	if h.Complete != s.complete {
		return r.invalid("header complete is %v, entries say %v", h.Complete, s.complete)
	}
	if len(h.Partial) != len(s.partial) {
		return r.invalid("header partial does not match the entries")
	}
	for i, p := range h.Partial {
		q := s.partial[i]
		if p.State != q.State || p.Reason != q.Reason || p.HasAt != q.HasAt || !bytes.Equal(p.At, q.At) || p.Count != q.Count {
			return r.invalid("header partial does not match the entries")
		}
	}
	return io.EOF
}

// ValidateAll reads a manifest through once and returns its header when it
// is valid.
func ValidateAll(open OpenManifest) (*Header, error) {
	rd, err := open()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	for {
		_, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return rd.Header(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// checkPath enforces the path rules on a relative tree path.
func checkPath(p []byte, maxLen int) error {
	if len(p) == 0 {
		return errors.New("empty path")
	}
	if len(p) > maxLen {
		return fmt.Errorf("path longer than %d bytes", maxLen)
	}
	if bytes.IndexByte(p, 0) >= 0 {
		return errors.New("NUL in path")
	}
	for c := range bytes.SplitSeq(p, []byte{'/'}) {
		switch {
		case len(c) == 0:
			return errors.New("empty path component")
		case string(c) == "." || string(c) == "..":
			return errors.New("dot component in path")
		case len(c) > MaxComponentCap:
			return fmt.Errorf("path component longer than %d bytes", MaxComponentCap)
		}
	}
	return nil
}

func validType(t EntryType) bool {
	for _, x := range AllTypes {
		if t == x {
			return true
		}
	}
	return false
}

func validState(s State) bool {
	for _, x := range AllStates {
		if s == x {
			return true
		}
	}
	return false
}

func reasonAllowed(s State, r Reason) bool {
	var set []Reason
	switch s {
	case StateSkipped:
		set = SkippedReasons
	case StateTruncated:
		set = TruncatedReasons
	}
	for _, x := range set {
		if r == x {
			return true
		}
	}
	return false
}

// stateTypes limits states that only one entry type can have.
var stateTypes = map[State]EntryType{
	StateUnstable:        TypeFile,
	StateTooBig:          TypeFile,
	StateSkipped:         TypeFile,
	StateSecret:          TypeFile,
	StateListingUnstable: TypeDir,
	StateTruncated:       TypeDir,
}

func checkEntryFields(e *Entry) error {
	if !validType(e.Type) {
		return fmt.Errorf("unknown type %q", e.Type)
	}
	if e.State != StateOK && !validState(e.State) {
		return fmt.Errorf("unknown state %q", e.State)
	}
	if t, ok := stateTypes[e.State]; ok && e.Type != t {
		return fmt.Errorf("state %s on a %s", e.State, e.Type)
	}
	if e.Type == TypeFile && e.State == StateOK && (!e.HasMeta || e.SHA256 == "") {
		return errors.New("file with no state and no content")
	}
	if e.State == StateSecret && e.SHA256 == "" {
		return errors.New("secret without sha256")
	}
	if e.Type == TypeUnknown && e.State != StateUnreadable {
		return errors.New("type unknown without state unreadable")
	}
	if clearsComplete(e.State) {
		if !reasonAllowed(e.State, e.Reason) {
			return fmt.Errorf("state %s with reason %q", e.State, e.Reason)
		}
	} else if e.Reason != "" {
		return fmt.Errorf("reason %q without state skipped or truncated", e.Reason)
	}
	if e.HasMeta && e.Mode&^0o7777 != 0 {
		return errors.New("mode has more than 12 bits")
	}
	if e.HasMeta && e.Size < 0 {
		return errors.New("negative size")
	}
	if e.SHA256 != "" {
		if e.Type != TypeFile {
			return errors.New("sha256 on a non-file")
		}
		if !validSum(e.SHA256) {
			return errors.New("bad sha256")
		}
		if e.State != StateOK && e.State != StateUnstable && e.State != StateSecret {
			return fmt.Errorf("sha256 with state %s", e.State)
		}
		if e.Verified < 0 {
			return errors.New("negative verified time")
		}
	}
	if e.HasTarget {
		if e.Type != TypeLink {
			return errors.New("target on a non-link")
		}
		if len(e.Target) > MaxTargetCap {
			return fmt.Errorf("link target longer than %d bytes", MaxTargetCap)
		}
		if bytes.IndexByte(e.Target, 0) >= 0 {
			return errors.New("NUL in link target")
		}
	} else if e.Type == TypeLink && e.State == StateOK {
		return errors.New("link without a target or a state")
	}
	if e.Listing != "" {
		if e.Listing != ListingUnreadable {
			return fmt.Errorf("unknown listing %q", e.Listing)
		}
		if e.Type != TypeDir {
			return errors.New("listing on a non-directory")
		}
	}
	return nil
}

// validSum reports whether s is 64 lowercase hex digits.
func validSum(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// scanner is a strict JSON token scanner over one line. It allows no
// insignificant whitespace and only integer numbers, because lever's
// encoder writes none, and it lets each object reject unknown and
// duplicate keys, which encoding/json silently accepts.
type scanner struct {
	b []byte
	i int
}

func (s *scanner) errf(format string, args ...any) error {
	return fmt.Errorf("offset %d: %s", s.i, fmt.Sprintf(format, args...))
}

func (s *scanner) consume(c byte) bool {
	if s.i < len(s.b) && s.b[s.i] == c {
		s.i++
		return true
	}
	return false
}

func (s *scanner) expect(c byte) error {
	if !s.consume(c) {
		return s.errf("want %q", c)
	}
	return nil
}

func (s *scanner) literal(lit string) bool {
	if bytes.HasPrefix(s.b[s.i:], []byte(lit)) {
		s.i += len(lit)
		return true
	}
	return false
}

func (s *scanner) end() error {
	if s.i != len(s.b) {
		return s.errf("trailing data")
	}
	return nil
}

// str decodes a JSON string. The result is always valid UTF-8: invalid raw
// bytes and lone surrogates are rejected, never replaced.
func (s *scanner) str() ([]byte, error) {
	if err := s.expect('"'); err != nil {
		return nil, err
	}
	var out []byte
	for {
		if s.i >= len(s.b) {
			return nil, s.errf("unterminated string")
		}
		c := s.b[s.i]
		switch {
		case c == '"':
			s.i++
			if !utf8.Valid(out) {
				return nil, s.errf("string is not valid UTF-8")
			}
			if out == nil {
				out = []byte{}
			}
			return out, nil
		case c < 0x20:
			return nil, s.errf("control byte in string")
		case c != '\\':
			out = append(out, c)
			s.i++
			continue
		}
		s.i++
		if s.i >= len(s.b) {
			return nil, s.errf("unterminated escape")
		}
		c = s.b[s.i]
		s.i++
		switch c {
		case '"', '\\', '/':
			out = append(out, c)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, err := s.hex4()
			if err != nil {
				return nil, err
			}
			if r >= 0xd800 && r < 0xdc00 {
				if !s.literal(`\u`) {
					return nil, s.errf("lone surrogate")
				}
				lo, err := s.hex4()
				if err != nil {
					return nil, err
				}
				if lo < 0xdc00 || lo >= 0xe000 {
					return nil, s.errf("lone surrogate")
				}
				r = 0x10000 + (r-0xd800)<<10 + (lo - 0xdc00)
			} else if r >= 0xdc00 && r < 0xe000 {
				return nil, s.errf("lone surrogate")
			}
			out = utf8.AppendRune(out, r)
		default:
			return nil, s.errf("bad escape")
		}
	}
}

func (s *scanner) hex4() (rune, error) {
	if s.i+4 > len(s.b) {
		return 0, s.errf("short \\u escape")
	}
	v, err := strconv.ParseUint(string(s.b[s.i:s.i+4]), 16, 32)
	if err != nil {
		return 0, s.errf("bad \\u escape")
	}
	s.i += 4
	return rune(v), nil
}

func (s *scanner) digits() ([]byte, error) {
	start := s.i
	for s.i < len(s.b) && s.b[s.i] >= '0' && s.b[s.i] <= '9' {
		s.i++
	}
	d := s.b[start:s.i]
	if len(d) == 0 {
		return nil, s.errf("want a number")
	}
	if len(d) > 1 && d[0] == '0' {
		return nil, s.errf("leading zero")
	}
	return d, nil
}

func (s *scanner) uint() (uint64, error) {
	d, err := s.digits()
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(string(d), 10, 64)
	if err != nil {
		return 0, s.errf("number out of range")
	}
	return v, nil
}

func (s *scanner) int() (int64, error) {
	neg := s.consume('-')
	d, err := s.digits()
	if err != nil {
		return 0, err
	}
	if neg && string(d) == "0" {
		return 0, s.errf("negative zero")
	}
	str := string(d)
	if neg {
		str = "-" + str
	}
	v, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return 0, s.errf("number out of range")
	}
	return v, nil
}

func (s *scanner) nonneg() (int64, error) {
	v, err := s.int()
	if err == nil && v < 0 {
		err = s.errf("negative number")
	}
	return v, err
}

func (s *scanner) boolean() (bool, error) {
	switch {
	case s.literal("true"):
		return true, nil
	case s.literal("false"):
		return false, nil
	}
	return false, s.errf("want a boolean")
}

func (s *scanner) text() (string, error) {
	b, err := s.str()
	return string(b), err
}

// object scans an object whose keys must all be in keys, each at most once,
// calling field for each.
func (s *scanner) object(keys []string, field func(k int) error) (seen uint64, err error) {
	if err := s.expect('{'); err != nil {
		return 0, err
	}
	if s.consume('}') {
		return 0, nil
	}
	for {
		kb, err := s.str()
		if err != nil {
			return 0, err
		}
		k := -1
		for i, name := range keys {
			if string(kb) == name {
				k = i
				break
			}
		}
		if k < 0 {
			return 0, s.errf("unknown field %q", kb)
		}
		if seen&(1<<k) != 0 {
			return 0, s.errf("duplicate field %q", kb)
		}
		seen |= 1 << k
		if err := s.expect(':'); err != nil {
			return 0, err
		}
		if err := field(k); err != nil {
			return 0, err
		}
		if s.consume(',') {
			continue
		}
		return seen, s.expect('}')
	}
}

func (s *scanner) array(elem func() error) error {
	if err := s.expect('['); err != nil {
		return err
	}
	if s.consume(']') {
		return nil
	}
	for {
		if err := elem(); err != nil {
			return err
		}
		if s.consume(',') {
			continue
		}
		return s.expect(']')
	}
}

// b64 decodes a *_b64 value. It must be canonical standard base64 of bytes
// that are not valid UTF-8, so every byte string has exactly one spelling.
func (s *scanner) b64() ([]byte, error) {
	enc, err := s.str()
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(string(enc))
	if err != nil || base64.StdEncoding.EncodeToString(b) != string(enc) {
		return nil, s.errf("bad base64")
	}
	if utf8.Valid(b) {
		return nil, s.errf("base64 of valid UTF-8 bytes")
	}
	return b, nil
}

func has(seen uint64, k int) bool { return seen&(1<<k) != 0 }

// requireAll reports the first of keys[:n] missing from seen.
func requireAll(seen uint64, keys []string, n int) error {
	for k := 0; k < n; k++ {
		if !has(seen, k) {
			return fmt.Errorf("missing field %q", keys[k])
		}
	}
	return nil
}

const (
	ePath = iota
	ePathB64
	eType
	eMode
	eSize
	eMtime
	eCtime
	eDev
	eIno
	eNlink
	eSHA256
	eVerified
	eTarget
	eTargetB64
	eState
	eReason
	eListing
)

var entryKeys = []string{"path", "path_b64", "type", "mode", "size", "mtime", "ctime", "dev", "ino", "nlink",
	"sha256", "verified", "target", "target_b64", "state", "reason", "listing"}

const metaKeys = 1<<eMode | 1<<eSize | 1<<eMtime | 1<<eCtime | 1<<eDev | 1<<eIno | 1<<eNlink

func decodeEntry(line []byte) (*Entry, error) {
	s := &scanner{b: line}
	e := &Entry{}
	seen, err := s.object(entryKeys, func(k int) error {
		var err error
		var u uint64
		switch k {
		case ePath:
			e.Path, err = s.str()
		case ePathB64:
			e.Path, err = s.b64()
		case eType:
			var t string
			t, err = s.text()
			e.Type = EntryType(t)
		case eMode:
			u, err = s.uint()
			if err == nil && u > 0o7777 {
				err = s.errf("mode has more than 12 bits")
			}
			e.Mode = uint32(u)
		case eSize:
			e.Size, err = s.nonneg()
		case eMtime:
			e.Mtime, err = s.int()
		case eCtime:
			e.Ctime, err = s.int()
		case eDev:
			e.Dev, err = s.uint()
		case eIno:
			e.Ino, err = s.uint()
		case eNlink:
			e.Nlink, err = s.uint()
		case eSHA256:
			e.SHA256, err = s.text()
			if err == nil && !validSum(e.SHA256) {
				err = s.errf("bad sha256")
			}
		case eVerified:
			e.Verified, err = s.nonneg()
		case eTarget:
			e.Target, err = s.str()
			e.HasTarget = true
		case eTargetB64:
			e.Target, err = s.b64()
			e.HasTarget = true
		case eState:
			var t string
			t, err = s.text()
			e.State = State(t)
			if err == nil && e.State == StateOK {
				err = s.errf("empty state")
			}
		case eReason:
			var t string
			t, err = s.text()
			e.Reason = Reason(t)
			if err == nil && e.Reason == "" {
				err = s.errf("empty reason")
			}
		case eListing:
			e.Listing, err = s.text()
			if err == nil && e.Listing == "" {
				err = s.errf("empty listing")
			}
		}
		return err
	})
	if err == nil {
		err = s.end()
	}
	if err != nil {
		return nil, err
	}
	if has(seen, ePath) == has(seen, ePathB64) {
		return nil, errors.New("want exactly one of path and path_b64")
	}
	if !has(seen, eType) {
		return nil, errors.New("missing field \"type\"")
	}
	switch seen & metaKeys {
	case metaKeys:
		e.HasMeta = true
	case 0:
	default:
		return nil, fmt.Errorf("%s: incomplete metadata", Q(e.Path))
	}
	if has(seen, eSHA256) != has(seen, eVerified) {
		return nil, fmt.Errorf("%s: sha256 and verified must appear together", Q(e.Path))
	}
	if has(seen, eTarget) && has(seen, eTargetB64) {
		return nil, fmt.Errorf("%s: both target and target_b64", Q(e.Path))
	}
	return e, nil
}

const (
	hVersion = iota
	hID
	hSeq
	hCreated
	hFinished
	hTrigger
	hQuiescent
	hPinned
	hLeverVersion
	hTreeDev
	hTreeIno
	hRoot
	hCacheBase
	hKnownBase
	hLimits
	hComplete
	hPartial
	hFlood
	hCounts
	hBytes
	hBlobs
	hTree
	hTreeB64
)

// headerKeys lists the required keys first, then tree and tree_b64.
var headerKeys = []string{"version", "id", "seq", "created", "finished", "trigger", "quiescent", "pinned",
	"lever_version", "tree_dev", "tree_ino", "root", "cache_base", "known_base", "limits", "complete",
	"partial", "flood", "counts", "bytes", "blobs", "tree", "tree_b64"}

var (
	rootKeys    = []string{"mode", "mtime", "state", "reason", "listing"}
	limitKeys   = []string{"file", "read", "entries", "manifest", "dir_entries", "depth", "path", "time", "new"}
	partialKeys = []string{"state", "reason", "count", "at", "at_b64"}
	floodKeys   = []string{"count", "at", "at_b64"}
	countKeys   = []string{"by_type", "by_state", "unreadable_new", "badname"}
	bytesKeys   = []string{"total", "read", "new"}
	blobsKeys   = []string{"new", "repaired"}
	typeKeys    = func() []string {
		k := make([]string, len(AllTypes))
		for i, t := range AllTypes {
			k[i] = string(t)
		}
		return k
	}()
	stateKeys = func() []string {
		k := make([]string, len(AllStates))
		for i, t := range AllStates {
			k[i] = string(t)
		}
		return k
	}()
)

func (s *scanner) timeField() (time.Time, error) {
	v, err := s.text()
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil || formatTime(t) != v {
		return time.Time{}, s.errf("bad time %q", v)
	}
	return t.UTC(), nil
}

func (s *scanner) nullableID() (string, error) {
	if s.literal("null") {
		return "", nil
	}
	v, err := s.text()
	if err == nil && !ValidID(v) {
		err = s.errf("bad snapshot id %q", v)
	}
	return v, err
}

func decodeHeader(line []byte) (*Header, error) {
	s := &scanner{b: line}
	h := &Header{}
	seen, err := s.object(headerKeys, func(k int) error {
		var err error
		switch k {
		case hVersion:
			var v int64
			v, err = s.int()
			h.Version = int(v)
		case hID:
			h.ID, err = s.text()
		case hSeq:
			h.Seq, err = s.uint()
		case hCreated:
			h.Created, err = s.timeField()
		case hFinished:
			h.Finished, err = s.timeField()
		case hTrigger:
			h.Trigger, err = s.text()
		case hQuiescent:
			h.Quiescent, err = s.text()
		case hPinned:
			h.Pinned, err = s.boolean()
		case hLeverVersion:
			h.LeverVersion, err = s.text()
		case hTree:
			h.Tree, err = s.str()
		case hTreeB64:
			h.Tree, err = s.b64()
		case hTreeDev:
			h.TreeDev, err = s.uint()
		case hTreeIno:
			h.TreeIno, err = s.uint()
		case hRoot:
			err = s.root(&h.Root)
		case hCacheBase:
			h.CacheBase, err = s.nullableID()
		case hKnownBase:
			h.KnownBase, err = s.nullableID()
		case hLimits:
			err = s.limits(&h.Limits)
		case hComplete:
			h.Complete, err = s.boolean()
		case hPartial:
			err = s.array(func() error {
				p, err := s.partial()
				h.Partial = append(h.Partial, p)
				return err
			})
		case hFlood:
			err = s.array(func() error {
				f, err := s.flood()
				h.Flood = append(h.Flood, f)
				return err
			})
		case hCounts:
			err = s.counts(&h.Counts)
		case hBytes:
			err = s.byteCounts(&h.Bytes)
		case hBlobs:
			err = s.blobCounts(&h.Blobs)
		}
		return err
	})
	if err == nil {
		err = s.end()
	}
	if err != nil {
		return nil, err
	}
	if err := requireAll(seen, headerKeys, hTree); err != nil {
		return nil, err
	}
	if has(seen, hTree) == has(seen, hTreeB64) {
		return nil, errors.New("want exactly one of tree and tree_b64")
	}
	return h, checkHeader(h)
}

func (s *scanner) root(r *RootInfo) error {
	seen, err := s.object(rootKeys, func(k int) error {
		var err error
		var t string
		switch k {
		case 0:
			var u uint64
			u, err = s.uint()
			if err == nil && u > 0o7777 {
				err = s.errf("mode has more than 12 bits")
			}
			r.Mode = uint32(u)
		case 1:
			r.Mtime, err = s.int()
		case 2:
			t, err = s.text()
			r.State = State(t)
			if err == nil && r.State == StateOK {
				err = s.errf("empty state")
			}
		case 3:
			t, err = s.text()
			r.Reason = Reason(t)
			if err == nil && r.Reason == "" {
				err = s.errf("empty reason")
			}
		case 4:
			r.Listing, err = s.text()
			if err == nil && r.Listing == "" {
				err = s.errf("empty listing")
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	if has(seen, 0) != has(seen, 1) {
		return errors.New("root: mode and mtime must appear together")
	}
	r.HasMeta = has(seen, 0)
	return nil
}

func (s *scanner) limits(l *Limits) error {
	seen, err := s.object(limitKeys, func(k int) error {
		v, err := s.nonneg()
		switch k {
		case 0:
			l.File = v
		case 1:
			l.Read = v
		case 2:
			l.Entries = v
		case 3:
			l.Manifest = v
		case 4:
			l.DirEntries = v
		case 5:
			l.Depth = v
		case 6:
			l.Path = v
		case 7:
			l.Time = time.Duration(v)
		case 8:
			l.New = v
		}
		return err
	})
	if err != nil {
		return err
	}
	return requireAll(seen, limitKeys, len(limitKeys))
}

func (s *scanner) partial() (Partial, error) {
	var p Partial
	seen, err := s.object(partialKeys, func(k int) error {
		var err error
		var t string
		switch k {
		case 0:
			t, err = s.text()
			p.State = State(t)
		case 1:
			t, err = s.text()
			p.Reason = Reason(t)
		case 2:
			p.Count, err = s.nonneg()
		case 3, 4:
			if p.HasAt {
				return s.errf("both at and at_b64")
			}
			p.HasAt = true
			if k == 3 {
				p.At, err = s.str()
			} else {
				p.At, err = s.b64()
			}
		}
		return err
	})
	if err != nil {
		return p, err
	}
	return p, requireAll(seen, partialKeys, 3)
}

func (s *scanner) flood() (Flood, error) {
	var f Flood
	seen, err := s.object(floodKeys, func(k int) error {
		var err error
		switch k {
		case 0:
			f.Count, err = s.nonneg()
		case 1, 2:
			if f.HasAt {
				return s.errf("both at and at_b64")
			}
			f.HasAt = true
			if k == 1 {
				f.At, err = s.str()
			} else {
				f.At, err = s.b64()
			}
		}
		return err
	})
	if err != nil {
		return f, err
	}
	return f, requireAll(seen, floodKeys, 1)
}

func (s *scanner) tally(keys []string, set func(i int, v int64)) error {
	seen, err := s.object(keys, func(k int) error {
		v, err := s.nonneg()
		set(k, v)
		return err
	})
	if err != nil {
		return err
	}
	return requireAll(seen, keys, len(keys))
}

func (s *scanner) counts(c *Counts) error {
	c.ByType = make(map[EntryType]int64, len(AllTypes))
	c.ByState = make(map[State]int64, len(AllStates))
	seen, err := s.object(countKeys, func(k int) error {
		var err error
		switch k {
		case 0:
			err = s.tally(typeKeys, func(i int, v int64) { c.ByType[AllTypes[i]] = v })
		case 1:
			err = s.tally(stateKeys, func(i int, v int64) { c.ByState[AllStates[i]] = v })
		case 2:
			c.UnreadableNew, err = s.nonneg()
		case 3:
			c.Badname, err = s.nonneg()
		}
		return err
	})
	if err != nil {
		return err
	}
	return requireAll(seen, countKeys, len(countKeys))
}

func (s *scanner) byteCounts(b *ByteCounts) error {
	return s.tally(bytesKeys, func(i int, v int64) {
		switch i {
		case 0:
			b.Total = v
		case 1:
			b.Read = v
		case 2:
			b.New = v
		}
	})
}

func (s *scanner) blobCounts(b *BlobCounts) error {
	return s.tally(blobsKeys, func(i int, v int64) {
		if i == 0 {
			b.New = v
		} else {
			b.Repaired = v
		}
	})
}

// checkHeader enforces the header rules that need no entries.
func checkHeader(h *Header) error {
	if h.Version != ManifestVersion {
		return fmt.Errorf("unsupported version %d", h.Version)
	}
	if !ValidID(h.ID) {
		return fmt.Errorf("bad id %q", h.ID)
	}
	if h.Seq < 1 {
		return errors.New("seq must be at least 1")
	}
	switch h.Trigger {
	case TriggerStop, TriggerDestroy, TriggerKeepVM, TriggerManual:
	default:
		return fmt.Errorf("unknown trigger %q", h.Trigger)
	}
	switch h.Quiescent {
	case QuiescentVMOff, QuiescentSuspended, QuiescentNone:
	default:
		return fmt.Errorf("unknown quiescent %q", h.Quiescent)
	}
	l := h.Limits
	if l.File <= 0 || l.Read <= 0 || l.New <= 0 || l.Entries <= 0 || l.Entries > MaxEntriesCap || l.Manifest <= 0 ||
		l.Manifest > MaxManifestCap || l.DirEntries <= 0 || l.Depth <= 0 || l.Path <= 0 || l.Path > MaxPathCap || l.Time < 0 {
		return errors.New("limits out of range")
	}
	r := h.Root
	switch r.State {
	case StateOK, StateUnreadable, StateListingUnstable:
		if r.Reason != "" {
			return fmt.Errorf("root reason %q without state truncated", r.Reason)
		}
	case StateTruncated:
		if !reasonAllowed(StateTruncated, r.Reason) {
			return fmt.Errorf("root truncated with reason %q", r.Reason)
		}
	default:
		return fmt.Errorf("root state %q", r.State)
	}
	if r.Listing != "" && r.Listing != ListingUnreadable {
		return fmt.Errorf("root listing %q", r.Listing)
	}
	maxPath := int(min(l.Path, MaxPathCap))
	for _, p := range h.Partial {
		if !clearsComplete(p.State) || !reasonAllowed(p.State, p.Reason) || p.Count < 1 {
			return errors.New("bad partial element")
		}
		if p.HasAt {
			if err := checkPath(p.At, maxPath); err != nil {
				return fmt.Errorf("partial at %s: %v", Q(p.At), err)
			}
		}
	}
	if len(h.Flood) > 3 {
		return errors.New("more than 3 flood elements")
	}
	for _, f := range h.Flood {
		if f.HasAt {
			if err := checkPath(f.At, maxPath); err != nil {
				return fmt.Errorf("flood at %s: %v", Q(f.At), err)
			}
		}
	}
	return nil
}
