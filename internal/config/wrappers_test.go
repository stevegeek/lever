package config

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

// The command wrappers lever reads are walked like env, nested in any order;
// a form lever cannot read is unread.
func TestParseWrappers(t *testing.T) {
	py := []string{"python3", "/tree/x.py"}
	for _, tc := range []struct {
		name   string
		argv   []string
		want   []string
		chdir  string
		paths  []string
		unread bool
	}{
		{"nice", []string{"nice", "python3", "/tree/x.py"}, py, "", nil, false},
		{"nice -n", []string{"nice", "-n", "5", "python3", "/tree/x.py"}, py, "", nil, false},
		{"nice -n5", []string{"nice", "-n5", "python3", "/tree/x.py"}, py, "", nil, false},
		{"nice --adjustment=5", []string{"nice", "--adjustment=5", "python3", "/tree/x.py"}, py, "", nil, false},
		{"nice --adj 5", []string{"nice", "--adj", "5", "python3", "/tree/x.py"}, py, "", nil, false},
		{"nice -10", []string{"nice", "-10", "python3", "/tree/x.py"}, py, "", nil, false},
		{"nice --", []string{"nice", "--", "python3", "/tree/x.py"}, py, "", nil, false},
		{"nice no command", []string{"nice"}, nil, "", nil, true},
		{"nice --help", []string{"nice", "--help"}, nil, "", nil, true},
		{"nice unknown option", []string{"nice", "-x", "python3"}, nil, "", nil, true},
		{"nice -n no value", []string{"nice", "-n"}, nil, "", nil, true},
		{"nohup", []string{"/usr/bin/nohup", "python3", "/tree/x.py"}, py, "", nil, false},
		{"setsid -fw", []string{"setsid", "-fw", "python3", "/tree/x.py"}, py, "", nil, false},
		{"timeout", []string{"timeout", "-k", "5", "--signal=TERM", "60", "python3", "/tree/x.py"}, py, "", nil, false},
		{"timeout -s9 -v", []string{"timeout", "-s9", "-v", "1m", "python3", "/tree/x.py"}, py, "", nil, false},
		{"timeout no duration", []string{"timeout"}, nil, "", nil, true},
		{"timeout no command", []string{"timeout", "60"}, nil, "", nil, true},
		{"stdbuf", []string{"stdbuf", "-oL", "-e", "0", "python3", "/tree/x.py"}, py, "", nil, false},
		{"ionice", []string{"ionice", "-c", "3", "-t", "python3", "/tree/x.py"}, py, "", nil, false},
		{"ionice -p", []string{"ionice", "-p", "1"}, nil, "", nil, true},
		{"taskset mask", []string{"taskset", "0x3", "python3", "/tree/x.py"}, py, "", nil, false},
		{"taskset -c list", []string{"taskset", "-c", "0,1", "python3", "/tree/x.py"}, py, "", nil, false},
		{"taskset -p", []string{"taskset", "-p", "1"}, nil, "", nil, true},
		{"nested", []string{"env", "-C", "/w", "nice", "-n", "1", "env", "A=1", "timeout", "5", "python3", "/tree/x.py"}, py, "/w", nil, false},
		{"unread inside nesting", []string{"nice", "env", "-vS", "x"}, nil, "", nil, true},

		{"uv run script", []string{"uv", "run", "/tree/x.py", "-a"}, []string{"python", "--", "/tree/x.py", "-a"}, "", nil, false},
		{"uv run --script", []string{"uv", "run", "--script", "tool"}, []string{"python", "--", "tool"}, "", nil, false},
		{"uv run python", []string{"uv", "run", "--frozen", "python3", "/tree/x.py"}, py, "", nil, false},
		{"uv run -m", []string{"uv", "run", "-m", "pkg.mod"}, []string{"python", "-m", "pkg.mod"}, "", nil, false},
		{"uv run a command", []string{"uv", "run", "--no-sync", "./bin/tool"}, []string{"./bin/tool"}, "", nil, false},
		{"uv run --", []string{"uv", "run", "--", "python3", "/tree/x.py"}, py, "", nil, false},
		{"uv run code flags", []string{"uv", "run", "--with", "/tree/pkg", "--project=/tree/proj", "--with-requirements", "r.txt", "x.py"},
			[]string{"python", "--", "x.py"}, "", []string{"/tree/pkg", "/tree/proj", "r.txt"}, false},
		{"uv --directory", []string{"uv", "--directory", "/tree/p", "run", "--with", "lib", "x.py"},
			[]string{"python", "--", "x.py"}, "/tree/p", []string{"/tree/p", "/tree/p/lib"}, false},
		{"uv run -p glued", []string{"uv", "run", "-p3.12", "x.py"}, []string{"python", "--", "x.py"}, "", []string{"3.12"}, false},
		{"env -C then uv code path", []string{"env", "-C", "/w", "uv", "run", "--with", "lib", "x.py"},
			[]string{"python", "--", "x.py"}, "/w", []string{"/w/lib"}, false},
		{"uv unknown option", []string{"uv", "run", "--frob", "x", "x.py"}, nil, "", nil, true},
		{"uv grouped shorts", []string{"uv", "run", "-qv", "x.py"}, nil, "", nil, true},
		{"uv tool run", []string{"uv", "tool", "run", "x"}, nil, "", nil, true},
		{"uv no run", []string{"uv", "--offline"}, nil, "", nil, true},
		{"uv run no command", []string{"uv", "run", "--frozen"}, nil, "", nil, true},
		{"uv value missing", []string{"uv", "run", "--with"}, nil, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := parseEnvPrefix(tc.argv)
			if (p.unread != "") != tc.unread {
				t.Fatalf("unread = %q, want unread=%v", p.unread, tc.unread)
			}
			if !slices.Equal(p.argv, tc.want) || p.chdir != tc.chdir {
				t.Fatalf("argv=%q chdir=%q, want %q %q", p.argv, p.chdir, tc.want, tc.chdir)
			}
			var paths []string
			for _, x := range p.paths {
				paths = append(paths, x.path)
			}
			if !slices.Equal(paths, tc.paths) {
				t.Fatalf("paths = %q, want %q", paths, tc.paths)
			}
		})
	}
}

