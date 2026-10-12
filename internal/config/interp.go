package config

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// flagRole is what an interpreter does with a flag's value.
type flagRole int

const (
	// takesValue: the flag has a value that is not code (-W ignore).
	takesValue flagRole = iota + 1
	// codePath: the value names code the interpreter loads (ruby -I lib,
	// node --require x): a program.
	codePath
	// inlineCode: the value is code (-c, -e); its paths are programs, and
	// no script follows.
	inlineCode
	// noScript: the value names a module run instead of a script
	// (python -m); nothing follows to check.
	noScript
)

// interpreterFlags are, per interpreter family, the flags whose value is
// not a flag group of its own. Flags not listed take no value.
var interpreterFlags = map[string]map[string]flagRole{
	"python": {"-W": takesValue, "-X": takesValue, "-Q": takesValue, "-c": inlineCode, "-m": noScript},
	"ruby": {"-I": codePath, "-r": codePath, "-e": inlineCode, "-C": takesValue, "-E": takesValue,
		"-F": takesValue, "--encoding": takesValue},
	"node": {"-r": codePath, "--require": codePath, "--import": codePath, "--loader": codePath,
		"--experimental-loader": codePath, "-e": inlineCode, "--eval": inlineCode, "-p": inlineCode, "--print": inlineCode,
		"--env-file": takesValue, "--inspect-port": takesValue, "--title": takesValue},
	"perl": {"-I": codePath, "-M": takesValue, "-m": takesValue, "-e": inlineCode, "-E": inlineCode},
	"deno": {"--config": takesValue, "-c": takesValue, "--import-map": takesValue, "--cert": takesValue,
		"--lock": takesValue, "--env-file": takesValue},
	"bun": {"--preload": codePath, "-r": codePath, "--cwd": takesValue, "--config": takesValue, "-c": takesValue,
		"--env-file": takesValue},
}

// shLongValueFlags are the long options of the sh family that take a value.
var shLongValueFlags = map[string]bool{"--rcfile": true, "--init-file": true}

// interpreterRE matches an interpreter command name, a version suffix
// allowed (python3.12, ruby3.3, perl5.36): group 1 is the family.
var interpreterRE = regexp.MustCompile(`^(python|ruby|node|perl|sh|bash|dash|zsh|deno|bun)[0-9][0-9.]*$|^(python|ruby|node|perl|sh|bash|dash|zsh|deno|bun)$`)

// interpreterFamily is the interpreterFlags key for a command name, or ""
// when it is no interpreter lever knows.
func interpreterFamily(name string) string {
	m := interpreterRE.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	fam := m[1] + m[2]
	switch fam {
	case "bash", "dash", "zsh":
		return "sh"
	}
	return fam
}

// unwrapEnv skips an env(1) prefix (parseEnvPrefix) and returns the argv
// env runs and the directory env -C/--chdir makes it run in ("" =
// unchanged). Anything that does not start with env is returned unchanged;
// a prefix lever cannot read as env does gives nil (config load refuses it,
// envFormUnread).
func unwrapEnv(argv []string) ([]string, string) {
	p := parseEnvPrefix(argv)
	if p.unread != "" {
		return nil, ""
	}
	return p.argv, p.chdir
}

// envFormUnread names an env(1) prefix form whose words lever cannot read
// the way env does, or "" (parseEnvPrefix). A flag hidden in such a form
// would escape the host-path checks, so config load refuses the command
// instead of guessing.
func envFormUnread(argv []string) string { return parseEnvPrefix(argv).unread }

// envPrefix is what env(1) makes of a command line that starts with it.
type envPrefix struct {
	argv   []string // the command env runs
	chdir  string   // -C/--chdir, joined across nested envs ("" = unchanged)
	unread string   // a form lever cannot read as env does ("" = none)
}

// envLongOpts are GNU env's long options (getopt_long also takes any
// unambiguous prefix of one). BSD env has none.
var envLongOpts = []string{
	"block-signal", "chdir", "debug", "default-signal", "help", "ignore-environment",
	"ignore-signal", "list-signal-handling", "null", "split-string", "unset", "version",
}

// envSplitSpace is what GNU env -S splits on.
const envSplitSpace = " \t\n\v\f\r"

