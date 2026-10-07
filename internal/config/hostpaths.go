package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
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
// that named it (for the error), and the tool whose command holds it ("" for
// a top-level key). raw is the path as written; dotDot marks one with a
// ".." component.
type hostPath struct {
	key    string
	path   string
	kind   hostPathKind
	tool   string
	raw    string
	dotDot bool
}

// toolPathFlags are the path flags of the tools lever ships
// (cmd/lever-tool-github, cmd/lever-tool-fizzy) and what each names. Only
// these are read as flag values; any other argument is checked only for the
// paths in it (pathCandidates).
var toolPathFlags = map[string]hostPathKind{
	"app-key":    hostSecret,  // github: the GitHub App private key
	"token-file": hostSecret,  // fizzy: the personal access token
	"state":      hostSecret,  // github, fizzy: the tool's private state dir
	"fizzy":      hostProgram, // fizzy: the fizzy CLI it runs
}

// scriptInterpreters are commands whose first argument is a script they
// run: that argument is a program (it may sit under manager.read_only),
// where any other unknown argument is treated as a secret.
var scriptInterpreters = map[string]bool{
	"python": true, "python3": true, "ruby": true, "node": true, "sh": true,
	"bash": true, "perl": true, "deno": true, "bun": true,
}

// hostPaths lists every host-run program and host secret the config names.
// Paths are made absolute against the instance dir, which the supervisor
// also makes every tool's working directory (brokerctl.ToolSpec.Dir).
func (a *App) hostPaths() []hostPath {
	var out []hostPath
	add := func(p hostPath) {
		if p.path == "" {
			return
		}
		p.raw = p.path
		p.dotDot = slices.Contains(strings.Split(filepath.ToSlash(p.path), "/"), "..")
		if !filepath.IsAbs(p.path) {
			p.path = filepath.Join(a.InstanceDir(), p.path)
		}
		if abs, err := filepath.Abs(p.path); err == nil {
			p.path = abs
		}
		out = append(out, p)
	}
	add(hostPath{key: "manager.credential_file", path: a.Manager.CredentialFile, kind: hostSecret})
	add(hostPath{key: "broker.api_key_file", path: a.Broker.APIKeyFile, kind: hostSecret})
	add(hostPath{key: "operator.signing_key", path: a.Operator.SigningKey, kind: hostSecret})
	add(hostPath{key: "operator.allowed_signers", path: a.OperatorAllowedSignersPath(), kind: hostSecret})
	for _, t := range a.Broker.Tools {
		for _, p := range toolHostPaths(t) {
			add(p)
		}
	}
	return out
}

// toolHostPaths lists the paths in one supervised tool's command, a
// best-effort reading (arbitrary programs' own flags are not parsed):
//
//   - the program itself when it is given as a path (a bare name is looked
//     up on the supervisor's fixed PATH, which is not in the tree): a
//     program;
//   - the value of each known path flag (toolPathFlags), of the kind the
//     table gives;
//   - every path in any other argument (pathCandidates): a secret, except
//     the first argument of a script interpreter (scriptInterpreters),
//     which is a program.
func toolHostPaths(t Tool) []hostPath {
	if t.External || len(t.Command) == 0 {
		return nil
	}
	var out []hostPath
	add := func(what, p string, kind hostPathKind) {
		out = append(out, hostPath{key: fmt.Sprintf("broker.tools[%s] %s", t.Name, what), path: p, kind: kind, tool: t.Name})
	}
	if strings.ContainsRune(t.Command[0], '/') {
		add("command", t.Command[0], hostProgram)
	}
	interpreter := scriptInterpreters[filepath.Base(t.Command[0])]
	args := t.Command[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if name, ok := strings.CutPrefix(arg, "-"); ok && name != "" {
			name, val, hasVal := strings.Cut(strings.TrimPrefix(name, "-"), "=")
			if kind, known := toolPathFlags[name]; known {
				switch {
				case hasVal:
					add("-"+name, val, kind)
				case i+1 < len(args):
					i++
					add("-"+name, args[i], kind)
				}
				continue
			}
		}
		for _, c := range pathCandidates(arg) {
			kind := hostSecret
			if interpreter && i == 0 && c == arg {
				kind = hostProgram
			}
			add(fmt.Sprintf("argument %q", arg), c, kind)
		}
	}
	return out
}

// pathCandidates are the paths an argument may hold: it is split on
// whitespace, '=', ':' and ',' (a shell string, a glued flag value such as
// -I/x or --x=/y, a PATH-like list), and each piece with a "/" yields itself
// when it is relative (not a flag) and every substring starting at a "/".
// A heuristic: it cannot know what a program does with its arguments.
func pathCandidates(arg string) []string {
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	sep := func(r rune) bool { return unicode.IsSpace(r) || r == '=' || r == ':' || r == ',' }
	for _, tok := range strings.FieldsFunc(arg, sep) {
		if !strings.ContainsRune(tok, '/') {
			continue
		}
		if tok[0] != '/' && tok[0] != '-' {
			add(tok)
		}
		for j := range len(tok) {
			if tok[j] == '/' {
				add(tok[j:])
			}
		}
	}
	return out
}

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
		if p.dotDot {
			return fmt.Errorf("config: %s %q has a \"..\" component: the kernel resolves \"..\" after following links, so the path "+
				"may reach somewhere other than it reads — write it without \"..\"", p.key, p.raw)
		}
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
