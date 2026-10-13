//go:build linux || darwin

package treebackup

import (
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLinkInsideRule(t *testing.T) {
	cases := []struct {
		target string
		depth  int
		want   bool
	}{
		{"f", 0, true},
		{"a/b/c", 0, true},
		{"a/", 0, true},
		{"..", 1, true},
		{"../", 1, true},
		{"..", 0, false},
		{"../..", 1, false},
		{"../../x", 2, true},
		{"../../x/y/", 2, true},
		{"b/../..", 5, false},
		{"b/..", 5, false},
		{"./b", 5, false},
		{"b/.", 5, false},
		{"a//b", 5, false},
		{"a//", 5, false},
		{"/etc", 5, false},
		{"/", 5, false},
		{"", 5, false},
		{"a\x00b", 5, false},
		{"../a/..", 5, false},
		{"..a", 0, true},
		{"...", 0, true},
	}
	for _, c := range cases {
		if got := linkInside([]byte(c.target), c.depth); got != c.want {
			t.Errorf("linkInside(%q, %d) = %v, want %v", c.target, c.depth, got, c.want)
		}
	}
}

type modelNode struct {
	dir    bool
	target string // links only
}

// randTarget draws link targets that mix the shapes the rule accepts with
// the ones it must refuse.
func randTarget(r *rand.Rand) string {
	comps := []string{"..", "a", "b", "c", ".", ""}
	var parts []string
	for range r.IntN(3) {
		parts = append(parts, "..")
	}
	for range r.IntN(4) {
		parts = append(parts, comps[r.IntN(len(comps))])
	}
	s := strings.Join(parts, "/")
	switch r.IntN(10) {
	case 0:
		s = "/" + s
	case 1:
		s += "/"
	}
	return s
}

// randModel builds a small tree of real directories and links; only links
// accept takes are kept.
func randModel(r *rand.Rand, accept func(target, parent string) bool) map[string]modelNode {
	nodes := map[string]modelNode{"": {dir: true}}
	dirs := []string{""}
	names := []string{"a", "b", "c"}
	for range 4 + r.IntN(12) {
		parent := dirs[r.IntN(len(dirs))]
		if Depth([]byte(parent)) >= 4 {
			continue
		}
		p := names[r.IntN(len(names))]
		if parent != "" {
			p = parent + "/" + p
		}
		if _, ok := nodes[p]; ok {
			continue
		}
		if r.IntN(2) == 0 {
			nodes[p] = modelNode{dir: true}
			dirs = append(dirs, p)
			continue
		}
		tg := randTarget(r)
		if accept(tg, parent) {
			nodes[p] = modelNode{target: tg}
		}
	}
	return nodes
}

// resolveModel follows a path from the root the way the kernel would:
// ".." is physical (the parent of the directory reached), links are
// followed wherever they appear. escaped reports a ".." above the root.
func resolveModel(nodes map[string]modelNode, stack []string, comps []string, hops int) (out []string, ok, escaped bool) {
	for _, c := range comps {
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return nil, false, true
			}
			stack = stack[:len(stack)-1]
			continue
		}
		p := strings.Join(append(append([]string{}, stack...), c), "/")
		n, found := nodes[p]
		if !found {
			return nil, false, false
		}
		if n.dir {
			stack = append(append([]string{}, stack...), c)
			continue
		}
		if hops > 40 || strings.HasPrefix(n.target, "/") {
			return nil, false, strings.HasPrefix(n.target, "/")
		}
		next, ok, esc := resolveModel(nodes, stack, strings.Split(n.target, "/"), hops+1)
		if esc {
			return nil, false, true
		}
		if !ok {
			return nil, false, false
		}
		stack = next
	}
	return stack, true, false
}

// The inside rule never lets a resolution leave the root, whatever names
// are looked up through whatever links.
func TestLinkInsideRuleFuzzNeverEscapes(t *testing.T) {
	inside := func(tg, parent string) bool { return linkInside([]byte(tg), Depth([]byte(parent))) }
	if n := fuzzEscapes(60000, inside); n != 0 {
		t.Fatalf("%d escapes under the inside rule", n)
	}
	// The resolver must be able to see an escape: the lexical rule escapes.
	lexical := func(tg, parent string) bool {
		if strings.HasPrefix(tg, "/") {
			return false
		}
		c := path.Clean(path.Join(parent, tg))
		return c != ".." && !strings.HasPrefix(c, "../")
	}
	if n := fuzzEscapes(2000, lexical); n == 0 {
		t.Fatal("the fuzz found no escape under the lexical rule")
	}
}

func fuzzEscapes(trees int, accept func(target, parent string) bool) int {
	r := rand.New(rand.NewPCG(1, 2))
	names := []string{"a", "b", "c"}
	escapes := 0
	for range trees {
		nodes := randModel(r, accept)
		for range 40 {
			q := make([]string, 1+r.IntN(6))
			for i := range q {
				q[i] = names[r.IntN(len(names))]
			}
			if _, _, esc := resolveModel(nodes, nil, q, 0); esc {
				escapes++
			}
		}
	}
	return escapes
}

// The same property on a real filesystem: random manifests restored with
// --links=inside, every path resolved by the kernel stays in the target.
// Under --links=all the same manifests do escape, which shows the check
// can see an escape.
func TestRestoreRandomLinksResolveInsideTheTarget(t *testing.T) {
	if n := realEscapes(t, LinksInside); n != 0 {
		t.Fatalf("%d escapes under --links=inside", n)
	}
	if n := realEscapes(t, LinksAll); n == 0 {
		t.Fatal("no escape seen under --links=all")
	}
}

func realEscapes(t *testing.T, policy LinkPolicy) int {
	r := rand.New(rand.NewPCG(3, 4))
	names := []string{"a", "b", "c"}
	base := t.TempDir()
	escapes := 0
	for i := range 150 {
		nodes := map[string]bool{}
		tr := newRTree()
		dirs := []string{""}
		for range 4 + r.IntN(14) {
			parent := dirs[r.IntN(len(dirs))]
			p := names[r.IntN(len(names))]
			if parent != "" {
				p = parent + "/" + p
			}
			if nodes[p] || Depth([]byte(p)) > 4 {
				continue
			}
			nodes[p] = true
			if r.IntN(2) == 0 {
				dirs = append(dirs, p)
				tr.dir(p, 0o755)
				continue
			}
			tr.link(p, randTarget(r))
		}
		// Targets sit at varying depths so a ".." that escapes lands on a
		// real directory.
		target := filepath.Join(base, strings.Repeat("d/", i%4), "t"+strconv.Itoa(i))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		o := tr.opts(target)
		o.Links = policy
		restore(t, o)
		real, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatal(err)
		}
		for range 60 {
			q := make([]string, 1+r.IntN(6))
			for j := range q {
				q[j] = names[r.IntN(len(names))]
			}
			got, err := filepath.EvalSymlinks(filepath.Join(append([]string{target}, q...)...))
			if err != nil {
				continue
			}
			if got != real && !strings.HasPrefix(got, real+string(filepath.Separator)) {
				escapes++
			}
		}
	}
	return escapes
}
