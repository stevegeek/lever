//go:build linux || darwin

package treebackup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// vcsLayouts plants the Mercurial and Jujutsu files that make those tools
// run a command in a repository they trust because the user owns it.
func vcsLayouts(marker string) *rtree {
	hgrc := "[hooks]\npre-status = touch " + marker + "\n[extensions]\nevil = ../evil.py\n"
	return newRTree().
		dir("hg", 0o755).dir("hg/.hg", 0o755).file("hg/.hg/requires", "store\n", 0o644).
		file("hg/.hg/hgrc", hgrc, 0o644).file("hg/.hg/hgrc-not-shared", hgrc, 0o644).file("hg/f", "f", 0o644).
		dir("share", 0o755).dir("share/.HG", 0o755).file("share/.HG/requires", "shared\n", 0o644).
		file("share/.HG/SharedPath", "/elsewhere/.hg\n", 0o644).
		dir("viaLink", 0o755).dir("viaLink/x", 0o755).file("viaLink/x/hgrc", hgrc, 0o644).link("viaLink/.hg", "x").
		dir("jj", 0o755).dir("jj/.jj", 0o755).dir("jj/.jj/repo", 0o755).file("jj/.jj/repo/config.toml", "[ui]\npager = \"touch "+marker+"\"\n", 0o644).
		dir("jjws", 0o755).dir("jjws/.jj", 0o755).file("jjws/.jj/repo", "../../jj/.jj/repo", 0o644).
		dir("plain", 0o755).file("plain/hgrc", "not hg", 0o644).file("plain/config.toml", "not jj", 0o644).
		dir("plain/repo", 0o755).file("plain/repo/config.toml", "not jj", 0o644)
}

func TestRestoreNeutralisesMercurialAndJujutsuControlFiles(t *testing.T) {
	base := t.TempDir()
	tr := vcsLayouts(filepath.Join(base, "marker"))
	target := filepath.Join(base, "out")
	o := tr.opts(target)
	o.Links = LinksAll
	rep := restore(t, o)
	renamed := []string{
		"hg/.hg/hgrc", "hg/.hg/hgrc-not-shared", "share/.HG/SharedPath", "viaLink/.hg",
		"jj/.jj/repo/config.toml", "jjws/.jj/repo",
	}
	for _, p := range renamed {
		mustNotExist(t, filepath.Join(target, p))
		if _, err := os.Lstat(filepath.Join(target, p+neutralSuffix)); err != nil {
			t.Errorf("%s not neutralised: %v", p, err)
		}
	}
	for _, p := range []string{"hg/.hg/requires", "plain/hgrc", "plain/config.toml", "plain/repo/config.toml", "jj/.jj/repo", "viaLink/x/hgrc"} {
		if _, err := os.Lstat(filepath.Join(target, p)); err != nil {
			t.Errorf("%s should keep its name: %v", p, err)
		}
	}
	if len(rep.Neutralised) != len(renamed) || !rep.OK() {
		t.Fatalf("neutralised %q, OK %v", rep.Neutralised, rep.OK())
	}
	if hg, err := exec.LookPath("hg"); err == nil {
		marker := filepath.Join(base, "marker")
		cmd := exec.Command(hg, "status")
		cmd.Dir = filepath.Join(target, "hg")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + base, "HGRCPATH="}
		out, _ := cmd.CombinedOutput()
		if _, err := os.Lstat(marker); err == nil {
			t.Fatalf("hg status in the restore ran the planted hook: %s", strings.TrimSpace(string(out)))
		}
	}
}
