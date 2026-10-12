//go:build linux

package voice

import (
	"fmt"
	"io/fs"
	"syscall"
)

// fileStamp describes fi's file without reading it ("" = cannot).
func fileStamp(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%o:%d", st.Dev, st.Ino, st.Size, st.Mtim.Nano(), st.Ctim.Nano(), st.Uid, st.Mode, st.Nlink)
}
