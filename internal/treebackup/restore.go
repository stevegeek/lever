package treebackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// LinkPolicy says which recorded symbolic links a restore creates.
type LinkPolicy string

const (
	// LinksInside creates a link only when its target cannot resolve
	// outside the restore target (see linkInside).
	LinksInside LinkPolicy = "inside"
	// LinksAll creates every link as recorded. Lever still never follows one.
	LinksAll LinkPolicy = "all"
	// LinksNone creates no links.
	LinksNone LinkPolicy = "none"
)

// RestoreOptions describe one restore into a new directory.
type RestoreOptions struct {
	// Target is the operator path of the directory to create. Neither it nor
	// Target+".lever-partial" may exist.
	Target   string
	Manifest OpenManifest
	Blobs    BlobSource
	// Paths limits the restore to these subtrees plus their ancestors; empty
	// restores everything.
	Paths     [][]byte
	Links     LinkPolicy
	KeepModes bool
	KeepGit   bool
	// Forbidden are directories (the tree, the store, the state dir) the
	// target's parent must not be inside.
	Forbidden []string
	// Inside reports whether inner is inside (or is) outer by identity. An
	// error refuses the restore.
	Inside  func(inner, outer string) (bool, error)
	MinFree int64
}

// RestoreReport lists what a restore did not restore as recorded. Every
// line is already rendered for the terminal.
type RestoreReport struct {
	Collisions         []string
	ForeignWriters     []string
	LinksNotCreated    []string
	SpecialsNotCreated []string
	NoContent          []string
	Unstable           []string
	BadNames           []string
	HashFailures       []string
	Neutralised        []string
	StrippedSBits      []string

	Files, Dirs, Links int64
}

// partialSuffix names the directory a restore writes into before the final
// rename, so an interrupted restore is visibly incomplete.
const partialSuffix = ".lever-partial"

// neutralSuffix is appended to the names of files git would act on.
const neutralSuffix = ".lever-restored"

// RestoreWarning is printed after every restore.
const RestoreWarning = "restored content was written by agents: inspect it before running anything in it; " +
	"copy back with `rsync -r --no-links` after `lever tree diff` shows no directory became a link"

// OK reports whether everything selected was restored. Unstable files,
// neutralised git files and stripped s-bits are restored, so they do not
// clear it.
func (r *RestoreReport) OK() bool {
	return len(r.Collisions) == 0 && len(r.ForeignWriters) == 0 && len(r.LinksNotCreated) == 0 &&
		len(r.SpecialsNotCreated) == 0 && len(r.NoContent) == 0 && len(r.BadNames) == 0 &&
		len(r.HashFailures) == 0
}

