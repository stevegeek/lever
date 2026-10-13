package treebackup

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
)

// memReader is an in-memory EntryReader over entries already in manifest
// order, for tests that do not need the JSON form.
type memReader struct {
	h       *Header
	entries []Entry
	i       int
	err     error // returned instead of io.EOF at the end, if set
}

func (m *memReader) Header() *Header { return m.h }

func (m *memReader) Next() (*Entry, error) {
	if m.i >= len(m.entries) {
		if m.err != nil {
			return nil, m.err
		}
		return nil, io.EOF
	}
	e := m.entries[m.i]
	m.i++
	return &e, nil
}

func (m *memReader) Close() error { return nil }

// memManifest returns an OpenManifest over h and entries, sorted into
// manifest order.
func memManifest(h *Header, entries []Entry) OpenManifest {
	es := slices.Clone(entries)
	slices.SortFunc(es, func(a, b Entry) int { return ComparePaths(a.Path, b.Path) })
	return func() (EntryReader, error) { return &memReader{h: h, entries: es}, nil }
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
