package treebackup

import (
	"bytes"
	"slices"
	"time"
)

// group is one file to read: the paths that share a (dev, ino), so a hard
// link is read once.
type group struct {
	members []*node
	size    int64
	class   int
	root    *writerRoot
	secret  bool
}

type groupKey struct {
	id     DevIno
	secret bool
}

// mergeBase calls set for each node whose path the base holds. Both are
// in manifest order, and the base is streamed.
func mergeBase(nodes []*node, base *Base, set func(n *node, e *Entry)) {
	if base == nil {
		return
	}
	i := 0
	base.each(func(e *Entry) bool {
		for i < len(nodes) && ComparePaths(nodes[i].e.Path, e.Path) < 0 {
			i++
		}
		if i == len(nodes) {
			return false
		}
		if bytes.Equal(nodes[i].e.Path, e.Path) {
			set(nodes[i], e)
		}
		return true
	})
}

// racyWindow is how long after a base started a ctime must predate for the
// stat cache to trust it: a running VM can update ctime late (dirty mmap
// pages), a stopped one cannot.
func racyWindow(h *Header) time.Duration {
	if h != nil && h.Quiescent == QuiescentVMOff {
		return 2 * time.Second
	}
	return 120 * time.Second
}

// cacheHit reports whether c, the cache base's entry for n's path, may
// stand for n's content without a read.
func (w *walker) cacheHit(n *node, c *Entry) bool {
	h := w.o.Cache.Header
	if h == nil || n.e.Type != TypeFile || !n.e.HasMeta || n.e.State != StateOK || n.e.Size > w.o.Limits.File {
		return false
	}
	if c.Type != TypeFile || c.State != StateOK || c.SHA256 == "" || !c.HasMeta {
		return false
	}
	if c.Dev != n.e.Dev || c.Ino != n.e.Ino || c.Size != n.e.Size || c.Mtime != n.e.Mtime || c.Ctime != n.e.Ctime {
		return false
	}
	if n.e.Ctime >= h.Created.Add(-racyWindow(h)).UnixNano() {
		return false
	}
	return w.sink.HasBlob(c.SHA256, c.Size)
}

// selectContent: class 0 is a known path whose content was captured, read
// first; class 1 is everything else, read per writer root.
func (w *walker) selectContent() (class0 []*group, class1 map[*writerRoot][]*group, reverify []*group) {
	mergeBase(w.all, w.o.Known, func(n *node, e *Entry) { n.knownContent = e.SHA256 != "" })
	mergeBase(w.all, w.o.Cache, func(n *node, e *Entry) {
		if w.cacheHit(n, e) {
			n.cacheHit = true
			n.e.SHA256, n.e.Verified = e.SHA256, e.Verified
		}
	})

	groups := make(map[groupKey]*group)
	var order []*group
	revGroups := make(map[DevIno]*group)
	stale := w.t.Add(-w.o.Reverify).Unix()
	for _, n := range w.all {
		if n.e.Type != TypeFile || !n.e.HasMeta {
			continue
		}
		secret := n.e.State == StateSecret
		if n.e.State != StateOK && !secret {
			continue
		}
		if n.e.Size > w.o.Limits.File {
			n.e.State, n.e.Reason = StateTooBig, ""
			continue
		}
		if n.cacheHit {
			if w.o.Full || (w.o.Reverify > 0 && n.e.Verified < stale) {
				id := DevIno{n.e.Dev, n.e.Ino}
				g := revGroups[id]
				if g == nil {
					g = &group{size: n.e.Size}
					revGroups[id] = g
					reverify = append(reverify, g)
				}
				g.members = append(g.members, n)
			}
			continue
		}
		// Only a file with more than one link can share a group; the map
		// holds those alone, since it costs memory per entry.
		var g *group
		k := groupKey{DevIno{n.e.Dev, n.e.Ino}, secret}
		if n.e.Nlink > 1 {
			g = groups[k]
		}
		if g == nil {
			g = &group{size: n.e.Size, class: 1, root: w.rootOf(n.e.Path), secret: secret}
			if n.e.Nlink > 1 {
				groups[k] = g
			}
			order = append(order, g)
		}
		g.members = append(g.members, n)
		if n.knownContent {
			g.class = 0
		}
	}

	class1 = make(map[*writerRoot][]*group)
	for _, g := range order {
		if g.class == 0 {
			class0 = append(class0, g)
		} else {
			class1[g.root] = append(class1[g.root], g)
		}
	}
	bySize := func(a, b *group) int {
		switch {
		case a.size < b.size:
			return -1
		case a.size > b.size:
			return 1
		}
		return 0
	}
	slices.SortStableFunc(class0, bySize)
	for _, gs := range class1 {
		slices.SortStableFunc(gs, bySize)
	}
	slices.SortStableFunc(reverify, func(a, b *group) int {
		va, vb := a.members[0].e.Verified, b.members[0].e.Verified
		switch {
		case va < vb:
			return -1
		case va > vb:
			return 1
		}
		return 0
	})
	return class0, class1, reverify
}
