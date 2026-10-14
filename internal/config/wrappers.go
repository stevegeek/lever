package config

import (
	"regexp"
	"strings"
)

// A command wrapper's own options must be read to find the command it runs,
// or the real program would escape the host-path checks. parseEnvPrefix
// reads the wrappers below exactly as their getopt does; a form it cannot
// read is unread, and config load refuses the command, as for env. A wrapper
// not in the table is taken as the program itself.

type wrapperSpec struct {
	short    string          // getopt letters; ':' after one takes a value
	long     map[string]bool // true when it takes a value
	operands int             // its own operands before the command (timeout's duration)
	numeric  bool            // accepts nice's old "-N" form
}

// wrapperSpecs omit --help, --version and the options that act on running
// processes (ionice -p/-P/-u, taskset -p): they run no command, so they
// must read as unread.
var wrapperSpecs = map[string]wrapperSpec{
	"nice":    {short: "n:", long: map[string]bool{"adjustment": true}, numeric: true},
	"nohup":   {},
	"setsid":  {short: "cfw", long: map[string]bool{"ctty": false, "fork": false, "wait": false}},
	"timeout": {short: "k:s:vfp", long: map[string]bool{"kill-after": true, "signal": true, "verbose": false, "foreground": false, "preserve-status": false}, operands: 1},
	"stdbuf":  {short: "i:o:e:", long: map[string]bool{"input": true, "output": true, "error": true}},
	"ionice":  {short: "c:n:t", long: map[string]bool{"class": true, "classdata": true, "ignore": false}},
	"taskset": {short: "ac", long: map[string]bool{"all-tasks": false, "cpu-list": false}, operands: 1},
}

var numericOptionRE = regexp.MustCompile(`^-[0-9]+$`)

func parseWrapper(name string, spec wrapperSpec, args []string) (cmd []string, unread string) {
	i := 0
	if spec.numeric && i < len(args) && numericOptionRE.MatchString(args[i]) {
		i++
	}
options:
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			i++
			break options
		case strings.HasPrefix(a, "--"):
			opt, _, hasVal := strings.Cut(a[2:], "=")
			full, takes, ok := longOption(spec.long, opt)
			if !ok {
				return nil, "an option " + name + " lever does not read (" + a + ")"
			}
			switch {
			case takes && !hasVal:
				if i+1 >= len(args) {
					return nil, name + " --" + full + " without its value"
				}
				i += 2
			case !takes && hasVal:
				return nil, "a " + name + " option with a value it does not take (" + a + ")"
			default:
				i++
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				k := strings.IndexByte(spec.short, a[j])
				if k < 0 || a[j] == ':' {
					return nil, "an option " + name + " lever does not read (-" + string(a[j]) + " in " + a + ")"
				}
				if k+1 < len(spec.short) && spec.short[k+1] == ':' {
					if j+1 == len(a) {
						if i+1 >= len(args) {
							return nil, name + " -" + string(a[j]) + " without its value"
						}
						i++
					}
					break
				}
			}
			i++
		default:
			break options
		}
	}
	i += spec.operands
	if i >= len(args) {
		return nil, "a " + name + " with no command"
	}
	return args[i:], ""
}

// longOption resolves a long option as getopt_long does, unambiguous
// prefixes included.
func longOption(opts map[string]bool, name string) (full string, takesValue, ok bool) {
	if name == "" {
		return "", false, false
	}
	if v, exact := opts[name]; exact {
		return name, v, true
	}
	for o, v := range opts {
		if strings.HasPrefix(o, name) {
			if full != "" {
				return "", false, false // ambiguous
			}
			full, takesValue = o, v
		}
	}
	return full, takesValue, full != ""
}

