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

// unwrapEnv skips an env(1) prefix — its flags and NAME=value assignments —
// and returns the argv env runs, and the directory env -C/--chdir makes it
// run in ("" = unchanged). -S's value is split into words, as env does.
// Anything else is returned unchanged.
func unwrapEnv(argv []string) ([]string, string) {
	chdir := ""
	for len(argv) > 0 && baseName(argv[0]) == "env" {
		rest := argv[1:]
		for len(rest) > 0 {
			a := rest[0]
			switch {
			case a == "--":
				rest = rest[1:]
			case a == "-u" || a == "--unset":
				rest = rest[min(2, len(rest)):]
				continue
			case a == "-C" || a == "--chdir":
				if len(rest) < 2 {
					return nil, ""
				}
				chdir = joinRaw(chdir, rest[1])
				rest = rest[2:]
				continue
			case strings.HasPrefix(a, "--chdir="):
				chdir = joinRaw(chdir, strings.TrimPrefix(a, "--chdir="))
				rest = rest[1:]
				continue
			case a == "-S" || a == "--split-string":
				if len(rest) < 2 {
					return nil, ""
				}
				rest = append(strings.Fields(rest[1]), rest[2:]...)
				continue
			case strings.HasPrefix(a, "-S"):
				rest = append(strings.Fields(a[2:]), rest[1:]...)
				continue
			case strings.HasPrefix(a, "-"):
				rest = rest[1:]
				continue
			case strings.Contains(a, "=") && !strings.HasPrefix(a, "/"):
				rest = rest[1:]
				continue
			}
			break
		}
		argv = rest
	}
	return argv, chdir
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
			name, _, glued := strings.Cut(a, "=")
			if shLongValueFlags[name] && !glued && i+1 < len(args) {
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
