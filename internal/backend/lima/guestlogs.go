package lima

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/lever/internal/backend/guest"
	"github.com/stevegeek/lever/internal/fsutil"
)

// guestLogFiles are the instance logs Lima recreates on every VM start, so a
// guest kernel panic's console trace is lost on the next start unless it is
// copied first. Which serial files exist depends on the VM type and arch.
var guestLogFiles = []string{"serial.log", "serialp.log", "serialv.log", "ha.stderr.log"}

func isSerialLog(name string) bool { return strings.HasPrefix(name, "serial") }

const (
	// Sets are pruned per kind, so the directory stays under 2 kinds x
	// GuestLogKeep sets x len(guestLogFiles) files x GuestLogCap.
	GuestLogCap  = 4 << 20
	GuestLogKeep = 5
	// livePanicKey names the serial log a live save was taken from: the live
	// scan saves at most once per VM run, so console text a guest process
	// forges cannot evict saved sets faster than the VM restarts.
	livePanicKey   = "panic-key"
	livePanicKind  = "-panic"
	guestLogLayout = "20060102T150405Z"
)

// guestLogSetRE matches only the set names lever creates; nothing else in
// the directory is listed or pruned.
var guestLogSetRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(?:-[2-9])?(?:-panic)?$`)

// panicLineRE is a lead, not proof: any guest process, an agent's container
// included, can put a line of this shape on the console (a process name with
// a newline in it, printed in an out-of-memory line). A kernel line may also
// follow a login prompt on the same console line, hence the unanchored
// timestamp form.
var panicLineRE = regexp.MustCompile(`(?:^(?:<[0-9]+>)?|\[\s*[0-9]+\.[0-9]+\]\s*)(?:\[\s*[TC][0-9]+\]\s*)?(?:Kernel panic - not syncing|Oops[: ]|BUG: |general protection fault|(?:traps: )?PANIC: double fault|double fault)`)

// panicLineMax bounds the line doctor prints.
const panicLineMax = 200

// openLimaLog: the instance directory is the operator's, so only the leaf is
// guarded.
func openLimaLog(path string) (*os.File, fs.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w", path, fsutil.ErrNotRegularFile)
	}
	return f, fi, nil
}

func readTail(f *os.File, size int64) ([]byte, error) {
	var off int64
	if size > GuestLogCap {
		off = size - GuestLogCap
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, GuestLogCap))
}

// openGuestLogRoot opens dir (created 0700 when missing) as an os.Root with
// no symbolic link below its parent, which is lever's state directory.
func openGuestLogRoot(dir string) (*os.Root, error) {
	parent, base := filepath.Dir(dir), filepath.Base(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	r, err := fsutil.OpenDirInTreeNoLinks(parent, base)
	if errors.Is(err, fs.ErrNotExist) {
		pr, perr := os.OpenRoot(parent)
		if perr != nil {
			return nil, perr
		}
		merr := pr.Mkdir(base, 0o700)
		pr.Close()
		if merr != nil && !errors.Is(merr, fs.ErrExist) {
			return nil, merr
		}
		r, err = fsutil.OpenDirInTreeNoLinks(parent, base)
	}
	if err != nil {
		return nil, err
	}
	if err := r.Chmod(".", 0o700); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

type guestLog struct {
	name  string
	data  []byte
	ident string // device and inode
}

// readGuestLogs reads the tail of every non-empty Lima log in instDir. A
// missing file is skipped; one that is not a regular file is an error.
func readGuestLogs(instDir string) ([]guestLog, error) {
	var out []guestLog
	var errs []error
	for _, name := range guestLogFiles {
		f, fi, err := openLimaLog(filepath.Join(instDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		b, err := readTail(f, fi.Size())
		f.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if len(b) > 0 {
			out = append(out, guestLog{name, b, fileIdentity(fi)})
		}
	}
	return out, errors.Join(errs...)
}

// writeGuestLogSet saves logs as a new set of the kind (suffix "" or
// livePanicKind) and prunes that kind to GuestLogKeep sets. It returns the
// set's path.
func writeGuestLogSet(dir string, now time.Time, kind string, logs []guestLog, extra map[string][]byte) (string, error) {
	r, err := openGuestLogRoot(dir)
	if err != nil {
		return "", fmt.Errorf("guest log directory %s: %w", dir, err)
	}
	defer r.Close()
	stamp := now.UTC().Format(guestLogLayout)
	var name string
	for i := 1; i <= 9; i++ {
		name = stamp
		if i > 1 {
			name = fmt.Sprintf("%s-%d", stamp, i)
		}
		name += kind
		if err = r.Mkdir(name, 0o700); !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("guest log set in %s: %w", dir, err)
	}
	// A failed save must not keep a partial set or skip the prune, or the
	// directory's bound no longer holds.
	defer pruneGuestLogSets(r, kind)
	set, err := r.OpenRoot(name)
	if err != nil {
		r.RemoveAll(name)
		return "", err
	}
	defer set.Close()
	files := map[string][]byte{}
	for _, l := range logs {
		files[l.name] = l.data
	}
	for k, v := range extra {
		files[k] = v
	}
	for n, data := range files {
		if err := writeNew(set, n, data); err != nil {
			r.RemoveAll(name)
			return "", fmt.Errorf("%s: %w", filepath.Join(dir, name, n), err)
		}
	}
	return filepath.Join(dir, name), nil
}

func writeNew(r *os.Root, name string, data []byte) error {
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func guestLogSets(r *os.Root) []string {
	d, err := r.Open(".")
	if err != nil {
		return nil
	}
	defer d.Close()
	ents, _ := d.ReadDir(-1)
	var out []string
	for _, e := range ents {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 && guestLogSetRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(setOrder(a), setOrder(b)) })
	return out
}

// setOrder: "T...Z" before "T...Z-2".
func setOrder(name string) string {
	base := strings.TrimSuffix(name, livePanicKind)
	if len(base) == len(guestLogLayout) {
		base += "-1"
	}
	return base
}

func pruneGuestLogSets(r *os.Root, kind string) {
	var mine []string
	for _, s := range guestLogSets(r) {
		if strings.HasSuffix(s, livePanicKind) == (kind == livePanicKind) {
			mine = append(mine, s)
		}
	}
	for len(mine) > GuestLogKeep {
		// os.Root.RemoveAll removes a link, never what it points to.
		r.RemoveAll(mine[0])
		mine = mine[1:]
	}
}

// SaveGuestLogs saves the Lima logs of instDir before a VM start recreates
// them; "" when every log is missing or empty. A set whose serial log holds a
// panic line is kept with the panic sets, so routine restarts do not evict it.
func SaveGuestLogs(instDir, dir string, now time.Time) (string, error) {
	logs, rerr := readGuestLogs(instDir)
	if len(logs) == 0 {
		return "", rerr
	}
	kind := ""
	for _, l := range logs {
		if isSerialLog(l.name) && firstPanicLine(l.data) != "" {
			kind = livePanicKind
		}
	}
	path, err := writeGuestLogSet(dir, now, kind, logs, nil)
	return path, errors.Join(rerr, err)
}

// SaveLivePanic saves the Lima logs of a running VM when a serial log holds a
// panic line: the guest reboots itself after a panic, and the trace is kept
// only until the VM process restarts. Later traces in the same VM run reach
// the set SaveGuestLogs takes before the next start.
func SaveLivePanic(instDir, dir string, now time.Time) (string, error) {
	logs, rerr := readGuestLogs(instDir)
	var key string
	for _, l := range logs {
		if !isSerialLog(l.name) {
			continue
		}
		if firstPanicLine(l.data) != "" {
			key = fmt.Sprintf("%s %s\n", l.name, l.ident)
			break
		}
	}
	if key == "" {
		return "", rerr
	}
	r, err := openGuestLogRoot(dir)
	if err != nil {
		return "", fmt.Errorf("guest log directory %s: %w", dir, err)
	}
	for _, s := range guestLogSets(r) {
		if !strings.HasSuffix(s, livePanicKind) {
			continue
		}
		if f, err := r.Open(filepath.Join(s, livePanicKey)); err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, 4096))
			f.Close()
			if string(b) == key {
				r.Close()
				return "", rerr
			}
		}
	}
	r.Close()
	path, err := writeGuestLogSet(dir, now, livePanicKind, logs, map[string][]byte{livePanicKey: []byte(key)})
	return path, errors.Join(rerr, err)
}

func fileIdentity(fi fs.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	return "?"
}

// firstPanicLine returns the first panic line in data, raw.
func firstPanicLine(data []byte) string {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if panicLineRE.Match(bytes.TrimLeft(line, "\r")) {
			return string(line)
		}
	}
	return ""
}

// LatestGuestLogSet returns the path of the newest saved set in dir, "" when
// there is none or dir cannot be read.
func LatestGuestLogSet(dir string) string {
	r, err := fsutil.OpenDirInTreeNoLinks(filepath.Dir(dir), filepath.Base(dir))
	if err != nil {
		return ""
	}
	defer r.Close()
	sets := guestLogSets(r)
	if len(sets) == 0 {
		return ""
	}
	return filepath.Join(dir, sets[len(sets)-1])
}

// GuestLogSetTime is when the set at path was saved; zero when its name is
// not one lever creates.
func GuestLogSetTime(path string) time.Time {
	name := filepath.Base(path)
	if !guestLogSetRE.MatchString(name) {
		return time.Time{}
	}
	t, _ := time.Parse(guestLogLayout, name[:len(guestLogLayout)])
	return t
}

// PanicLineIn returns the first panic line in the serial logs of the saved
// set at path, made safe to print: the console is written by the guest, so
// the line is untrusted. "" when there is none.
func PanicLineIn(set string) string {
	for _, name := range guestLogFiles {
		if !isSerialLog(name) {
			continue
		}
		f, _, err := fsutil.OpenInTreeNoLinks(set, name, GuestLogCap)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, GuestLogCap))
		f.Close()
		if line := firstPanicLine(b); line != "" {
			return name + ": " + boundLine(line)
		}
	}
	return ""
}

func boundLine(s string) string {
	return guest.SafeLine(strings.TrimRight(s, "\r\n"), panicLineMax)
}
