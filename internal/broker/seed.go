package broker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

// Seeding a restored agent's workspace from an archive. The archive holds
// what a destroyed agent wrote, so it is read as hostile, by these rules:
//
//   - only directories and regular files are written; symbolic and hard
//     links, devices, FIFOs and anything else are skipped (and counted);
//   - every name must be a clean relative path: absolute names, ".." and
//     "." components, empty components and over-long names refuse the whole
//     archive;
//   - each name appears once, and nothing lies beneath a regular file;
//   - the whole is held to seedLimits (bytes, entries, depth), and a passed
//     limit refuses the archive, never cuts it short;
//   - modes keep the owner's execute bit only (0644 or 0755): no setuid,
//     setgid or sticky bit, no owner;
//   - everything is written through an os.Root on the new agent's own
//     directory, the one spawn just created, each file created exclusively
//     with no link followed at its leaf, so nothing outside it is reached;
//   - the archive is checked whole (seedCheck) before any name is drawn, and
//     checked again as it is written.

// seedLimits bound what a restore writes into the tree.
type seedLimits struct {
	MaxBytes   int64
	MaxEntries int
	MaxDepth   int
}

// defaultSeedLimits match what destroy archives (archiveMaxBytes and
// archiveMaxFiles; a file may lie one level below the deepest directory),
// so every archive destroy wrote can be restored.
var defaultSeedLimits = seedLimits{MaxBytes: archiveMaxBytes, MaxEntries: archiveMaxFiles, MaxDepth: archiveMaxDepth + 1}

// seedMaxNameBytes bounds the names a check holds in memory (each is kept
// to refuse duplicates).
const seedMaxNameBytes = 128 << 20

// seedMaxTrailer bounds what may follow a tar's end in the gzip stream.
const seedMaxTrailer = 1 << 20

// errSeedRefused wraps every reason an archive is refused.
var errSeedRefused = errors.New("the archive was refused")

func seedRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSeedRefused, fmt.Sprintf(format, args...))
}

// seedStats is what a check or a seed found.
type seedStats struct {
	Files   int
	Dirs    int
	Skipped int
	Bytes   int64
	// Newest is the .jsonl file with the latest modification time (a
	// conversation's transcript): agent-written, sanitize before showing.
	Newest     string
	NewestTime time.Time
}

// seedOpts says where and what a seed writes.
type seedOpts struct {
	// Prefix is the workspace-relative directory the entries go under ("":
	// the workspace itself). Lever's own constant, never archive text.
	Prefix string
	// Skip reports names (archive-relative) to pass over: the template's
	// skill, which spawn already wrote, and lever's own transcript folder.
	Skip   func(name string) bool
	Limits seedLimits
}

// seedCheck reads the gzip tar src whole and refuses it as seedTar would,
// writing nothing.
func seedCheck(ctx context.Context, src io.Reader, o seedOpts) (seedStats, error) {
	return seedTar(ctx, src, nil, o)
}