// Write prints the report sections that have lines, then the fixed warning.
func (r *RestoreReport) Write(w io.Writer) {
	fmt.Fprintf(w, "restored %d files, %d directories, %d links\n", r.Files, r.Dirs, r.Links)
	sections := []struct {
		title string
		lines []string
	}{
		{"collisions (names the target filesystem folds together; the later one was skipped with its contents)", r.Collisions},
		{"foreign writers in the target (a name was taken by something lever did not write)", r.ForeignWriters},
		{"links not created", r.LinksNotCreated},
		{"special files not created", r.SpecialsNotCreated},
		{"entries with no content", r.NoContent},
		{"unstable (content may be torn)", r.Unstable},
		{"names the target could not hold", r.BadNames},
		{"hash failures (not restored)", r.HashFailures},
		{"neutralised git files", r.Neutralised},
		{"setuid, setgid and sticky bits not restored", r.StrippedSBits},
	}
	for _, s := range sections {
		if len(s.lines) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s (%d):\n", s.title, len(s.lines))
		for _, l := range s.lines {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	fmt.Fprintf(w, "warning: %s\n", RestoreWarning)
}

// Restore writes the selected entries of a validated manifest into a fresh
// directory. On an error after the target was created, the
// ".lever-partial" directory is left in place and the error names it.
func Restore(ctx context.Context, o RestoreOptions) (*RestoreReport, error) {
	rep := &RestoreReport{}
	fsys, err := newSysFS()
	if err != nil {
		return rep, err
	}
	if restoreFSWrap != nil {
		fsys = restoreFSWrap(fsys)
	}
	r := &restorer{o: o, fs: fsys, rep: rep, dirIndex: map[string]int{}, headDirs: map[string]bool{}}
	return rep, r.run(ctx)
}

type fsID struct{ dev, ino uint64 }

type fsStat struct {
	id  fsID
	dir bool
}

// restoreFS is the set of system calls a restore makes. Directory and file
// handles are fds; names are single components.
type restoreFS interface {
	openParent(path string) (int, error)
	stat(path string) (fsStat, error)
	fstat(fd int) (fsStat, error)
	lstatat(dirfd int, name string) (fsStat, error)
	mkdirat(dirfd int, name string) error
	openDir(dirfd int, name string) (int, error)
	createFile(dirfd int, name string) (int, error)
	write(fd int, b []byte) (int, error)
	unlinkat(dirfd int, name string) error
	symlinkat(target string, dirfd int, name string) error
	setTimes(fd int, mtime int64) error
	setLinkTimes(dirfd int, name string, mtime int64) error
	chmod(fd int, mode uint32) error
	close(fd int) error
	renameNoReplace(dirfd int, from, to string) error
	freeBytes(fd int) (int64, error)
}

type restoreErrClass int

const (
	errClassOther restoreErrClass = iota
	errClassExist
	errClassNotExist
	errClassBadName
	errClassFatal
)

func classifyRestore(err error) restoreErrClass {
	switch {
	case errors.Is(err, fs.ErrExist):
		return errClassExist
	case errors.Is(err, fs.ErrNotExist):
		return errClassNotExist
	}
	var en syscall.Errno
	if !errors.As(err, &en) {
		return errClassOther
	}
	switch en {
	case syscall.EILSEQ, syscall.ENAMETOOLONG, syscall.EINVAL:
		return errClassBadName
	case syscall.ENOSPC, syscall.EDQUOT, syscall.EIO, syscall.EROFS:
		return errClassFatal
	default:
		return errClassOther
	}
}

type frame struct {
	path []byte
	fd   int
	// names maps the fold key of each on-disk name in this directory to the
	// manifest paths of the selected entries that use it.
	names map[string][][]byte
	links []*Entry
}

type dirRec struct {
	path    []byte
	disk    string
	id      fsID
	hasMeta bool
	mode    uint32
	mtime   int64
}

type restorer struct {
	o   RestoreOptions
	fs  restoreFS
	rep *RestoreReport

	paths    [][]byte
	headDirs map[string]bool
	dirs     []dirRec
	dirIndex map[string]int
	root     RootInfo
}

func (r *restorer) run(ctx context.Context) error {
	o := r.o
	switch o.Links {
	case "":
		r.o.Links = LinksInside
	case LinksInside, LinksAll, LinksNone:
	default:
		return fmt.Errorf("tree restore: unknown link policy %q (inside, all or none)", o.Links)
	}
	if o.Manifest == nil || o.Blobs == nil {
		return errors.New("tree restore: no manifest or blob source")
	}
	for _, p := range o.Paths {
		if !validRelPath(p) {
			return fmt.Errorf("tree restore: %s is not a path in the tree", Q(p))
		}
		r.paths = append(r.paths, p)
	}
	if o.Target == "" {
		return errors.New("tree restore: no target directory")
	}
	target := filepath.Clean(o.Target)
	parent, base := filepath.Dir(target), filepath.Base(target)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return fmt.Errorf("tree restore: %s cannot be a restore target", Q([]byte(o.Target)))
	}
	partial := base + partialSuffix

	pfd, err := r.fs.openParent(parent)
	if err != nil {
		return fmt.Errorf("tree restore: parent of the target: %w", err)
	}
	defer r.fs.close(pfd)
	if err := r.checkParent(parent, pfd); err != nil {
		return err
	}
	for _, name := range []string{base, partial} {
		_, err := r.fs.lstatat(pfd, name)
		switch {
		case err == nil:
			return fmt.Errorf("tree restore: %s already exists", Q([]byte(filepath.Join(parent, name))))
		case classifyRestore(err) != errClassNotExist:
			return fmt.Errorf("tree restore: %s: %w", Q([]byte(filepath.Join(parent, name))), err)
		}
	}

	need, err := r.preflight(ctx)
	if err != nil {
		return err
	}
	free, err := r.fs.freeBytes(pfd)
	if err != nil {
		return fmt.Errorf("tree restore: free space of the target filesystem: %w", err)
	}
	if free < satAdd(need, max(o.MinFree, 0)) {
		return fmt.Errorf("tree restore: not enough free space: %d bytes to write plus %d bytes min_free, %d bytes free",
			need, o.MinFree, free)
	}

	if err := r.fs.mkdirat(pfd, partial); err != nil {
		return fmt.Errorf("tree restore: create %s: %w", Q([]byte(filepath.Join(parent, partial))), err)
	}
	partialPath := filepath.Join(parent, partial)
	fail := func(err error) error {
		return fmt.Errorf("tree restore: %w (the incomplete restore is left in %s)", err, Q([]byte(partialPath)))
	}
	rootfd, err := r.fs.openDir(pfd, partial)
	if err != nil {
		return fail(err)
	}
	defer r.fs.close(rootfd)
	st, err := r.fs.fstat(rootfd)
	if err != nil {
		return fail(err)
	}
	lst, err := r.fs.lstatat(pfd, partial)
	if err != nil {
		return fail(err)
	}
	if !st.dir || st.id != lst.id {
		return fail(errors.New("the new directory was replaced while it was opened"))
	}
	r.dirs = append(r.dirs, dirRec{id: st.id, hasMeta: r.root.HasMeta, mode: r.root.Mode, mtime: r.root.Mtime})

	if err := r.phaseFiles(ctx, rootfd); err != nil {
		return fail(err)
	}
	if err := r.phaseLinks(ctx, rootfd); err != nil {
		return fail(err)
	}
	if err := r.phaseMeta(ctx, rootfd); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := r.fs.renameNoReplace(pfd, partial, base); err != nil {
		return fail(fmt.Errorf("rename to %s: %w", Q([]byte(target)), err))
	}
	return nil
}

func (r *restorer) checkParent(parent string, pfd int) error {
	pst, err := r.fs.fstat(pfd)
	if err != nil {
		return fmt.Errorf("tree restore: parent of the target: %w", err)
	}
	if !pst.dir {
		return fmt.Errorf("tree restore: %s is not a directory", Q([]byte(parent)))
	}
	for _, f := range r.o.Forbidden {
		if r.o.Inside == nil {
			return errors.New("tree restore: no identity check for the target")
		}
		in, err := r.o.Inside(parent, f)
		if err != nil {
			return fmt.Errorf("tree restore: cannot prove %s is outside %s: %w", Q([]byte(parent)), Q([]byte(f)), err)
		}
		if in {
			return fmt.Errorf("tree restore: the target's parent %s is inside %s", Q([]byte(parent)), Q([]byte(f)))
		}
	}
	again, err := r.fs.stat(parent)
	if err != nil {
		return fmt.Errorf("tree restore: parent of the target: %w", err)
	}
	if again.id != pst.id {
		return fmt.Errorf("tree restore: %s changed while it was checked", Q([]byte(parent)))
	}
	return nil
}

// preflight reads the whole manifest before anything is created, so an
// invalid one is refused with nothing written. It returns the bytes the
// restore will write.
func (r *restorer) preflight(ctx context.Context) (int64, error) {
	rd, err := r.o.Manifest()
	if err != nil {
		return 0, fmt.Errorf("tree restore: %w", err)
	}
	defer rd.Close()
	if h := rd.Header(); h != nil {
		r.root = h.Root
	}
	found := make([]bool, len(r.paths))
	var need int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("tree restore: %w", err)
		}
		e, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("tree restore: %w", err)
		}
		if isHEAD(BaseName(e.Path)) {
			r.headDirs[string(ParentOf(e.Path))] = true
		}
		for i, p := range r.paths {
			if bytes.Equal(p, e.Path) {
				found[i] = true
			}
		}
		if r.selected(e.Path) && fileContent(e) {
			need = satAdd(need, max(e.Size, 0))
		}
	}
	for i, ok := range found {
		if !ok {
			return 0, fmt.Errorf("tree restore: %s is not in the snapshot", Q(r.paths[i]))
		}
	}
	return need, nil
}