// uvRunCodeFlags name code uv installs or runs, checked as programs. Any
// option missing from these tables is unread: its value would otherwise be
// read as the command.
var (
	uvRunValueFlags = map[string]bool{
		"--extra": true, "--no-extra": true, "--group": true, "--no-group": true, "--only-group": true,
		"--with": true, "--with-editable": true, "--with-requirements": true, "--env-file": true,
		"--package": true, "--python": true, "-p": true, "--index": true, "--default-index": true,
		"--index-url": true, "-i": true, "--extra-index-url": true, "--find-links": true, "-f": true,
		"--index-strategy": true, "--keyring-provider": true, "--resolution": true, "--prerelease": true,
		"--exclude-newer": true, "--fork-strategy": true, "--link-mode": true, "--upgrade-package": true,
		"-P": true, "--reinstall-package": true, "--no-binary-package": true, "--no-build-package": true,
		"--config-setting": true, "-C": true, "--refresh-package": true, "--cache-dir": true,
		"--config-file": true, "--directory": true, "--project": true, "--python-preference": true,
		"--color": true, "--allow-insecure-host": true,
	}
	uvRunCodeFlags = map[string]bool{
		"--with": true, "--with-editable": true, "--with-requirements": true, "--find-links": true, "-f": true,
		"--directory": true, "--project": true, "--config-file": true, "--env-file": true, "--python": true, "-p": true,
		// Package sources and the cache uv installs from: a local path there is code.
		"--index": true, "--default-index": true, "--index-url": true, "-i": true, "--extra-index-url": true,
		"--cache-dir": true,
	}
	uvRunBoolFlags = map[string]bool{
		"--all-extras": true, "--no-default-groups": true, "--all-groups": true, "--no-dev": true,
		"--dev": true, "--only-dev": true, "--editable": true, "--no-editable": true, "--exact": true,
		"--no-env-file": true, "--isolated": true, "--active": true, "--no-active": true, "--no-sync": true,
		"--locked": true, "--frozen": true, "--no-project": true, "--all-packages": true,
		"--no-build-isolation": true, "--no-build": true, "--no-binary": true, "--compile-bytecode": true,
		"--reinstall": true, "--upgrade": true, "-U": true, "--refresh": true, "--no-cache": true,
		"-n": true, "--offline": true, "--native-tls": true, "--no-progress": true, "--quiet": true,
		"-q": true, "--verbose": true, "-v": true, "--no-config": true, "--no-python-downloads": true,
		"--managed-python": true, "--no-managed-python": true, "--preview": true, "--no-sources": true,
		"--no-index": true,
	}
)

// parseUvRun reads `uv [options] run [options] command…` into the command it
// amounts to (a script becomes `python <script>`). Not read: UV_* variables
// an env prefix sets, and where a bare command after `uv run` resolves (the
// project's virtual environment, not the supervisor's PATH).
func parseUvRun(args []string) (cmd []string, chdir string, paths []interpPath, unread string) {
	seenRun, script, module := false, false, false
	i := 0
	for i < len(args) {
		a := args[i]
		if !seenRun {
			if a == "run" {
				seenRun = true
				i++
				continue
			}
			if !strings.HasPrefix(a, "-") {
				return nil, "", nil, "a uv command other than run (" + a + ")"
			}
		}
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		name, val, glued := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") && len(a) > 2 {
			// A short option with its value glued on (-p3.12, -Cx=y).
			name, val, glued = a[:2], a[2:], true
		}
		switch {
		case seenRun && (name == "--script" || name == "-s" || name == "--gui-script") && !glued:
			script = true
			i++
		case seenRun && (name == "--module" || name == "-m") && !glued:
			module = true
			i++
		case uvRunBoolFlags[name] && !glued:
			i++
		case uvRunValueFlags[name]:
			if !glued {
				if i+1 >= len(args) {
					return nil, "", nil, "uv " + name + " without its value"
				}
				i++
				val = args[i]
			}
			i++
			if uvRunCodeFlags[name] {
				paths = append(paths, interpPath{"uv " + name + " value", val})
			}
			if name == "--directory" {
				chdir = joinRaw(chdir, val)
			}
		default:
			return nil, "", nil, "an option uv run lever does not read (" + a + ")"
		}
	}
	if !seenRun {
		return nil, "", nil, "a uv command other than run"
	}
	// uv changes to --directory before anything else, wherever the option
	// stands, so every other relative code path resolves against it.
	for k := range paths {
		if paths[k].what != "uv --directory value" {
			paths[k].path = joinRaw(chdir, paths[k].path)
		}
	}
	if i >= len(args) {
		return nil, "", nil, "a uv run with no command"
	}
	rest := args[i:]
	switch {
	case module:
		return append([]string{"python", "-m"}, rest...), chdir, paths, ""
	case script || strings.HasSuffix(rest[0], ".py") || strings.HasSuffix(rest[0], ".pyw"):
		return append([]string{"python", "--"}, rest...), chdir, paths, ""
	}
	return rest, chdir, paths, ""
}