// A program or script in the tree is refused behind a wrapper too, and a
// form lever cannot read is refused at load.
func TestHostPathWrappersRefused(t *testing.T) {
	for _, cmd := range []string{
		"[nice, -n, \"5\", python3, ws/tools/bin]",
		"[nice, ws/tools/bin]",
		"[timeout, \"60\", ROOT/ws/tools/bin]",
		"[env, -C, ws, nice, python3, tools/bin]",
		"[pypy3, ws/tools/bin]",
		"[nodejs, ws/tools/bin]",
		"[uv, run, ws/tools/bin.py]",
		"[uv, run, python3, ws/tools/bin]",
		"[uv, run, --project, ws/tools, tool]",
		"[uv, run, --with, ROOT/ws/tools, tool]",
		"[uv, --directory, ws, run, tools/bin.py]",
		"[uv, run, --cache-dir, ws/tools, x.py]",
		"[uv, run, --find-links=ws/tools, x.py]",
	} {
		p, root := hostPathsInstance(t, "")
		mustWrite(t, root+"/ws/tools/bin.py")
		body := "name: demo\nbackend: orbstack\ntree: ws\nmanager: {}\n" + supervisedTool(strings.ReplaceAll(cmd, "ROOT", root))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadNoHostChecks(p)
		testutil.WantErrContaining(t, err, "inside the mounted tree")
	}
	for _, cmd := range []string{
		"[nice, --frob, python3, x]",
		"[timeout, \"60\"]",
		"[uv, tool, run, x]",
		"[uv, run, --unknown-flag, v, x.py]",
	} {
		p, _ := hostPathsInstance(t, "manager: {}\n"+supervisedTool(cmd))
		_, err := LoadNoHostChecks(p)
		testutil.WantErrContaining(t, err, "cannot read the way env or the wrapper does")
	}
}

// Behind a wrapper, a script under read_only is still allowed.
func TestHostPathWrapperScriptUnderReadOnly(t *testing.T) {
	p, _ := hostPathsInstance(t, "manager:\n  read_only: [tools]\n"+supervisedTool("[nice, -n, \"5\", uv, run, --frozen, python3, ws/tools/bin]"))
	if _, err := LoadNoHostChecks(p); err != nil {
		t.Fatalf("a wrapped script under read_only refused: %v", err)
	}
}
