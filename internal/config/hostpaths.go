package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	// hostProgramOutside is code the host runs that must lie outside the
	// tree altogether (lever-tool-whisper's -server): a manager.read_only
	// mount does not excuse it.
	hostProgramOutside
	// hostPrivate is a host path an agent must neither replace nor reach:
	// lever-tool-whisper's -models (the model whisper-server loads, and its
	// working directory) and -dictate-socket (remote.voice.socket). Never
	// safe in the tree, read-only or not.
	hostPrivate
	// hostUnchecked is a known flag whose value is not a host program or
	// secret: the github tool's -tree (it names the tree on purpose) or a
	// data file the agent may edit by design (-csv, -dsn). Consumed so it
	// is not mistaken for anything else, and not checked.
	hostUnchecked
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
// (cmd/lever-tool-github, cmd/lever-tool-fizzy, cmd/lever-tool-db, and the
// assistant-demo example's lever-tool-todo) and what each names, read in
// every supervised tool's command. Lever cannot know what an unknown flag's
// value is, so no other flag is checked.
var toolPathFlags = map[string]hostPathKind{
	"app-key":    hostSecret,    // github: the GitHub App private key
	"token-file": hostSecret,    // fizzy: the personal access token
	"state":      hostSecret,    // github, fizzy: the tool's private state dir
	"fizzy":      hostProgram,   // fizzy: the fizzy CLI it runs
	"tree":       hostUnchecked, // github, whisper: the tree, to refuse paths inside it
	"dsn":        hostUnchecked, // db: its sqlite data
	"csv":        hostUnchecked, // todo (example): the agent-editable todo list
}

// whisperToolFlags are lever-tool-whisper's flags config load reads, read
// only in the command of a tool that runs lever-tool-whisper
// (isWhisperCommand): generic names such as -server or -model mean
// something else to another tool.
var whisperToolFlags = map[string]hostPathKind{
	"server":         hostProgramOutside, // the whisper-server program it runs
	"models":         hostPrivate,        // the model directory
	"dictate-socket": hostPrivate,        // the dictation socket (remote.voice.socket)
	// Non-path flags (WhisperTools): consumed with their values, never
	// checked as paths.
	"whisper-port":      hostUnchecked,
	"model":             hostUnchecked,
	"max-seconds":       hostUnchecked,
	"agent-max-seconds": hostUnchecked,
}

// whisperProgram is the base name of lever-tool-whisper's program.
const whisperProgram = "lever-tool-whisper"

// isWhisperCommand reports whether t runs lever-tool-whisper: its program,
// after an env(1) prefix, has that base name, or its command takes
// -dictate-socket (a renamed copy or wrapper keeps the whisper checks).
func isWhisperCommand(t Tool) bool {
	if t.External || len(t.Command) == 0 {
		return false
	}
	argv, _ := unwrapEnv(t.Command)
	if len(argv) == 0 {
		return false
	}
	if baseName(argv[0]) == whisperProgram {
		return true
	}
	// A renamed copy or wrapper still gets the whisper checks when it
	// takes the dictation socket flag, which no other lever tool has.
	for _, a := range argv[1:] {
		name := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if a != name && (name == "dictate-socket" || strings.HasPrefix(name, "dictate-socket=")) {
			return true
		}
	}
	return false
}

// knownFlag is the kind of a flag name, and whether lever reads it in a
// command: toolPathFlags in every tool's, whisperToolFlags in a
// whisper tool's.
func knownFlag(whisper bool, name string) (hostPathKind, bool) {
	if k, ok := toolPathFlags[name]; ok {
		return k, true
	}
	if whisper {
		k, ok := whisperToolFlags[name]
		return k, ok
	}
	return 0, false
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
	if a.RemoteEnabled() && a.Remote.Voice.Enabled {
		add(hostPath{key: "remote.voice.socket", path: a.Remote.Voice.Socket, kind: hostPrivate})
	}
	return out
}

