//go:build linux

package treebackup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux casefold applies full case folding and ignores default ignorable
// code points: a ligature, a soft hyphen or a zero-width joiner in a git
// name must not keep it from being renamed.
func TestRestoreGitNamesOnACasefoldTarget(t *testing.T) {
	if !inUserns(t, "TestRestoreGitNamesOnACasefoldTarget") {
		return
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	base := t.TempDir()
	mnt := filepath.Join(base, "cf")
	os.Mkdir(mnt, 0o755)
	if err := unix.Mount("tmpfs", mnt, "tmpfs", 0, "casefold=utf8-12.1.0"); err != nil {
		t.Skipf("casefold tmpfs: %v", err)
	}
	defer unix.Unmount(mnt, unix.MNT_DETACH)
	parent := filepath.Join(mnt, "restores")
	os.Mkdir(parent, 0o755)
	fd, _ := unix.Open(parent, unix.O_RDONLY, 0)
	var fl int
	unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.FS_IOC_GETFLAGS, uintptr(unsafe.Pointer(&fl)))
	fl |= 0x40000000
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.FS_IOC_SETFLAGS, uintptr(unsafe.Pointer(&fl))); e != 0 {
		t.Skipf("setflags: %v", e)
	}
	unix.Close(fd)
	marker := filepath.Join(base, "PWNED")
	cfg := "[core]\n\trepositoryformatversion = 0\n\tfsmonitor = \"touch " + marker + "; false\"\n"
	tr := newRTree().dir("p", 0o755).dir("p/.git", 0o755).file("p/.git/HEAD", "ref: refs/heads/master\n", 0o644).
		file("p/.git/con\ufb01g", cfg, 0o644).dir("p/.git/objects", 0o755).dir("p/.git/refs", 0o755).file("p/f", "f", 0o644).
		dir("g", 0o755).dir("g/r", 0o755).file("g/r/HE\u00adAD", "ref: refs/heads/master\n", 0o644).file("g/r/con\u200bfig", cfg, 0o644).
		dir("g/r/objects", 0o755).dir("g/r/refs", 0o755).dir("g/w", 0o755).file("g/w/.gi\u200dt", "gitdir: ../r\n", 0o644).file("g/w/f", "f", 0o644)
	target := filepath.Join(parent, "out")
	rep := restore(t, tr.opts(target))
	if len(rep.Neutralised) != 3 {
		t.Errorf("neutralised %q", rep.Neutralised)
	}
	home := filepath.Join(base, "home")
	os.Mkdir(home, 0o700)
	cmd := exec.Command(gitBin, "status", "--porcelain")
	cmd.Dir = filepath.Join(target, "p")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES=" + base}
	out, _ := cmd.CombinedOutput()
	t.Logf("git: %s", out)
	if _, err := os.Lstat(marker); err == nil {
		t.Error("git ran the planted command through con\\ufb01g")
	}
	os.Remove(marker)
	cmd = exec.Command(gitBin, "status", "--porcelain")
	cmd.Dir = filepath.Join(target, "g/w")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES=" + base}
	out, _ = cmd.CombinedOutput()
	t.Logf("git g/w: %s", out)
	if _, err := os.Lstat(marker); err == nil {
		t.Error("git ran the planted command through .gi\\u200dt -> HE\\u00adAD/con\\u200bfig")
	}
}