func (r *restorer) selected(p []byte) bool {
	if len(r.paths) == 0 {
		return true
	}
	for _, q := range r.paths {
		if bytes.Equal(p, q) || IsUnder(p, q) || IsUnder(q, p) {
			return true
		}
	}
	return false
}

// walk streams the selected entries in pre-order with a stack of open
// directory fds. enter returns the frame of a directory to descend into, or
// nil to skip its subtree. leave runs for every frame, the root last, before
// its fd is closed.
func (r *restorer) walk(ctx context.Context, rootfd int,
	enter func(top *frame, e *Entry) (*frame, error),
	visit func(top *frame, e *Entry) error,
	leave func(f *frame) error,
) (err error) {
	rd, err := r.o.Manifest()
	if err != nil {
		return err
	}
	defer rd.Close()
	stack := []*frame{{fd: rootfd, names: map[string][][]byte{}}}
	defer func() {
		for _, f := range stack[1:] {
			r.fs.close(f.fd)
		}
	}()
	pop := func() error {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		lerr := leave(f)
		if cerr := r.fs.close(f.fd); lerr == nil && cerr != nil {
			lerr = cerr
		}
		return lerr
	}
	var skip []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !r.selected(e.Path) {
			continue
		}
		// Paths and entries outlive this iteration (frames, the directory
		// list, deferred links); the reader may reuse its buffers.
		ec := *e
		ec.Path, ec.Target = bytes.Clone(e.Path), bytes.Clone(e.Target)
		e = &ec
		if skip != nil {
			if IsUnder(e.Path, skip) {
				continue
			}
			skip = nil
		}
		parent := ParentOf(e.Path)
		for len(stack) > 1 && !bytes.Equal(stack[len(stack)-1].path, parent) {
			if err := pop(); err != nil {
				return err
			}
		}
		top := stack[len(stack)-1]
		if !bytes.Equal(top.path, parent) {
			return fmt.Errorf("internal error: the parent of %s is not open", Q(e.Path))
		}
		if e.Type == TypeDir {
			f, err := enter(top, e)
			if err != nil {
				return err
			}
			if f == nil {
				skip = e.Path
				continue
			}
			stack = append(stack, f)
			continue
		}
		if err := visit(top, e); err != nil {
			return err
		}
	}
	for len(stack) > 1 {
		if err := pop(); err != nil {
			return err
		}
	}
	return leave(stack[0])
}

