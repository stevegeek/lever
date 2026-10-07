package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// hostPathKind says what the host does with a path the config names, which
// decides whether manager.read_only can make an in-tree location safe.
type hostPathKind int

const (
	// hostProgram is code the host runs (a broker tool's command, or a
	// script it is handed). Safe in the tree only when the manager sees it
	// read-only and no worker mounts it read-write.
	hostProgram hostPathKind = iota
	// hostSecret is a file the host reads as a credential or trust anchor,
	// or a private state dir a host tool keeps. Never safe in the tree: a
	// read-only mount still lets the manager read it.
	hostSecret
)

// hostPath is one host-side path from the config, absolute, with the key
// that named it (for the error).
type hostPath struct {
	key  string
	path string
	kind hostPathKind
}

// toolPathFlags are the path flags of the tools lever ships
// (cmd/lever-tool-github, cmd/lever-tool-fizzy) and what each names. Only
// these are read as flag values; any other argument is checked only when it
// looks like a path itself (toolHostPaths).
var toolPathFlags = map[string]hostPathKind{
	"app-key":    hostSecret,  // github: the GitHub App private key
	"token-file": hostSecret,  // fizzy: the personal access token
	"state":      hostSecret,  // github, fizzy: the tool's private state dir
	"fizzy":      hostProgram, // fizzy: the fizzy CLI it runs
}

// hostPaths lists every host-run program and host secret the config names.
// Paths are made absolute against the instance dir, which is also the
// broker's working directory (it runs where lever runs: the instance root).
func (a *App) hostPaths() []hostPath {
	var out []hostPath
	add := func(key, p string, kind hostPathKind) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(a.dir, p)
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, hostPath{key, p, kind})
	}
	add("manager.credential_file", a.Manager.CredentialFile, hostSecret)
	add("broker.api_key_file", a.Broker.APIKeyFile, hostSecret)
	add("operator.signing_key", a.Operator.SigningKey, hostSecret)
	add("operator.allowed_signers", a.OperatorAllowedSignersPath(), hostSecret)
	for _, t := range a.Broker.Tools {
		for _, p := range toolHostPaths(t) {
			add(p.key, p.path, p.kind)
		}
	}
	return out
}

// toolHostPaths lists the paths in one supervised tool's command: the
// program itself when it is given as a path (a bare name is looked up on
// the supervisor's fixed PATH, which is not in the tree), the value of each
// known path flag (toolPathFlags), and any other argument that is a path —
// absolute, or relative with a "/" in it. An unknown argument is treated as
// a program (it may be the script an interpreter runs); arbitrary programs'
// own flags are not parsed.
func toolHostPaths(t Tool) []hostPath {
	if t.External || len(t.Command) == 0 {
		return nil
	}
	var out []hostPath
	key := func(what string) string { return fmt.Sprintf("broker.tools[%s] %s", t.Name, what) }
	if strings.ContainsRune(t.Command[0], '/') {
		out = append(out, hostPath{key("command"), t.Command[0], hostProgram})
	}
	args := t.Command[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if name, ok := strings.CutPrefix(arg, "-"); ok && name != "" {
			name = strings.TrimPrefix(name, "-")
			name, val, hasVal := strings.Cut(name, "=")
			kind, known := toolPathFlags[name]
			switch {
			case known && hasVal:
				out = append(out, hostPath{key("-" + name), val, kind})
			case known && i+1 < len(args):
				i++
				out = append(out, hostPath{key("-" + name), args[i], kind})
			case hasVal && looksLikePath(val):
				out = append(out, hostPath{key("-" + name), val, hostProgram})
			}
			continue
		}
		if looksLikePath(arg) {
			out = append(out, hostPath{key(fmt.Sprintf("argument %q", arg)), arg, hostProgram})
		}
	}
	return out
}

// looksLikePath: absolute, or relative with a directory part.
func looksLikePath(s string) bool { return strings.ContainsRune(s, '/') }

