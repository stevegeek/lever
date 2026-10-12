package config

import (
	"slices"
	"testing"
)

// parseEnvPrefix reads every env(1) form as GNU and BSD env do, and
// unwrapEnv and envFormUnread agree because both use it.
func TestParseEnvPrefix(t *testing.T) {
	tool := []string{"tool", "-state", "/tree/s"}
	for _, tc := range []struct {
		name   string
		argv   []string
		want   []string // the command env runs (nil when unread)
		chdir  string
		unread bool
	}{
		{"not env", []string{"tool", "-x"}, []string{"tool", "-x"}, "", false},
		{"plain", []string{"env", "tool", "-state", "/tree/s"}, tool, "", false},
		{"NAME=value", []string{"env", "A=1", "B=2", "tool", "-state", "/tree/s"}, tool, "", false},
		{"slash NAME=value is an assignment", []string{"env", "/a=b", "/tree/evil"}, []string{"/tree/evil"}, "", false},
		{"-i", []string{"env", "-i", "tool", "-state", "/tree/s"}, tool, "", false},
		{"-", []string{"env", "-", "A=1", "tool", "-state", "/tree/s"}, tool, "", false},
		{"- then an option", []string{"env", "-", "-u", "X", "tool"}, nil, "", true},
		{"-0 -v", []string{"env", "-0", "-v", "tool", "-state", "/tree/s"}, tool, "", false},
		{"-iv0 group", []string{"env", "-iv0", "tool", "-state", "/tree/s"}, tool, "", false},
		{"--", []string{"env", "-i", "--", "A=1", "tool", "-state", "/tree/s"}, tool, "", false},
		{"options stop at an assignment", []string{"env", "A=1", "-i", "tool"}, []string{"-i", "tool"}, "", false},

		{"-u NAME", []string{"env", "-u", "X", "tool", "-state", "/tree/s"}, tool, "", false},
		{"-uNAME", []string{"env", "-uSSH_AUTH_SOCK", "tool", "-state", "/tree/s"}, tool, "", false},
		{"-iu NAME", []string{"env", "-iu", "X", "tool", "-state", "/tree/s"}, tool, "", false},
		{"-iuNAME", []string{"env", "-iuX", "tool", "-state", "/tree/s"}, tool, "", false},
		{"--unset=NAME", []string{"env", "--unset=X", "tool", "-state", "/tree/s"}, tool, "", false},
		{"--unset NAME", []string{"env", "--unset", "X", "tool", "-state", "/tree/s"}, tool, "", false},
		{"-u missing value", []string{"env", "-u"}, nil, "", true},

		{"-C dir", []string{"env", "-C", "/w", "tool", "-state", "/tree/s"}, tool, "/w", false},
		{"-Cdir", []string{"env", "-C/Shared/x", "tool", "-state", "/tree/s"}, tool, "/Shared/x", false},
		{"--chdir=dir", []string{"env", "--chdir=ws", "tool", "-state", "/tree/s"}, tool, "ws", false},
		{"--chdir dir", []string{"env", "--chdir", "ws", "tool", "-state", "/tree/s"}, tool, "ws", false},
		{"--chd= prefix", []string{"env", "--chd=ws", "tool", "-state", "/tree/s"}, tool, "ws", false},
		{"-C twice joins", []string{"env", "-C", "/w", "-C", "sub", "tool", "-state", "/tree/s"}, tool, "/w/sub", false},
		{"-C missing value", []string{"env", "-C"}, nil, "", true},

		{"-S str", []string{"env", "-S", "tool -state /tree/s"}, tool, "", false},
		{"-Sstr", []string{"env", "-Stool -state /tree/s"}, tool, "", false},
		{"--split-string str", []string{"env", "--split-string", "tool -state /tree/s"}, tool, "", false},
		{"--split-string=str", []string{"env", "--split-string=tool -state /tree/s"}, tool, "", false},
		{"-S words are options again", []string{"env", "-S", "-u X -C /w A=1 tool -state /tree/s"}, tool, "/w", false},
		{"-S blanks", []string{"env", "-S", "tool\t-state\n/tree/s"}, tool, "", false},
		{"-u X then -S quoted (the bypass)", []string{"env", "-u", "X", "-S", `tool "-state /tree/s"`}, nil, "", true},
		{"-uX then -S quoted", []string{"env", "-uX", "-S", "tool '-state' /tree/s"}, nil, "", true},
		{"-C dir then -S $", []string{"env", "-C", "/w", "-S", "tool -state $HOME/s"}, nil, "", true},
		{"-S backslash", []string{"env", "-S", `tool \-state /tree/s`}, nil, "", true},
		{"-S comment", []string{"env", "-S", "tool #-state /tree/s"}, nil, "", true},
		{"-S missing value", []string{"env", "-S"}, nil, "", true},
		{"-iS group", []string{"env", "-iS", "tool -state /tree/s"}, nil, "", true},
		{"-vS group", []string{"env", "-vS", "tool -state /tree/s"}, nil, "", true},
		{"-vStr group", []string{"env", "-vStool"}, nil, "", true},

		{"signal options", []string{"env", "--ignore-signal", "--default-signal=PIPE", "--block-signal=INT", "tool", "-state", "/tree/s"}, tool, "", false},
		{"long flags", []string{"env", "--ignore-environment", "--null", "--debug", "tool", "-state", "/tree/s"}, tool, "", false},
		{"long flag with a value", []string{"env", "--null=1", "tool"}, nil, "", true},
		{"ambiguous long option", []string{"env", "--ignore", "tool"}, nil, "", true},
		{"unknown long option", []string{"env", "--frob", "tool"}, nil, "", true},
		{"--help", []string{"env", "--help"}, nil, "", true},
		{"BSD -P altpath", []string{"env", "-P", "/tree/bin", "tool"}, nil, "", true},
		{"unknown short option", []string{"env", "-x", "tool"}, nil, "", true},
		{"no command", []string{"env", "A=1"}, nil, "", true},

		{"nested env", []string{"/usr/bin/env", "-C", "/w", "env", "-u", "X", "-C", "sub", "tool", "-state", "/tree/s"}, tool, "/w/sub", false},
		{"nested env unread", []string{"env", "A=1", "env", "-vS", "tool"}, nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := parseEnvPrefix(tc.argv)
			if (p.unread != "") != tc.unread {
				t.Fatalf("unread = %q, want unread=%v", p.unread, tc.unread)
			}
			if !slices.Equal(p.argv, tc.want) || p.chdir != tc.chdir {
				t.Fatalf("argv=%q chdir=%q, want %q %q", p.argv, p.chdir, tc.want, tc.chdir)
			}
			// The two callers agree with the parser.
			got, dir := unwrapEnv(tc.argv)
			if !slices.Equal(got, tc.want) || dir != tc.chdir {
				t.Fatalf("unwrapEnv = %q %q, want %q %q", got, dir, tc.want, tc.chdir)
			}
			if (envFormUnread(tc.argv) != "") != tc.unread {
				t.Fatalf("envFormUnread disagrees with the parser")
			}
		})
	}
}