func (r *restorer) phaseFiles(ctx context.Context, rootfd int) error {
	enter := func(top *frame, e *Entry) (*frame, error) {
		disk := r.diskName(e)
		if err := r.fs.mkdirat(top.fd, disk); err != nil {
			return nil, r.createFailed(top, e, disk, err)
		}
		addName(top, disk, e.Path)
		fd, err := r.fs.openDir(top.fd, disk)
		if err != nil {
			return nil, fmt.Errorf("open the new directory %s: %w", Q(e.Path), err)
		}
		st, err := r.fs.fstat(fd)
		var lst fsStat
		if err == nil {
			lst, err = r.fs.lstatat(top.fd, disk)
		}
		if err == nil && (!st.dir || st.id != lst.id) {
			err = errors.New("replaced while it was created")
		}
		if err != nil {
			r.fs.close(fd)
			return nil, fmt.Errorf("the new directory %s: %w", Q(e.Path), err)
		}
		r.noteNeutralised(e, disk)
		r.noteDirState(e)
		r.dirIndex[string(e.Path)] = len(r.dirs)
		r.dirs = append(r.dirs, dirRec{path: e.Path, disk: disk, id: st.id, hasMeta: e.HasMeta, mode: e.Mode, mtime: e.Mtime})
		r.rep.Dirs++
		return &frame{path: e.Path, fd: fd, names: map[string][][]byte{}}, nil
	}
	visit := func(top *frame, e *Entry) error {
		switch e.Type {
		case TypeFile:
			if !fileContent(e) {
				r.rep.NoContent = append(r.rep.NoContent, Q(e.Path)+": "+noContentWhy(e))
				return nil
			}
			return r.restoreFile(ctx, top, e)
		case TypeLink:
			return nil
		case TypeFIFO, TypeSocket, TypeChar, TypeBlock:
			r.rep.SpecialsNotCreated = append(r.rep.SpecialsNotCreated, fmt.Sprintf("%s: %s", Q(e.Path), e.Type))
		default:
			r.rep.NoContent = append(r.rep.NoContent, Q(e.Path)+": type unknown ("+stateWhy(e)+")")
		}
		return nil
	}
	switch {
	case r.root.State == StateListingUnstable && r.root.Listing == "":
		r.rep.Unstable = append(r.rep.Unstable, "the tree root: the directory changed while it was listed; entries may be missing")
	case r.root.State != StateOK || r.root.Listing != "":
		r.rep.NoContent = append(r.rep.NoContent, "the tree root: "+rootWhy(r.root))
	}
	return r.walk(ctx, rootfd, enter, visit, func(*frame) error { return nil })
}

