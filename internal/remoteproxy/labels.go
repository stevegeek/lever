package remoteproxy

// Labels: short text the manager writes beside agent names. The file is
// jail-written: it is read with no symbolic link on its path, bounded, and
// each label is reduced to one short line of text before it leaves this
// package. The page shows a label only as text beside the agent's true name.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/stevegeek/lever/internal/fsutil"
)

const (
	MaxLabelsFile = 16 << 10
	MaxLabelRunes = 60
	labelsTTL     = 10 * time.Second
)

// ErrLabelsShape means the file is not a JSON object of strings.
var ErrLabelsShape = errors.New("not a JSON object of strings")

// CleanLabel applies the page's oneLine rules (chatcore.js).
func CleanLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) ||
			r == 0x2800 || r == 0x3164 || r == 0x115F || r == 0x1160 || r == 0xFFA0 {
			r = ' '
		}
		b.WriteRune(r)
	}
	out := []rune(strings.Join(strings.Fields(b.String()), " "))
	if len(out) > MaxLabelRunes {
		out = out[:MaxLabelRunes]
	}
	return string(out)
}

// ParseLabels drops empty labels. Unknown names stay: the caller only looks
// up names it already shows.
func ParseLabels(b []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	var raw map[string]string
	if err := dec.Decode(&raw); err != nil || raw == nil {
		return nil, ErrLabelsShape
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrLabelsShape
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if c := CleanLabel(v); c != "" {
			out[k] = c
		}
	}
	return out, nil
}

func ReadLabels(tree, rel string) (map[string]string, error) {
	b, err := fsutil.ReadInTreeNoLinks(tree, rel, MaxLabelsFile)
	if err != nil {
		return nil, err
	}
	return ParseLabels(b)
}

// LabelSource re-reads the file only when the leaf's mtime or size changed.
// Any fault is "no labels" (lever doctor names it).
type LabelSource struct {
	Tree, Rel string
	now       func() time.Time

	mu     sync.Mutex
	at     time.Time
	mtime  time.Time
	size   int64
	labels map[string]string
}

// Labels returns a copy, nil for none, so a caller cannot change the cache.
// Nil-safe.
func (s *LabelSource) Labels() map[string]string {
	if s == nil || s.Rel == "" {
		return nil
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now()
	if !s.at.IsZero() && t.Sub(s.at) < labelsTTL {
		return maps.Clone(s.labels)
	}
	s.at = t
	fi, err := fsutil.StatInTreeNoLinks(s.Tree, s.Rel)
	if err != nil || !fi.Mode().IsRegular() {
		s.labels, s.mtime, s.size = nil, time.Time{}, -1
		return nil
	}
	if s.labels != nil && fi.ModTime().Equal(s.mtime) && fi.Size() == s.size {
		return maps.Clone(s.labels)
	}
	labels, err := ReadLabels(s.Tree, s.Rel)
	if err != nil {
		labels = nil
	}
	s.labels, s.mtime, s.size = labels, fi.ModTime(), fi.Size()
	return maps.Clone(s.labels)
}