// parseEnvPrefix walks an env(1) prefix exactly as GNU and BSD env do (their
// getopt: options first, stopping at the first operand; then NAME=value
// words; then the command), nested envs included, and is the one reader
// behind unwrapEnv and envFormUnread:
//
//   - -i, -0, -v, alone or grouped (-iv), and "-" (= -i, which ends the
//     options; an option word right after it is unread);
//   - -u NAME, -uNAME, --unset NAME, --unset=NAME (the value is never an
//     option, so -uSSH_AUTH_SOCK unsets SSH_AUTH_SOCK);
//   - -C dir, -Cdir, --chdir dir, --chdir=dir;
//   - -S str, -Sstr, --split-string str, --split-string=str: str is split on
//     blanks as GNU does and its words parsed again as options, assignments
//     and the command. A string with quotes, a backslash, $ or a word
//     starting with # is unread (env unquotes, unescapes, expands and
//     comments those; lever does not);
//   - -S after other letters in one group (-iS, -vS) is unread: precise
//     getopt would take it, but such a group is refused rather than read;
//   - --block-signal, --default-signal, --ignore-signal (optional =SIG),
//     --debug, --ignore-environment, --null, --list-signal-handling;
//   - "--" ends the options; one "-" may follow (-i), then NAME=value
//     words (any word with "=", as env reads it), then the command.
//
// Any other option (BSD -P altpath, which changes where the program is
// looked up, -L, -U; --help and --version, which run nothing; an unknown or
// ambiguous long option), a missing option value and an env with no
// command are unread.
func parseEnvPrefix(argv []string) envPrefix {
	var p envPrefix
	for len(argv) > 0 && baseName(argv[0]) == "env" {
		rest, chdir, unread := parseOneEnv(argv[1:])
		if unread != "" {
			return envPrefix{unread: unread}
		}
		if chdir != "" {
			p.chdir = joinRaw(p.chdir, chdir)
		}
		argv = rest
	}
	p.argv = argv
	return p
}

// parseOneEnv is one env's words (after its argv[0]): the command it runs,
// its -C directory (several join), and an unread form or "".
func parseOneEnv(args []string) (cmd []string, chdir string, unread string) {
	setDir := func(d string) { chdir = joinRaw(chdir, d) }
	// value takes an option's value: glued, else the next word.
	value := func(glued string, has bool, i *int, opt string) (string, bool) {
		if has {
			return glued, true
		}
		if *i+1 >= len(args) {
			unread = "env " + opt + " without its value"
			return "", false
		}
		*i++
		return args[*i], true
	}
	// split inserts -S's words where it stood, to be read again.
	split := func(i int, str string) bool {
		if strings.ContainsAny(str, "\"'\\$") {
			unread = "an env -S string with quotes, backslashes or $"
			return false
		}
		words := strings.FieldsFunc(str, func(r rune) bool { return strings.ContainsRune(envSplitSpace, r) })
		for _, w := range words {
			if strings.HasPrefix(w, "#") {
				unread = "an env -S string with a # comment"
				return false
			}
		}
		args = append(words, args[i+1:]...)
		return true
	}
	i := 0
options:
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			i++
			break options
		case a == "-":
			// An operand: the options are over (handled below).
			break options
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			full := ""
			for _, o := range envLongOpts {
				if strings.HasPrefix(o, name) {
					if o == name {
						full = o
						break
					}
					if full != "" {
						return nil, "", "an ambiguous env option (" + a + ")"
					}
					full = o
				}
			}
			if name == "" || full == "" {
				return nil, "", "an unknown env option (" + a + ")"
			}
			switch full {
			case "unset":
				if _, ok := value(val, hasVal, &i, "--unset"); !ok {
					return nil, "", unread
				}
				i++
			case "chdir":
				d, ok := value(val, hasVal, &i, "--chdir")
				if !ok {
					return nil, "", unread
				}
				setDir(d)
				i++
			case "split-string":
				str, ok := value(val, hasVal, &i, "--split-string")
				if !ok || !split(i, str) {
					return nil, "", unread
				}
				i = 0
			case "block-signal", "default-signal", "ignore-signal":
				i++
			case "debug", "ignore-environment", "null", "list-signal-handling":
				if hasVal {
					return nil, "", "an env option with a value it does not take (" + a + ")"
				}
				i++
			default: // help, version: env runs nothing
				return nil, "", "env --" + full
			}
		case strings.HasPrefix(a, "-"):
			for j := 1; j < len(a); j++ {
				switch c := a[j]; c {
				case 'i', '0', 'v':
					continue
				case 'u', 'C', 'S':
					if c == 'S' && j > 1 {
						return nil, "", "a combined env option group with S (" + a + ")"
					}
					v, ok := value(a[j+1:], j+1 < len(a), &i, "-"+string(c))
					if !ok {
						return nil, "", unread
					}
					switch c {
					case 'C':
						setDir(v)
					case 'S':
						if !split(i, v) {
							return nil, "", unread
						}
						i = 0
						continue options
					}
				default:
					return nil, "", "an env option lever does not read (-" + string(c) + " in " + a + ")"
				}
				break
			}
			i++
		default:
			break options
		}
	}
	// GNU reads one "-" right after the options (after "--" too) as -i,
	// so a word after it is never an option there; BSD may go on reading
	// options, so refuse the case where that matters.
	if i < len(args) && args[i] == "-" {
		if i+1 < len(args) && strings.HasPrefix(args[i+1], "-") {
			return nil, "", "an env option after \"-\""
		}
		i++
	}
	for i < len(args) && strings.Contains(args[i], "=") {
		i++
	}
	if i >= len(args) {
		return nil, "", "an env with no command"
	}
	return args[i:], chdir, ""
}