func (r *restorer) restoreFile(ctx context.Context, top *frame, e *Entry) error {
	disk := r.diskName(e)
	fd, err := r.fs.createFile(top.fd, disk)
	if err != nil {
		return r.createFailed(top, e, disk, err)
	}
	addName(top, disk, e.Path)
	defer r.fs.close(fd)
	why, err := r.fill(ctx, fd, e)
	if err != nil {
		return fmt.Errorf("write %s: %w", Q(e.Path), err)
	}
	if why != "" {
		if err := r.unlinkOwn(top.fd, disk, fd); err != nil {
			return fmt.Errorf("remove %s after a failed restore: %w", Q(e.Path), err)
		}
		r.rep.HashFailures = append(r.rep.HashFailures, Q(e.Path)+": "+why)
		return nil
	}
	if r.o.KeepModes && e.HasMeta {
		if err := r.fs.chmod(fd, e.Mode&0o777); err != nil {
			return fmt.Errorf("mode of %s: %w", Q(e.Path), err)
		}
		r.noteSBits(e.Path, e.Mode)
	}
	if e.HasMeta {
		if err := r.fs.setTimes(fd, e.Mtime); err != nil {
			return fmt.Errorf("mtime of %s: %w", Q(e.Path), err)
		}
	}
	r.noteNeutralised(e, disk)
	if e.State == StateUnstable {
		r.rep.Unstable = append(r.rep.Unstable, Q(e.Path)+": the file changed while it was read")
	}
	r.rep.Files++
	return nil
}

// fill copies the blob into fd, hashing what it writes. A non-empty reason
// means the content is not the recorded content and the file must go.
func (r *restorer) fill(ctx context.Context, fd int, e *Entry) (string, error) {
	rc, _, err := r.o.Blobs.OpenBlob(e.SHA256)
	if err != nil {
		return "blob " + e.SHA256 + " unavailable: " + err.Error(), nil
	}
	defer rc.Close()
	h := sha256.New()
	in := io.LimitReader(rc, satAdd(max(e.Size, 0), 1))
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		k, rerr := in.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			n += int64(k)
			if err := r.writeAll(fd, buf[:k]); err != nil {
				return "", err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "blob " + e.SHA256 + " unreadable: " + rerr.Error(), nil
		}
	}
	if n != e.Size {
		return fmt.Sprintf("blob %s holds %d bytes, the snapshot recorded %d", e.SHA256, n, e.Size), nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Sprintf("blob %s hashes to %s", e.SHA256, got), nil
	}
	return "", nil
}

