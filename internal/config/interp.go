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
	"python": {"-W": takesValue, "-X": takesValue, "-c": inlineCode, "-m": noScript},
	"ruby": {"-I": codePath, "-r": codePath, "-e": inlineCode, "-C": takesValue, "-E": takesValue,
		"-F": takesValue, "--encoding": takesValue},
	"node": {"-r": codePath, "--require": codePath, "--import": codePath, "--loader": codePath,
		"--experimental-loader": codePath, "-e": inlineCode, "--eval": inlineCode, "-p": inlineCode, "--print": inlineCode},
	"perl": {"-I": codePath, "-M": takesValue, "-m": takesValue, "-e": inlineCode, "-E": inlineCode},
	"sh":   {"-c": inlineCode, "-o": takesValue, "-O": takesValue},
	"deno": {},
	"bun":  {},
}

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
// and returns the argv env runs. -S's value is split into words, as env
// does. Anything else is returned unchanged.
func unwrapEnv(argv []string) []string {
	for len(argv) > 0 && baseName(argv[0]) == "env" {
		rest := argv[1:]
		for len(rest) > 0 {
			a := rest[0]
			switch {
			case a == "--":
				rest = rest[1:]
			case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
				rest = rest[min(2, len(rest)):]
				continue
			case a == "-S" || a == "--split-string":
				if len(rest) < 2 {
					return nil
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
	return argv
}

// baseName is the last element of a slash-separated command name.
func baseName(cmd string) string { return cmd[strings.LastIndexByte(cmd, '/')+1:] }

// interpPath is one path an interpreter command line names.
type interpPath struct {
	what, path string
}

// interpreterPaths lists the code an interpreter command line runs:
// argv[0] is the interpreter, family its interpreterFlags key. Leading
// flags are read (a combined short group such as -ec counts for sh's -c);
// a code-path flag's value is code; an inline-code flag's value is scanned
// for paths (codePaths) and ends the walk; otherwise the first non-flag
// argument is the script (after deno's or bun's "run"). What follows the
// script is the script's own business and is not read.
func interpreterPaths(family string, argv []string) []interpPath {
	flags := interpreterFlags[family]
	var out []interpPath
	args := argv[1:]
	if (family == "deno" || family == "bun") && len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				out = append(out, interpPath{"script", args[i+1]})
			}
			return out
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			return append(out, interpPath{"script", a})
		}
		name, val, glued := a, "", false
		if strings.HasPrefix(a, "--") {
			name, val, glued = strings.Cut(a, "=")
		} else if len(a) > 2 {
			if _, ok := flags[a[:2]]; ok {
				name, val, glued = a[:2], a[2:], true
			} else if family == "sh" && strings.ContainsRune(a[1:], 'c') {
				name = "-c" // a combined group: -ec, -xc
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
			out = append(out, interpPath{name, val})
		case inlineCode:
			for _, p := range codePaths(val) {
				out = append(out, interpPath{name + " code", p})
			}
			return out
		case noScript:
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