// seedTar reads the gzip tar src and, with dir set, writes its directories
// and regular files under dir/o.Prefix. With dir nil it only checks.
func seedTar(ctx context.Context, src io.Reader, dir *os.Root, o seedOpts) (seedStats, error) {
	var st seedStats
	lim := o.Limits
	if lim.MaxBytes <= 0 {
		lim = defaultSeedLimits
	}
	zr, err := gzip.NewReader(src)
	if err != nil {
		return st, seedRefused("not a gzip stream: %v", err)
	}
	zr.Multistream(false)
	tr := tar.NewReader(zr)
	seen := map[string]bool{}
	files := map[string]bool{}
	parents := map[string]bool{}
	var skippedBytes, nameBytes int64
	entries := 0
	if dir != nil && o.Prefix != "" {
		if err := dir.MkdirAll(o.Prefix, 0o755); err != nil {
			return st, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, seedRefused("reading the archive: %v", err)
		}
		if entries++; entries > lim.MaxEntries {
			return st, seedRefused("more than %d entries", lim.MaxEntries)
		}
		name, keep, err := seedName(h.Name, lim.MaxDepth)
		if err != nil {
			return st, err
		}
		if !keep {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what old tars write
		default:
			// Links, devices, FIFOs: never written. Their data, if any, still
			// passes through the decompressor, so it counts.
			if h.Size < 0 || h.Size > lim.MaxBytes || skippedBytes+h.Size > lim.MaxBytes {
				return st, seedRefused("more than %d bytes in all", lim.MaxBytes)
			}
			skippedBytes += h.Size
			st.Skipped++
			continue
		}
		if seen[name] {
			return st, seedRefused("%q appears twice", name)
		}
		if nameBytes += int64(len(name)); nameBytes > seedMaxNameBytes {
			return st, seedRefused("more than %d MiB of names", seedMaxNameBytes>>20)
		}
		seen[name] = true
		if h.Typeflag != tar.TypeDir && parents[name] {
			return st, seedRefused("%q is a file, and an earlier entry lies beneath it", name)
		}
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if files[p] {
				return st, seedRefused("%q lies beneath the file %q", name, p)
			}
			parents[p] = true
		}
		skip := o.Skip != nil && o.Skip(name)
		if h.Typeflag == tar.TypeDir {
			st.Dirs++
			if dir != nil && !skip {
				if err := dir.MkdirAll(path.Join(o.Prefix, name), 0o755); err != nil {
					return st, fmt.Errorf("%s: %w", name, err)
				}
			}
			continue
		}
		if h.Size < 0 || st.Bytes+h.Size > lim.MaxBytes || st.Bytes+h.Size+skippedBytes > lim.MaxBytes {
			return st, seedRefused("more than %d bytes in all", lim.MaxBytes)
		}
		files[name] = true
		st.Bytes += h.Size
		if skip {
			st.Skipped++
			continue
		}
		st.Files++
		mtime := h.ModTime
		if strings.HasSuffix(name, ".jsonl") && (st.Newest == "" || mtime.After(st.NewestTime)) {
			st.Newest, st.NewestTime = name, mtime
		}
		if dir == nil {
			if _, err := io.CopyN(io.Discard, tr, h.Size); err != nil {
				return st, seedRefused("%q: %v", name, err)
			}
			continue
		}
		if err := seedFile(dir, path.Join(o.Prefix, name), h, tr); err != nil {
			return st, fmt.Errorf("%s: %w", name, err)
		}
	}
	// gzip checks its CRC and length only at the stream's end: read on, so a
	// damaged archive is refused rather than half seeded.
	if n, err := io.Copy(io.Discard, io.LimitReader(zr, seedMaxTrailer+1)); err != nil {
		return st, seedRefused("reading the archive: %v", err)
	} else if n > seedMaxTrailer {
		return st, seedRefused("more than %d bytes after the end of the archive", seedMaxTrailer)
	}
	return st, nil
}

// seedFile writes one regular file, exclusively, with no link at its leaf.
func seedFile(dir *os.Root, p string, h *tar.Header, r io.Reader) error {
	if d := path.Dir(p); d != "." {
		if err := dir.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	mode := fs.FileMode(0o644)
	if h.Mode&0o100 != 0 {
		mode = 0o755
	}
	f, err := dir.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
	if err != nil {
		return err
	}
	n, err := io.CopyN(f, r, h.Size)
	if err != nil {
		f.Close()
		return seedRefused("read %d of %d bytes: %v", n, h.Size, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The modification time decides which transcript is newest; a failure
	// to keep it changes nothing else.
	if !h.ModTime.IsZero() {
		_ = dir.Chtimes(p, h.ModTime, h.ModTime)
	}
	return nil
}

// seedName checks an entry's name. keep is false for the top directory
// itself.
func seedName(raw string, maxDepth int) (string, bool, error) {
	name := strings.TrimSuffix(raw, "/")
	name = strings.TrimPrefix(name, "./")
	if raw == "./" || raw == "." {
		return "", false, nil // the top directory itself, as tar -C dir . writes it
	}
	switch {
	case name == "" || name == "." || len(name) > 4096:
		return "", false, seedRefused("an entry name is empty, \".\" or longer than 4096 bytes")
	case strings.HasPrefix(name, "/"):
		return "", false, seedRefused("%q is absolute", raw)
	case strings.ContainsRune(name, 0):
		return "", false, seedRefused("an entry name holds a NUL")
	case path.Clean(name) != name:
		return "", false, seedRefused("%q is not a clean path", raw)
	}
	parts := strings.Split(name, "/")
	if len(parts) > maxDepth {
		return "", false, seedRefused("%q is nested deeper than %d", raw, maxDepth)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || len(p) > 255 {
			return "", false, seedRefused("%q has an empty, dot, dot-dot or over-long component", raw)
		}
	}
	return name, true, nil
}