func (r *restorer) writeAll(fd int, b []byte) error {
	for len(b) > 0 {
		n, err := r.fs.write(fd, b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// unlinkOwn removes name only while it is still the file open as fd.
func (r *restorer) unlinkOwn(dirfd int, name string, fd int) error {
	st, err := r.fs.fstat(fd)
	if err != nil {
		return err
	}
	lst, err := r.fs.lstatat(dirfd, name)
	if err != nil {
		return err
	}
	if st.id != lst.id {
		return errors.New("the name no longer refers to the file lever wrote")
	}
	return r.fs.unlinkat(dirfd, name)
}

// createFailed reports a name the target would not take. It returns an
// error only when the restore must stop.
func (r *restorer) createFailed(top *frame, e *Entry, disk string, err error) error {
	switch classifyRestore(err) {
	case errClassExist:
		if other := otherName(top, disk, e.Path); other != nil {
			r.rep.Collisions = append(r.rep.Collisions,
				fmt.Sprintf("%s and %s are one name in the target; %s not restored", Q(other), Q(e.Path), Q(e.Path)))
		} else {
			r.rep.ForeignWriters = append(r.rep.ForeignWriters,
				fmt.Sprintf("%s: the name was taken by something lever did not write; not restored", Q(e.Path)))
		}
	case errClassFatal:
		return fmt.Errorf("create %s: %w", Q(e.Path), err)
	default:
		r.rep.BadNames = append(r.rep.BadNames, fmt.Sprintf("%s: %v", Q(e.Path), err))
	}
	return nil
}

func (r *restorer) phaseLinks(ctx context.Context, rootfd int) error {
	enter := func(top *frame, e *Entry) (*frame, error) {
		addName(top, r.diskName(e), e.Path)
		i, ok := r.dirIndex[string(e.Path)]
		if !ok {
			return nil, nil
		}
		fd, err := r.reopen(top.fd, r.dirs[i])
		if err != nil {
			return nil, err
		}
		return &frame{path: e.Path, fd: fd, names: map[string][][]byte{}}, nil
	}
	visit := func(top *frame, e *Entry) error {
		addName(top, r.diskName(e), e.Path)
		if e.Type == TypeLink {
			top.links = append(top.links, e)
		}
		return nil
	}
	// Links are created only once their directory's whole listing has been
	// seen, so an EEXIST can be matched against every sibling that folds to
	// the same name.
	leave := func(f *frame) error {
		for _, e := range f.links {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.restoreLink(f, e); err != nil {
				return err
			}
		}
		return nil
	}
	return r.walk(ctx, rootfd, enter, visit, leave)
}

func (r *restorer) restoreLink(f *frame, e *Entry) error {
	if why := r.linkRefusal(e); why != "" {
		r.rep.LinksNotCreated = append(r.rep.LinksNotCreated, why)
		return nil
	}
	disk := r.diskName(e)
	if err := r.fs.symlinkat(string(e.Target), f.fd, disk); err != nil {
		return r.createFailed(f, e, disk, err)
	}
	if e.HasMeta {
		if err := r.fs.setLinkTimes(f.fd, disk, e.Mtime); err != nil {
			return fmt.Errorf("mtime of %s: %w", Q(e.Path), err)
		}
	}
	r.noteNeutralised(e, disk)
	r.rep.Links++
	return nil
}

func (r *restorer) linkRefusal(e *Entry) string {
	if !e.HasTarget {
		return Q(e.Path) + ": its target was not recorded"
	}
	switch r.o.Links {
	case LinksNone:
		return fmt.Sprintf("%s -> %s: --links=none", Q(e.Path), Q(e.Target))
	case LinksAll:
		return ""
	}
	if !linkInside(e.Target, Depth(ParentOf(e.Path))) {
		return fmt.Sprintf("%s -> %s: could resolve outside the restore", Q(e.Path), Q(e.Target))
	}
	return ""
}

// reopen opens a directory phase 1 created and proves it is the same one.
func (r *restorer) reopen(dirfd int, d dirRec) (int, error) {
	fd, err := r.fs.openDir(dirfd, d.disk)
	if err != nil {
		return -1, fmt.Errorf("reopen %s: %w", Q(d.path), err)
	}
	st, err := r.fs.fstat(fd)
	if err == nil && st.id != d.id {
		err = errors.New("it was replaced during the restore")
	}
	if err != nil {
		r.fs.close(fd)
		return -1, fmt.Errorf("reopen %s: %w", Q(d.path), err)
	}
	return fd, nil
}

// phaseMeta applies directory metadata children first, so setting a
// directory's mode or mtime is the last thing done in it.
func (r *restorer) phaseMeta(ctx context.Context, rootfd int) error {
	type open struct {
		d  dirRec
		fd int
	}
	stack := []open{{d: r.dirs[0], fd: rootfd}}
	defer func() {
		for _, o := range stack[1:] {
			r.fs.close(o.fd)
		}
	}()
	pop := func() error {
		o := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		err := r.applyDirMeta(o.fd, o.d)
		if cerr := r.fs.close(o.fd); err == nil {
			err = cerr
		}
		return err
	}
	for _, d := range r.dirs[1:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent := ParentOf(d.path)
		for len(stack) > 1 && !bytes.Equal(stack[len(stack)-1].d.path, parent) {
			if err := pop(); err != nil {
				return err
			}
		}
		if !bytes.Equal(stack[len(stack)-1].d.path, parent) {
			return fmt.Errorf("internal error: the parent of %s is not open", Q(d.path))
		}
		fd, err := r.reopen(stack[len(stack)-1].fd, d)
		if err != nil {
			return err
		}
		stack = append(stack, open{d: d, fd: fd})
	}
	for len(stack) > 1 {
		if err := pop(); err != nil {
			return err
		}
	}
	return r.applyDirMeta(rootfd, r.dirs[0])
}

func (r *restorer) applyDirMeta(fd int, d dirRec) error {
	if !d.hasMeta {
		return nil
	}
	name := d.path
	if name == nil {
		name = []byte(".")
	}
	if r.o.KeepModes {
		if err := r.fs.chmod(fd, d.mode&0o777); err != nil {
			return fmt.Errorf("mode of %s: %w", Q(name), err)
		}
		r.noteSBits(name, d.mode)
	}
	if err := r.fs.setTimes(fd, d.mtime); err != nil {
		return fmt.Errorf("mtime of %s: %w", Q(name), err)
	}
	return nil
}

func (r *restorer) noteSBits(p []byte, mode uint32) {
	if mode&0o7000 != 0 {
		r.rep.StrippedSBits = append(r.rep.StrippedSBits,
			fmt.Sprintf("%s: mode %04o restored as %04o", Q(p), mode&0o7777, mode&0o777))
	}
}

func (r *restorer) noteNeutralised(e *Entry, disk string) {
	if disk == string(BaseName(e.Path)) {
		return
	}
	now := append(append(bytes.Clone(ParentOf(e.Path)), '/'), disk...)
	if len(ParentOf(e.Path)) == 0 {
		now = []byte(disk)
	}
	r.rep.Neutralised = append(r.rep.Neutralised, fmt.Sprintf("%s written as %s", Q(e.Path), Q(now)))
}

func (r *restorer) noteDirState(e *Entry) {
	switch {
	case e.State == StateListingUnstable:
		r.rep.Unstable = append(r.rep.Unstable, Q(e.Path)+": the directory changed while it was listed; entries may be missing")
	case e.State != StateOK:
		r.rep.NoContent = append(r.rep.NoContent, Q(e.Path)+": directory "+stateWhy(e)+"; entries below it may be missing")
	case e.Listing != "":
		r.rep.NoContent = append(r.rep.NoContent, Q(e.Path)+": directory could not be listed; entries below it may be missing")
	}
}

// diskName is the name an entry is written under: git's control files are
// renamed so that nothing in the restored tree makes the operator's git
// run a command.
func (r *restorer) diskName(e *Entry) string {
	name := string(BaseName(e.Path))
	if r.o.KeepGit {
		return name
	}
	if r.headDirs[string(ParentOf(e.Path))] && isGitControl(name) {
		return name + neutralSuffix
	}
	// A .git link is a gitlink too, through whatever file it points at.
	if e.Type != TypeDir && isGitLink(name) {
		return name + neutralSuffix
	}
	return name
}

// The git names are compared through gitKey, not byte for byte: a
// case-insensitive target opens "hoo\u212As" (Kelvin sign) as "hooks", and
// Linux casefold also applies full case folding and ignores default
// ignorable code points ("con\uFB01g", "HE\u00ADAD", ".gi\u200Dt").
func isHEAD(name []byte) bool { return gitKey(string(name)) == "head" }

func isGitControl(name string) bool {
	k := gitKey(name)
	return k == "config" || k == "config.worktree" || k == "commondir" || k == "hooks"
}

func isGitLink(name string) bool { return gitKey(name) == ".git" }

// gitKey reduces a name to the ASCII a folding filesystem might read it
// as, erring towards a match: renaming one file too many is cheap, and one
// too few lets git run the agent's command. Invisible and format characters
// are dropped, ligatures and other characters that fold to ASCII letters
// are expanded, fullwidth ASCII is mapped down, and any other non-ASCII
// rune is dropped.
func gitKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < utf8.RuneSelf:
			if 'A' <= r && r <= 'Z' {
				r += 'a' - 'A'
			}
			b.WriteRune(r)
		case gitIgnorable(r):
		case gitExpand[r] != "":
			b.WriteString(gitExpand[r])
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteString(strings.ToLower(string(r - 0xFEE0)))
		default:
			for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
				if f < utf8.RuneSelf {
					b.WriteString(strings.ToLower(string(f)))
					break
				}
			}
		}
	}
	return b.String()
}

