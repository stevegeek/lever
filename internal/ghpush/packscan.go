package ghpush

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// PackLimits bound what git index-pack must inflate and hold in memory for
// one bundle. A small, highly compressible pack can declare objects of many
// GiB; index-pack holds a delta base and its result in memory to resolve a
// delta, whatever core.bigFileThreshold says. Zero fields are not checked.
type PackLimits struct {
	MaxObjects    uint32 // objects in the pack
	MaxObjectSize int64  // inflated size of one object, delta base or delta result
	MaxInflated   int64  // sum of the inflated object and delta-result sizes
}

// ErrPackLimit: the bundle's pack is over a PackLimits cap or malformed.
var ErrPackLimit = errors.New("bundle pack refused")

// maxBundleHeader caps the bundle header (version line, capabilities,
// prerequisites, refs) the scanner reads before the pack.
const maxBundleHeader = 16 << 20

// scanBundle walks the pack in the bundle at path, inflating each object
// with bounded memory, and refuses it when it is over a limit. It reads the
// header as git does (read_bundle_header): lines up to the first blank one.
// It treats more characters as blank than git, so where the two disagree the
// scanner stops first and git then fails on a "PACK" header line.
func scanBundle(path string, l PackLimits) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	hashLen := 20
	var header int
	for i := 0; ; i++ {
		line, err := readLine(r, maxBundleHeader-header)
		if err != nil {
			return fmt.Errorf("%w: bundle header: %v", ErrPackLimit, err)
		}
		header += len(line)
		t := string(bytes.TrimRight(line, " \t\r\n\v\f"))
		if i == 0 {
			if t != "# v2 git bundle" && t != "# v3 git bundle" {
				return fmt.Errorf("%w: not a v2 or v3 git bundle", ErrPackLimit)
			}
			continue
		}
		if t == "" {
			break
		}
		if t == "@object-format=sha256" {
			hashLen = 32
		}
	}
	var ph [12]byte
	if _, err := io.ReadFull(r, ph[:]); err != nil {
		return fmt.Errorf("%w: pack header: %v", ErrPackLimit, err)
	}
	if string(ph[:4]) != "PACK" {
		return fmt.Errorf("%w: no pack after the bundle header", ErrPackLimit)
	}
	if v := binary.BigEndian.Uint32(ph[4:8]); v != 2 && v != 3 {
		return fmt.Errorf("%w: pack version %d", ErrPackLimit, v)
	}
	count := binary.BigEndian.Uint32(ph[8:12])
	if l.MaxObjects > 0 && count > l.MaxObjects {
		return fmt.Errorf("%w: %d objects (max %d)", ErrPackLimit, count, l.MaxObjects)
	}
	var total int64
	var zr io.ReadCloser
	for i := uint32(0); i < count; i++ {
		typ, size, err := readObjectHeader(r)
		if err != nil {
			return fmt.Errorf("%w: object %d: %v", ErrPackLimit, i, err)
		}
		switch typ {
		case 1, 2, 3, 4: // commit, tree, blob, tag
		case 6: // ofs-delta: a base offset, 7 bits per byte
			for n := 0; ; n++ {
				c, err := r.ReadByte()
				if err != nil || n == 10 {
					return fmt.Errorf("%w: object %d: bad delta offset", ErrPackLimit, i)
				}
				if c&0x80 == 0 {
					break
				}
			}
		case 7: // ref-delta: a base object id
			if _, err := r.Discard(hashLen); err != nil {
				return fmt.Errorf("%w: object %d: %v", ErrPackLimit, i, err)
			}
		default:
			return fmt.Errorf("%w: object %d: bad type %d", ErrPackLimit, i, typ)
		}
		if err := checkSize(l, "object", i, size); err != nil {
			return err
		}
		if zr == nil {
			if zr, err = zlib.NewReader(r); err != nil {
				return fmt.Errorf("%w: object %d: %v", ErrPackLimit, i, err)
			}
		} else if err := zr.(zlib.Resetter).Reset(r, nil); err != nil {
			return fmt.Errorf("%w: object %d: %v", ErrPackLimit, i, err)
		}
		lr := &countingReader{r: zr}
		inflated := size
		if typ == 6 || typ == 7 {
			base, err1 := readDeltaSize(lr)
			result, err2 := readDeltaSize(lr)
			if err1 != nil || err2 != nil {
				return fmt.Errorf("%w: object %d: bad delta header", ErrPackLimit, i)
			}
			if err := checkSize(l, "delta base", i, base); err != nil {
				return err
			}
			if err := checkSize(l, "delta result", i, result); err != nil {
				return err
			}
			inflated = result
		}
		total += inflated
		if l.MaxInflated > 0 && total > l.MaxInflated {
			return fmt.Errorf("%w: objects inflate to more than %d bytes", ErrPackLimit, l.MaxInflated)
		}
		// Inflate at most the declared size plus one byte: the work stays
		// bounded by the sizes checked above.
		rest := size - lr.n
		n, err := io.CopyN(io.Discard, lr, rest+1)
		if n > rest {
			return fmt.Errorf("%w: object %d inflates past its declared size", ErrPackLimit, i)
		}
		if err != io.EOF || n != rest {
			return fmt.Errorf("%w: object %d: truncated or corrupt data: %v", ErrPackLimit, i, err)
		}
	}
	return nil
}

func checkSize(l PackLimits, what string, i uint32, size int64) error {
	if l.MaxObjectSize > 0 && size > l.MaxObjectSize {
		return fmt.Errorf("%w: %s %d is %d bytes inflated (max %d)", ErrPackLimit, what, i, size, l.MaxObjectSize)
	}
	return nil
}

// readLine reads one line of at most max bytes, the newline included.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return nil, fmt.Errorf("longer than %d bytes", maxBundleHeader)
		}
		if err == nil {
			return line, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

// readObjectHeader reads a pack entry header: type in bits 4-6 of the first
// byte, size in its low 4 bits and 7 bits per following byte.
func readObjectHeader(r *bufio.Reader) (int, int64, error) {
	c, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	typ := int(c>>4) & 7
	size := uint64(c & 0x0f)
	for shift := uint(4); c&0x80 != 0; shift += 7 {
		if shift > 56 {
			return 0, 0, errors.New("size overflows")
		}
		if c, err = r.ReadByte(); err != nil {
			return 0, 0, err
		}
		size |= uint64(c&0x7f) << shift
	}
	return typ, int64(size), nil
}

// readDeltaSize reads one size of a delta header: 7 bits per byte, low
// bits first.
func readDeltaSize(r io.Reader) (int64, error) {
	var size uint64
	var b [1]byte
	for shift := uint(0); ; shift += 7 {
		if shift > 56 {
			return 0, errors.New("size overflows")
		}
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		size |= uint64(b[0]&0x7f) << shift
		if b[0]&0x80 == 0 {
			return int64(size), nil
		}
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