// joinRaw is base/p without cleaning (so a ".." stays for the caller's
// refusal), p itself when it is absolute or base is empty.
func joinRaw(base, p string) string {
	if base == "" || strings.HasPrefix(p, "/") || p == "" {
		return p
	}
	return strings.TrimSuffix(base, "/") + "/" + p
}

// baseName is the last element of a slash-separated command name.
func baseName(cmd string) string { return cmd[strings.LastIndexByte(cmd, '/')+1:] }

// interpPath is one path an interpreter command line names.
type interpPath struct {
	what, path string
}

// interpreterPaths lists the code an interpreter command line runs, argv[0]
// being the interpreter and family its interpreterFlags key (or "sh",
// shPaths). Leading flags are read: a code-path flag's value is code; an
// inline-code flag's value is scanned for paths (codePaths) and ends the
// walk; otherwise the first non-flag argument is the script (after deno's
// or bun's "run"). A value-taking flag's value is checked too, so a value
// read as the script cannot hide the real one. A flag missing from the
// table that takes a value makes that value read as the script and hides
// the real one: the table is the limit of this best-effort reading. What
// follows the script is the script's own business and is not read.
func interpreterPaths(family string, argv []string) []interpPath {
	if family == "sh" {
		return shPaths(argv)
	}
	flags := interpreterFlags[family]
	var out []interpPath
	add := func(what, p string) {
		for _, x := range out {
			if x.path == p {
				return
			}
		}
		out = append(out, interpPath{what, p})
	}
	args := argv[1:]
	if (family == "deno" || family == "bun") && len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				add("script", args[i+1])
			}
			return out
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			add("script", a)
			return out
		}
		name, val, glued := a, "", false
		if strings.HasPrefix(a, "--") {
			name, val, glued = strings.Cut(a, "=")
		} else if len(a) > 2 {
			if _, ok := flags[a[:2]]; ok {
				name, val, glued = a[:2], a[2:], true
			}
		}
		role, ok := flags[name]
		if !ok {
			continue
		}
		if !glued {
			if i+1 >= len(args) {
				return out
			}
			i++
			val = args[i]
		}
		switch role {
		case codePath:
			add(name, val)
		case takesValue, noScript:
			add(name+" value", val)
			if role == noScript {
				return out
			}
		case inlineCode:
			for _, p := range codePaths(val) {
				add(name+" code", p)
			}
			return out
		}
	}
	return out
}

// shPaths reads a sh-family command line as the shell does: options are
// short groups after "-" or "+" (-euo, +e, -ec), long options, and
// "-o name"/"+o name" (an 'o' or 'O' in a group takes the next word); a
// 'c' anywhere in a "-" group makes the first operand the command string
// (never a glued value: -ce is -c -e). The first operand is otherwise the
// script. As a fallback, every word after a "-c" or a "-" group with a
// 'c' anywhere in argv is scanned as inline code, and every option value
// is checked as a path.
func shPaths(argv []string) []interpPath {
	var out []interpPath
	add := func(what, p string) {
		for _, x := range out {
			if x.path == p {
				return
			}
		}
		out = append(out, interpPath{what, p})
	}
	code := func(line string) {
		for _, p := range codePaths(line) {
			add("-c code", p)
		}
	}
	args := argv[1:]
	isGroupWithC := func(a string) bool {
		return len(a) > 1 && a[0] == '-' && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'c')
	}
	for i, a := range args {
		if isGroupWithC(a) && i+1 < len(args) {
			code(args[i+1])
		}
	}
	inline := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				if inline {
					code(args[i+1])
				} else {
					add("script", args[i+1])
				}
			}
			return out
		case strings.HasPrefix(a, "--"):
			name, val, glued := strings.Cut(a, "=")
			switch {
			case shLongValueFlags[name] && glued:
				add(name+" value", val)
			case shLongValueFlags[name] && i+1 < len(args):
				i++
				add(name+" value", args[i])
			}
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			for _, r := range a[1:] {
				switch r {
				case 'c':
					if a[0] == '-' {
						inline = true
					}
				case 'o', 'O':
					if i+1 < len(args) {
						i++
						add("-"+string(r)+" value", args[i])
					}
				}
			}
		default:
			if inline {
				code(a)
			} else {
				add("script", a)
			}
			return out
		}
	}
	return out
}

// codePaths are the paths in a line of inline code: it is split on
// whitespace, quotes and shell punctuation, '=', ':' and ','. A piece that
// starts with "/" counts, a relative one with a "/" in it counts (it
// resolves against the tool's working directory, the instance root), and a
// flag with a path glued on counts from its first "/" (-I/x).
func codePaths(line string) []string {
	var out []string
	sep := func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`=:,'"`+"`"+`;&|<>(){}`, r)
	}
	for _, tok := range strings.FieldsFunc(line, sep) {
		i := strings.IndexByte(tok, '/')
		if i < 0 {
			continue
		}
		p := tok
		if tok[0] == '-' {
			p = tok[i:]
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
