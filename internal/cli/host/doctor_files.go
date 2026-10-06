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
	for _, agent := range names {
		ws := app.AgentWorkspaces()[agent]
		rel := path.Join(ws, chatfiles.Dir)
		if _, err := fsutil.StatInTreeNoLinks(app.Tree, rel); errors.Is(err, fsutil.ErrSymlink) {
			return checkResult{name, false, fmt.Sprintf("%s's exchange %s has a symbolic link on its path: every upload to %s is refused", agent, rel, agent),
				"remove the link at " + filepath.Join(app.Tree, rel) + " (the agent made it; lever creates the directory again)"}
		}
	}
	return checkResult{name, true, detail + "; ledger " + stateRel(st, p) + "/ (0700)", ""}
}

// unsafeFilesLedgerFile is the first agent file in dir (or its .1) that is
// not a regular file, is writable by others, or belongs to another user.
func unsafeFilesLedgerFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !fileledger.IsAgentFile(e.Name()) {
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
