//go:build linux || darwin

package treebackup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitLayouts builds the gitdir layouts an agent can plant: a plain .git
// directory, a gitlink file into a gitdir whose commondir points at a
// directory with no HEAD, and a .git directory with such a commondir.
// Every config asks git to run a command that creates marker.
func gitLayouts(marker string) *rtree {
	cfg := "[core]\n\trepositoryformatversion = 0\n\tfsmonitor = \"touch " + marker + "; false\"\n"
	head := "ref: refs/heads/master\n"
	return newRTree().
		dir("plain", 0o755).dir("plain/.git", 0o755).file("plain/.git/HEAD", head, 0o644).
		file("plain/.git/config", cfg, 0o644).dir("plain/.git/objects", 0o755).dir("plain/.git/refs", 0o755).
		dir("plain/.git/hooks", 0o755).file("plain/.git/hooks/post-checkout", "#!/bin/sh\ntouch "+marker+"\n", 0o755).
		file("plain/f", "f", 0o644).
		dir("r", 0o755).dir("r/c", 0o755).file("r/c/config", cfg, 0o644).dir("r/c/objects", 0o755).dir("r/c/refs", 0o755).
		dir("r/w", 0o755).file("r/w/HEAD", head, 0o644).file("r/w/commondir", "../c\n", 0o644).
		dir("r/work", 0o755).file("r/work/.git", "gitdir: ../w\n", 0o644).file("r/work/f", "f", 0o644).
		dir("r2", 0o755).dir("r2/c", 0o755).file("r2/c/config", cfg, 0o644).dir("r2/c/objects", 0o755).dir("r2/c/refs", 0o755).
		dir("r2/work", 0o755).dir("r2/work/.git", 0o755).file("r2/work/.git/HEAD", head, 0o644).
		file("r2/work/.git/commondir", "../../c\n", 0o644).file("r2/work/f", "f", 0o644).
		dir("case", 0o755).dir("case/G", 0o755).file("case/G/head", head, 0o644).file("case/G/Config", cfg, 0o644).
		file("case/G/CONFIG.WORKTREE", cfg, 0o644).file("case/G/CommonDir", "x", 0o644).dir("case/G/HOOKS", 0o755).
		file("case/.GIT", "gitdir: G\n", 0o644).dir("case/.Git", 0o755).
		dir("app", 0o755).file("app/config", "not git", 0o644).file("app/hooks", "not git", 0o644)
}

func TestRestoreNeutralisesGitControlFiles(t *testing.T) {
	base := t.TempDir()
	tr := gitLayouts(filepath.Join(base, "marker"))
	target := filepath.Join(base, "out")
	rep := restore(t, tr.opts(target))
	renamed := []string{
		"plain/.git/config", "plain/.git/hooks", "r/w/commondir", "r/work/.git", "r2/work/.git/commondir",
		"case/G/Config", "case/G/CONFIG.WORKTREE", "case/G/CommonDir", "case/G/HOOKS", "case/.GIT",
	}
	for _, p := range renamed {
		mustNotExist(t, filepath.Join(target, p))
		if _, err := os.Lstat(filepath.Join(target, p+neutralSuffix)); err != nil {
			t.Errorf("%s not neutralised: %v", p, err)
		}
	}
	for _, p := range []string{"r/c/config", "r2/c/config", "app/config", "app/hooks", "case/.Git", "plain/.git/HEAD"} {
		if _, err := os.Lstat(filepath.Join(target, p)); err != nil {
			t.Errorf("%s should keep its name: %v", p, err)
		}
	}
	if len(rep.Neutralised) != len(renamed) || !rep.OK() {
		t.Fatalf("neutralised %q, OK %v", rep.Neutralised, rep.OK())
	}

	o := tr.opts(filepath.Join(base, "kept"))
	o.KeepGit = true
	rep = restore(t, o)
	if len(rep.Neutralised) != 0 {
		t.Fatalf("KeepGit neutralised %q", rep.Neutralised)
	}
	if _, err := os.Lstat(filepath.Join(base, "kept/r/w/commondir")); err != nil {
		t.Fatal(err)
	}
}

// git status in the restored layouts must run nothing; the same layouts
// restored with KeepGit do run the planted command, which shows the
// layouts are live.
func TestRestoreGitStatusInRestoredTreeRunsNothing(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	os.Mkdir(home, 0o700)
	for _, keep := range []bool{false, true} {
		name := "neutral"
		if keep {
			name = "kept"
		}
		marker := filepath.Join(base, name+"-marker")
		tr := gitLayouts(marker)
		target := filepath.Join(base, name)
		o := tr.opts(target)
		o.KeepGit = keep
		o.KeepModes = true
		restore(t, o)
		for _, dir := range []string{"plain", "r/work", "r2/work"} {
			os.Remove(marker)
			cmd := exec.Command(gitBin, "status", "--porcelain")
			cmd.Dir = filepath.Join(target, dir)
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1",
				"GIT_CEILING_DIRECTORIES=" + base, "LC_ALL=C",
			}
			out, _ := cmd.CombinedOutput()
			_, err := os.Lstat(marker)
			ran := err == nil
			if ran != keep {
				t.Errorf("%s/%s: planted command ran=%v, want %v; git said: %s", name, dir, ran, keep,
					strings.TrimSpace(string(out)))
			}
		}
	}
}

func TestRestoreGitNeutralisationWithPathSubset(t *testing.T) {
	base := t.TempDir()
	tr := gitLayouts(filepath.Join(base, "marker"))
	o := tr.opts(filepath.Join(base, "out"))
	o.Paths = [][]byte{[]byte("r/w/commondir"), []byte("plain/.git/hooks/post-checkout")}
	rep := restore(t, o)
	for _, p := range []string{"r/w/commondir" + neutralSuffix, "plain/.git/hooks" + neutralSuffix + "/post-checkout"} {
		if _, err := os.Lstat(filepath.Join(o.Target, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if len(rep.Neutralised) != 2 {
		t.Fatalf("neutralised %q", rep.Neutralised)
	}
}
