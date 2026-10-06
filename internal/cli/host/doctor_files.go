package host

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/fileledger"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/state"
)

// checkFiles reports remote.files: off; on but without a record (state
// inside the tree); the files ledger unsafe; an agent's exchange directory
// behind a symbolic link (every upload to it is refused); or on, with the
// limits.
func checkFiles(app *config.App, st state.State) checkResult {
	const name = "files"
	if !app.FilesOn() {
		return checkResult{name, true, "off (no upload or download routes; contact_files and share_file answer off)", ""}
	}
	if brokerctl.StateInsideTree(app, st) {
		return checkResult{name, false, "on, but the state directory is inside the tree: no record can be kept, so every upload, share and download is refused",
			"point `tree:` at a subdirectory that does not contain " + stateDirName() + "/"}
	}
	detail := fmt.Sprintf("on: max %d MiB, types %s; agents need an image with this release's lever-agent (contact_files, share_file)",
		app.EffectiveFilesMaxBytes()>>20, strings.Join(app.EffectiveFilesExtensions(), ","))
	p := st.FilesLedger()
	if fi, err := os.Lstat(p); err == nil {
		switch {
		case !fi.IsDir():
			return checkResult{name, false, "the files ledger " + stateRel(st, p) + " is not a directory (a symlink?)", "remove " + p}
		case fi.Mode().Perm()&0o022 != 0:
			return checkResult{name, false, fmt.Sprintf("the files ledger %s is %v: another user can add a record", stateRel(st, p), fi.Mode().Perm()), "chmod 700 " + p}
		}
		if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
			return checkResult{name, false, fmt.Sprintf("the files ledger %s belongs to uid %d, not to you", stateRel(st, p), owner), "remove " + p}
		}
		if bad := unsafeFilesLedgerFile(p); bad != "" {
			return checkResult{name, false, "the files ledger file " + stateRel(st, bad) + " is not a private regular file of yours: that agent's uploads, shares and downloads are refused",
				"chmod 600 " + bad}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return checkResult{name, false, "cannot read the files ledger: " + err.Error(), "check " + stateRel(st, p)}
	}
	names := slices.Sorted(maps.Keys(app.AgentWorkspaces()))
	var usage []string
	for _, agent := range names {
		ws := app.AgentWorkspaces()[agent]
		bytes, link := exchangeUsage(app.Tree, ws)
		if link != "" {
			return checkResult{name, false, fmt.Sprintf("%s's exchange has a symbolic link at %s: uploads to %s are refused", agent, link, agent),
				"remove the link at " + filepath.Join(app.Tree, link) + " (the agent made it; lever creates the directory again)"}
		}
		if bytes > 0 {
			usage = append(usage, fmt.Sprintf("%s %s", agent, byteText(bytes)))
		}
	}
	used := "no uploads stored"
	if len(usage) > 0 {
		used = "uploads stored: " + strings.Join(usage, ", ")
	}
	return checkResult{name, true, detail + "; ledger " + stateRel(st, p) + "/ (0700); " + used +
		" (lever never removes them: delete old ones from .lever-files/in/ by hand)", ""}
}

// exchangeUsage is the bytes of the regular files in ws's
// .lever-files/in/<key>/ directories, and the tree-relative path of the
// first symbolic link it meets at .lever-files, in, out or an in/<key>
// ("" for none). Nothing is followed: every entry is Lstat-ed.
func exchangeUsage(tree, ws string) (int64, string) {
	dir := path.Join(ws, chatfiles.Dir)
	for _, rel := range []string{dir, path.Join(dir, "in"), path.Join(dir, "out")} {
		if _, err := fsutil.StatInTreeNoLinks(tree, rel); errors.Is(err, fsutil.ErrSymlink) {
			return 0, rel
		}
	}
	in := path.Join(dir, "in")
	if fi, err := fsutil.StatInTreeNoLinks(tree, in); err != nil || !fi.IsDir() {
		return 0, ""
	}
	keys, err := os.ReadDir(filepath.Join(tree, filepath.FromSlash(in)))
	if err != nil {
		return 0, ""
	}
	var total int64
	for _, k := range keys {
		rel := path.Join(in, k.Name())
		fi, err := os.Lstat(filepath.Join(tree, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return 0, rel
		}
		if !fi.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(tree, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, f := range files {
			if fi, err := os.Lstat(filepath.Join(tree, filepath.FromSlash(rel), f.Name())); err == nil && fi.Mode().IsRegular() {
				total += fi.Size()
			}
		}
	}
	return total, ""
}

// byteText is n bytes for people.
func byteText(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

// unsafeFilesLedgerFile is the first agent file in dir (or its .1), or the
// .lock both writers share, that is not a regular file, is writable by
// others, or belongs to another user.
func unsafeFilesLedgerFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !fileledger.IsAgentFile(e.Name()) && e.Name() != ".lock" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
			return p
		}
		if owner, ok := fileOwner(fi); ok && owner != os.Getuid() {
			return p
		}
	}
	return ""
}
