package treebackup

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// LogSnapshot is one retained snapshot, in seq order.
type LogSnapshot struct {
	ID   string
	Open OpenManifest
}

// LogOptions select what Log prints. N > 0 limits the output to the last N
// snapshots (with a path: the last N lines). All lists every changed path
// instead of the first logChangedPaths.
type LogOptions struct {
	N       int
	All     bool
	Path    []byte
	HasPath bool
}

const (
	logChangedPaths = 20
	logNotePaths    = 5
)

// emptyReader is the "before" of the oldest retained snapshot: a fully
// listed, empty tree.
type emptyReader struct{ h Header }

func (e *emptyReader) Header() *Header       { return &e.h }
func (e *emptyReader) Next() (*Entry, error) { return nil, io.EOF }
func (e *emptyReader) Close() error          { return nil }

func emptyManifest() (EntryReader, error) { return &emptyReader{}, nil }

// logState memoises the validating pass of each snapshot, so each
// manifest is validated once and nothing is printed about one that fails.
type logState struct {
	snaps []LogSnapshot
	valid []error
	done  []bool
}

func (s *logState) check(i int) error {
	if !s.done[i] {
		s.valid[i] = validateOnce(s.snaps[i].Open)
		s.done[i] = true
	}
	return s.valid[i]
}

// prevValid returns the newest valid snapshot before i, or -1.
func (s *logState) prevValid(i int) int {
	for j := i - 1; j >= 0; j-- {
		if s.check(j) == nil {
			return j
		}
	}
	return -1
}

func (s *logState) opener(i int) (OpenManifest, string) {
	if i < 0 {
		return emptyManifest, ""
	}
	return s.snaps[i].Open, s.snaps[i].ID
}