// checkHostPathsOutsideTree refuses a host-run program or host secret that
// lies in the mounted tree, where an agent can write (and read) it. A
// program is allowed there only under a manager.read_only entry, reached
// through no symbolic link inside the tree, and in no worker dir.
func (a *App) checkHostPathsOutsideTree() error {
	paths := a.hostPaths()
	if len(paths) == 0 {
		return nil
	}
	realTree, err := resolveExisting(a.Tree)
	if err != nil {
		return fmt.Errorf("config: tree %s: %w", a.Tree, err)
	}
	for _, p := range paths {
		w, err := a.walkTree(realTree, p.path)
		if err != nil {
			return fmt.Errorf("config: %s %q: %w", p.key, p.path, err)
		}
		if !w.inTree {
			continue
		}
		if p.kind == hostSecret {
			return fmt.Errorf("config: %s %q is inside the mounted tree (%s): an agent can read and replace it there, "+
				"and a manager.read_only mount still lets the manager read it — move it outside the tree", p.key, p.path, a.Tree)
		}
		switch {
		case w.viaLink:
			return fmt.Errorf("config: %s %q reaches the mounted tree (%s) through a symbolic link: an agent can repoint "+
				"a link in the tree, and the host runs what it points at — move it outside the tree, or name its real path "+
				"under a manager.read_only entry", p.key, p.path, a.Tree)
		case w.worker != "":
			return fmt.Errorf("config: %s %q is inside worker %q's dir, which the worker mounts read-write, and the host runs it — "+
				"move it outside the tree, or cover it with manager.read_only outside every worker dir", p.key, p.path, w.worker)
		case w.entry == "":
			return fmt.Errorf("config: %s %q is inside the mounted tree (%s): an agent can replace it there, and the host runs it — "+
				"move it outside the tree, or cover it with manager.read_only", p.key, p.path, a.Tree)
		}
	}
	return nil
}

// treeWalk is what walkTree learnt about one path.
type treeWalk struct {
	inTree  bool   // the path is the tree, lies in it, or reaches it through a link
	viaLink bool   // a symbolic link inside the tree, or one pointing into it, is on the way
	entry   string // the manager.read_only entry the path lies under ("" = none)
	worker  string // the worker whose dir holds the path ("" = none)
}

// maxLinkDepth bounds the links walkTree follows (the kernel's own limit).
const maxLinkDepth = 40

// walkTree follows p one component at a time, as the kernel resolves it,
// and reports where it passes. The tree, the read_only entries and the
// worker dirs are recognised by identity (os.SameFile), not by spelling, so
// a case alias on a case-insensitive filesystem or a link at the instance
// root is seen through. A missing tail is placed lexically below the deepest
// existing component; and in case the tree does not exist yet, the resolved
// path is also compared with the tree lexically.
func (a *App) walkTree(realTree, p string) (treeWalk, error) {
	var w treeWalk
	treeFI, err := os.Stat(realTree)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return w, err
		}
		r, err := resolveExisting(p)
		if err != nil {
			return w, err
		}
		w.inTree = insideTree(realTree, r)
		return w, nil
	}
	// Each read_only entry and worker dir that exists, to recognise.
	type mark struct {
		name string
		fi   os.FileInfo
	}
	var entries, workers []mark
	for _, e := range a.Manager.ReadOnly {
		if fi, err := os.Stat(filepath.Join(realTree, e)); err == nil {
			entries = append(entries, mark{e, fi})
		}
	}
	for _, g := range a.Workers {
		if fi, err := os.Stat(filepath.Join(realTree, g.Dir)); err == nil {
			workers = append(workers, mark{g.Name, fi})
		}
	}
	var walk func(p string, depth int) error
	walk = func(p string, depth int) error {
		if depth > maxLinkDepth {
			return errors.New("too many levels of symbolic links")
		}
		cur := string(filepath.Separator)
		comps := strings.Split(strings.TrimPrefix(filepath.Clean(p), string(filepath.Separator)), string(filepath.Separator))
		for i, c := range comps {
			if c == "" {
				continue
			}
			next := filepath.Join(cur, c)
			fi, err := os.Lstat(next)
			if errors.Is(err, fs.ErrNotExist) {
				// The rest does not exist yet. Below the tree it would be
				// created in the tree; elsewhere, only a lexical match with
				// a tree that does not exist could put it there.
				if !w.inTree && insideTree(realTree, filepath.Join(append([]string{next}, comps[i+1:]...)...)) {
					w.inTree = true
				}
				return nil
			}
			if err != nil {
				return err
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(next)
				if err != nil {
					return err
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(cur, target)
				}
				if w.inTree {
					w.viaLink = true
					return nil
				}
				if err := walk(target, depth+1); err != nil {
					return err
				}
				if w.inTree {
					w.viaLink = true
					return nil
				}
				if cur, err = resolveExisting(target); err != nil {
					return err
				}
				continue
			}
			cur = next
			if !w.inTree && os.SameFile(fi, treeFI) {
				w.inTree = true
				continue
			}
			if w.inTree {
				for _, m := range entries {
					if os.SameFile(fi, m.fi) {
						w.entry = m.name
					}
				}
				for _, m := range workers {
					if os.SameFile(fi, m.fi) {
						w.worker = m.name
					}
				}
			}
		}
		return nil
	}
	if err := walk(p, 0); err != nil {
		return w, err
	}
	return w, nil
}