func gitIgnorable(r rune) bool {
	switch {
	case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r):
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF, r >= 0x180B && r <= 0x180F:
		return true
	}
	switch r {
	case 0x034F, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x3164, 0xFFA0:
		return true
	}
	return false
}

// gitExpand: characters whose full case folding or compatibility form is
// ASCII letters.
var gitExpand = map[rune]string{
	0x00DF: "ss", 0x1E9E: "ss", 0x017F: "s", 0x212A: "k", 0x0130: "i", 0x0131: "i",
	0xFB00: "ff", 0xFB01: "fi", 0xFB02: "fl", 0xFB03: "ffi", 0xFB04: "ffl", 0xFB05: "st", 0xFB06: "st",
}

// foldKey maps names a case-insensitive filesystem treats as one to the
// same key: each valid rune becomes the smallest rune of its Unicode
// simple-fold orbit (the equivalence strings.EqualFold uses), and invalid
// bytes stay as they are, so "x\xff" and "x\xfe" stay apart. Unicode
// normalisation (NFC against NFD) is not folded: such a pair is reported as
// a foreign writer, which is still a failed restore.
func foldKey(name string) string {
	var b []byte
	for i := 0; i < len(name); {
		c, n := utf8.DecodeRuneInString(name[i:])
		if c == utf8.RuneError && n == 1 {
			b = append(b, name[i])
			i++
			continue
		}
		m := c
		for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		b = utf8.AppendRune(b, m)
		i += n
	}
	return string(b)
}