// Log writes the history of the retained snapshots: one block per
// snapshot compared with the previous retained one, or with a path, the
// snapshots in which that path (or something below it) changed. A manifest
// that fails validation is named and skipped, and Log then returns an
// error.
func Log(w io.Writer, snaps []LogSnapshot, o LogOptions) error {
	s := &logState{snaps: snaps, valid: make([]error, len(snaps)), done: make([]bool, len(snaps))}
	first := 0
	if !o.HasPath && o.N > 0 && o.N < len(snaps) {
		first = len(snaps) - o.N
	}
	for i := first; i < len(snaps); i++ {
		s.check(i)
	}
	if first > 0 {
		s.prevValid(first)
	}
	var err error
	if o.HasPath {
		err = logPath(w, s, o)
	} else {
		err = logAll(w, s, first, o)
	}
	if err != nil {
		return err
	}
	bad := 0
	for i := first; i < len(snaps); i++ {
		if s.done[i] && s.valid[i] != nil {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d snapshot manifests are invalid; run lever tree verify", bad)
	}
	return nil
}

func invalidLine(id string, err error) string {
	return qs(id) + "  INVALID manifest, skipped: " + safeText(err.Error())
}

type logNotes struct {
	sbits, nlink          int64
	sbitPaths, nlinkPaths []string
}

func (n *logNotes) item(p []byte, o, e *Entry) {
	if e == nil || !e.HasMeta {
		return
	}
	var had uint32
	if o != nil && o.HasMeta {
		had = o.Mode
	}
	if e.Mode&^had&0o6000 != 0 {
		n.sbits++
		if len(n.sbitPaths) < logNotePaths {
			n.sbitPaths = append(n.sbitPaths, fmt.Sprintf("%s (%04o)", Q(p), e.Mode))
		}
	}
	if e.Type == TypeFile && e.Nlink > 1 && (o == nil || o.Type != TypeFile || !o.HasMeta || o.Nlink <= 1) {
		n.nlink++
		if len(n.nlinkPaths) < logNotePaths {
			n.nlinkPaths = append(n.nlinkPaths, Q(p))
		}
	}
}

func noteLine(what string, n int64, paths []string) string {
	s := fmt.Sprintf("    note: %s appeared on %d: %s", what, n, strings.Join(paths, " "))
	if int64(len(paths)) < n {
		s += " …"
	}
	return s
}

func snapshotLine(h *Header) string {
	s := qs(h.ID) + "  " + qs(h.Trigger) + " " + qs(h.Quiescent)
	if h.Pinned {
		s += " pinned"
	}
	if h.Complete {
		return s + "  complete"
	}
	s += "  INCOMPLETE (" + partialText(h)
	if len(h.Flood) > 0 {
		var fl []string
		for _, f := range h.Flood {
			at := "the tree root"
			if f.HasAt {
				at = Q(f.At)
			}
			fl = append(fl, fmt.Sprintf("%s (%d)", at, f.Count))
		}
		s += "; flood " + strings.Join(fl, ", ")
	}
	return s + ")"
}

func logAll(w io.Writer, s *logState, first int, o LogOptions) error {
	out := &outWriter{w: w}
	for i := first; i < len(s.snaps); i++ {
		if err := s.check(i); err != nil {
			out.line(invalidLine(s.snaps[i].ID, err))
			continue
		}
		prevM, prevID := s.opener(s.prevValid(i))
		cur := s.snaps[i].Open
		var notes logNotes
		var changed []*change
		var lines int64
		counts, err := mergeManifests(prevM, cur, nil, false, mergeSink{
			change: func(c *change) error {
				lines++
				if len(changed) < logChangedPaths {
					changed = append(changed, c)
				}
				return nil
			},
			item: notes.item,
		})
		if err != nil {
			return err
		}
		h, err := headerOf(cur)
		if err != nil {
			return err
		}
		line := snapshotLine(h)
		if prevID == "" {
			line += "  oldest retained: " + counts.String()
		} else {
			line += "  vs " + qs(prevID) + ": " + counts.String()
		}
		if h.Counts.UnreadableNew > 0 {
			line += fmt.Sprintf("  unreadable new %d", h.Counts.UnreadableNew)
		}
		out.line(line)
		if notes.sbits > 0 {
			out.line(noteLine("setuid/setgid", notes.sbits, notes.sbitPaths))
		}
		if notes.nlink > 0 {
			out.line(noteLine("nlink > 1", notes.nlink, notes.nlinkPaths))
		}
		if o.All && lines > int64(len(changed)) {
			_, err := mergeManifests(prevM, cur, nil, false, mergeSink{
				change: func(c *change) error {
					if !out.line("    " + changeLine(c, "")) {
						return errStopOutput
					}
					return nil
				},
			})
			if err != nil {
				return err
			}
		} else {
			for _, c := range changed {
				out.line("    " + changeLine(c, ""))
			}
			if lines > int64(len(changed)) {
				out.line(fmt.Sprintf("    … %d more changed paths (--all lists them)", lines-int64(len(changed))))
			}
		}
		if out.err != nil {
			return out.err
		}
	}
	return out.err
}

func logPath(w io.Writer, s *logState, o LogOptions) error {
	var lines []string
	for i := range s.snaps {
		if err := s.check(i); err != nil {
			lines = append(lines, invalidLine(s.snaps[i].ID, err))
			continue
		}
		prevM, _ := s.opener(s.prevValid(i))
		var cur *Entry
		var exact *change
		counts, err := mergeManifests(prevM, s.snaps[i].Open, [][]byte{o.Path}, false, mergeSink{
			change: func(c *change) error {
				if bytes.Equal(c.Path, o.Path) {
					exact = c
				}
				return nil
			},
			item: func(p []byte, _, n *Entry) {
				if bytes.Equal(p, o.Path) {
					cur = n
				}
			},
		})
		if err != nil {
			return err
		}
		below := counts.total()
		if exact != nil {
			below -= 1 + exact.Below
		}
		if exact == nil && below == 0 {
			continue
		}
		status := "-"
		if exact != nil {
			status = string(exact.Status)
		}
		line := qs(s.snaps[i].ID) + "  " + status + "  " + entryBrief(cur)
		if exact != nil && exact.Note != "" {
			line += "  (" + exact.Note + ")"
		}
		if below > 0 {
			line += fmt.Sprintf("  %d changes below", below)
		}
		lines = append(lines, line)
	}
	if o.N > 0 && len(lines) > o.N {
		lines = lines[len(lines)-o.N:]
	}
	out := &outWriter{w: w}
	for _, l := range lines {
		out.line(l)
	}
	return out.err
}

// entryBrief is size, short hash and state of the entry, or "absent".
func entryBrief(e *Entry) string {
	if e == nil {
		return "absent"
	}
	size := "-"
	if e.HasMeta && e.Type == TypeFile {
		size = fmt.Sprint(e.Size)
	}
	state := "ok"
	if e.State != StateOK || e.Listing != "" {
		state = stateWord(e)
	}
	return fmt.Sprintf("%s %s %s %s", word(e.Type), size, shortHash(e.SHA256), state)
}