// toolHostPaths lists the paths in one supervised tool's command that lever
// can classify — a best-effort guard, not a sandbox:
//
//   - the program itself when it is given as a path (a bare name is looked
//     up on the supervisor's fixed PATH, which is not in the tree), after
//     an env(1) prefix too (unwrapEnv);
//   - the value of each known path flag of the shipped tools
//     (toolPathFlags; whisperToolFlags only in lever-tool-whisper's
//     command), as "-f v", "-f=v" or "--f=v";
//   - the code an interpreter runs (interpreterPaths): its script, the
//     values of its code-path flags (ruby -I, node --require), and the
//     paths in inline code (sh -c, -e).
//
// Other flags and arguments are not checked: lever cannot tell a secret
// from a data file in an unknown tool's argv.
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
	// After an env prefix, a relative path resolves against env's -C
	// directory (joinRaw keeps a ".." in it for the refusal).
	argv, chdir := t.Command, ""
	if real, dir := unwrapEnv(argv); len(real) > 0 && len(real) < len(argv) {
		argv, chdir = real, dir
		if strings.ContainsRune(argv[0], '/') {
			add("command (after env)", joinRaw(chdir, argv[0]), hostProgram)
		}
	}
	if fam := interpreterFamily(baseName(argv[0])); fam != "" {
		for _, p := range interpreterPaths(fam, argv) {
			add(p.what, joinRaw(chdir, p.path), hostProgram)
		}
	}
	for _, f := range toolFlags(t) {
		if f.kind != hostUnchecked {
			if f.val != "" {
				add("-"+f.name, f.val, f.kind)
			}
			if f.dashNext != "" {
				add("-"+f.name, f.dashNext, f.kind)
			}
		}
	}
	return out
}

// toolFlag is one known flag in a tool's command (knownFlag), its kind,
// and its value ("" when it has none).
type toolFlag struct {
	name, val string
	kind      hostPathKind
	// dashNext is the following word when it starts with "-" and so was
	// not taken as the value: Go's flag package would take it, so a path
	// flag's check covers it too (toolHostPaths).
	dashNext string
}

// toolFlags lists the known flags in t's command (knownFlag), as "-f v",
// "-f=v", "--f v" or "--f=v", in order. Unknown flags and other arguments
// are skipped. A following word that starts with "-" is never taken as a
// flag's value: it is read as a flag itself, so "-model -state x" cannot
// hide -state from its check (the flag before it then has no value; a path
// flag's check still covers that word as its value, as Go's flag package
// would read it).
func toolFlags(t Tool) []toolFlag {
	if len(t.Command) == 0 {
		return nil
	}
	whisper := isWhisperCommand(t)
	var out []toolFlag
	// The words the tool itself gets: past an env(1) prefix, with a plain
	// env -S string split into words. The env forms this cannot read as env
	// does (quotes, escapes or $ in -S, -vS, --split-string=) are refused
	// at config load (envFormUnread).
	argv, _ := unwrapEnv(t.Command)
	if len(argv) == 0 {
		argv = t.Command
	}
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		name, ok := strings.CutPrefix(args[i], "-")
		if !ok || name == "" {
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(name, "-"), "=")
		kind, known := knownFlag(whisper, name)
		if !known {
			continue
		}
		dashNext := ""
		if !hasVal && i+1 < len(args) {
			if strings.HasPrefix(args[i+1], "-") {
				dashNext = args[i+1]
			} else {
				i++
				val = args[i]
			}
		}
		out = append(out, toolFlag{name: name, val: val, kind: kind, dashNext: dashNext})
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
			class := "unreadable"
			if errors.Is(err, fs.ErrPermission) {
				class = "permission denied"
			}
			return fmt.Errorf("config: %s: lever cannot tell whether it lies in the mounted tree (%s: %w), and it refuses "+
				"what it cannot check — make the path's directories readable by you, or move it", p.key, class, err)
		}
		if !w.inTree {
			continue
		}
		if p.kind == hostProgramOutside {
			return fmt.Errorf("config: %s %q is inside the mounted tree (%s): an agent could replace the program the host runs — "+
				"install it outside the tree (a manager.read_only mount does not excuse it)", p.key, p.path, a.Tree)
		}
		if p.kind == hostPrivate {
			return fmt.Errorf("config: %s %q is inside the mounted tree (%s): an agent could replace or reach it there, "+
				"and a manager.read_only mount does not excuse it — move it outside the tree", p.key, p.path, a.Tree)
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

// ToolsOnReadOnly maps each supervised tool whose command runs a program
// from inside the tree to the manager.read_only entries that excuse it
// (checkHostPathsOutsideTree). That excuse holds only while the running
// manager carries those read-only mounts, which are create-time only, so the
// broker starts such a tool only after it has seen them (brokerctl).
func (a *App) ToolsOnReadOnly() (map[string][]string, error) {
	out := map[string][]string{}
	paths := a.hostPaths()
	if len(paths) == 0 {
		return out, nil
	}
	realTree, err := resolveExisting(a.Tree)
	if err != nil {
		return nil, fmt.Errorf("config: tree %s: %w", a.Tree, err)
	}
	for _, p := range paths {
		if p.tool == "" || p.kind != hostProgram {
			continue
		}
		w, err := a.walkTree(realTree, p.path)
		if err != nil {
			return nil, fmt.Errorf("config: %s %q: %w", p.key, p.path, err)
		}
		if w.inTree && w.entry != "" && !slices.Contains(out[p.tool], w.entry) {
			out[p.tool] = append(out[p.tool], w.entry)
		}
	}
	return out, nil
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