func addName(f *frame, disk string, p []byte) {
	k := foldKey(disk)
	f.names[k] = append(f.names[k], p)
}

func otherName(f *frame, disk string, self []byte) []byte {
	for _, p := range f.names[foldKey(disk)] {
		if !bytes.Equal(p, self) {
			return p
		}
	}
	return nil
}

// linkInside reports whether a link whose parent lies parentDepth levels
// below the restore root may be created under --links=inside. The target
// must be k leading ".." followed only by plain names, with k at most
// parentDepth. Restored directories are real and links are leaves, so a
// resolution climbs at most to the root and then only descends through
// real directories or other links that obey the same rule.
func linkInside(target []byte, parentDepth int) bool {
	if len(target) == 0 || target[0] == '/' || bytes.IndexByte(target, 0) >= 0 {
		return false
	}
	t := bytes.TrimSuffix(target, []byte{'/'})
	comps := bytes.Split(t, []byte{'/'})
	k := 0
	for k < len(comps) && string(comps[k]) == ".." {
		k++
	}
	for _, c := range comps[k:] {
		if len(c) == 0 || string(c) == "." || string(c) == ".." {
			return false
		}
	}
	return k <= parentDepth
}

func validRelPath(p []byte) bool {
	if len(p) == 0 || bytes.IndexByte(p, 0) >= 0 {
		return false
	}
	for _, c := range bytes.Split(p, []byte{'/'}) {
		if len(c) == 0 || string(c) == "." || string(c) == ".." {
			return false
		}
	}
	return true
}

func fileContent(e *Entry) bool {
	return e.Type == TypeFile && e.SHA256 != "" && e.State != StateSecret
}

func noContentWhy(e *Entry) string {
	switch e.State {
	case StateSecret:
		return "secret: its content is never stored"
	case StateOK:
		return "no content recorded"
	}
	return stateWhy(e) + ": content not captured"
}

func stateWhy(e *Entry) string {
	switch {
	case e.State == StateOK:
		return "no state"
	case e.Reason != "":
		return string(e.State) + " (" + string(e.Reason) + ")"
	}
	return string(e.State)
}

func rootWhy(root RootInfo) string {
	switch {
	case root.State == StateOK:
		return "could not be listed; entries may be missing"
	case root.Reason != "":
		return string(root.State) + " (" + string(root.Reason) + "); entries may be missing"
	}
	return string(root.State) + "; entries may be missing"
}

func satAdd(a, b int64) int64 {
	if s := a + b; s >= a {
		return s
	}
	return 1<<63 - 1
}

func clampMul(a, b uint64) int64 {
	const top = 1<<63 - 1
	if b != 0 && a > top/b {
		return top
	}
	return int64(a * b)
}
