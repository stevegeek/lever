package ghpush

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type packEntry struct {
	typ  int
	size int64  // declared in the entry header
	data []byte // inflated payload (for a delta: the delta stream)
}

func objHeader(typ int, size int64) []byte {
	b := []byte{byte(typ<<4) | byte(size&0x0f)}
	size >>= 4
	for size > 0 {
		b[len(b)-1] |= 0x80
		b = append(b, byte(size&0x7f))
		size >>= 7
	}
	return b
}

func deltaSize(n int64) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// fakeBundle writes a v2 bundle with a hand-built pack (no valid trailer:
// the scanner does not check it).
func fakeBundle(t *testing.T, header string, entries ...packEntry) string {
	t.Helper()
	var buf bytes.Buffer
	if header == "" {
		header = "# v2 git bundle\n" + strings.Repeat("a", 40) + " refs/heads/agent/x\n\n"
	}
	buf.WriteString(header)
	buf.WriteString("PACK")
	binary.Write(&buf, binary.BigEndian, uint32(2))
	binary.Write(&buf, binary.BigEndian, uint32(len(entries)))
	for _, e := range entries {
		buf.Write(objHeader(e.typ, e.size))
		if e.typ == 7 {
			buf.Write(make([]byte, 20))
		}
		zw := zlib.NewWriter(&buf)
		zw.Write(e.data)
		zw.Close()
	}
	buf.Write(make([]byte, 20))
	p := filepath.Join(t.TempDir(), "x.bundle")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func blob(n int) packEntry { return packEntry{typ: 3, size: int64(n), data: make([]byte, n)} }

func TestScanBundleLimits(t *testing.T) {
	l := PackLimits{MaxObjects: 3, MaxObjectSize: 1 << 20, MaxInflated: 3 << 20}
	hugeDelta := append(deltaSize(10), deltaSize(1<<40)...)
	bigBase := append(deltaSize(1<<40), deltaSize(10)...)
	okDelta := append(deltaSize(10), deltaSize(10)...)
	// A delta with a tiny result but 1 MiB of instruction data: git still
	// inflates the whole stream, so it counts toward the total.
	fatDelta := append(append(deltaSize(10), deltaSize(10)...), make([]byte, 1<<20-2)...)
	cases := []struct {
		name    string
		header  string
		entries []packEntry
		want    string // "" = accepted
	}{
		{"small blobs", "", []packEntry{blob(10), blob(1 << 20)}, ""},
		{"ref delta", "", []packEntry{{typ: 7, size: int64(len(okDelta)), data: okDelta}}, ""},
		{"too many objects", "", []packEntry{blob(1), blob(1), blob(1), blob(1)}, "4 objects"},
		{"object too big", "", []packEntry{blob(1<<20 + 1)}, "object 0 is"},
		{"delta result too big", "", []packEntry{{typ: 7, size: int64(len(hugeDelta)), data: hugeDelta}}, "delta result 0"},
		{"delta base too big", "", []packEntry{{typ: 7, size: int64(len(bigBase)), data: bigBase}}, "delta base 0"},
		{"at the inflated cap", "", []packEntry{blob(1 << 20), blob(1 << 20), blob(1 << 20)}, ""},
		{"delta data counts toward the total", "", []packEntry{
			{typ: 7, size: int64(len(fatDelta)), data: fatDelta},
			{typ: 7, size: int64(len(fatDelta)), data: fatDelta},
			{typ: 7, size: int64(len(fatDelta)), data: fatDelta}}, "inflate to more than"},
		{"inflates past size", "", []packEntry{{typ: 3, size: 10, data: make([]byte, 5000)}}, "past its declared size"},
		{"truncated object", "", []packEntry{{typ: 3, size: 5000, data: make([]byte, 10)}}, "truncated"},
		{"bad type", "", []packEntry{{typ: 5, size: 1, data: []byte{0}}}, "bad type"},
		{"not a bundle", "hello\n\n", nil, "not a v2 or v3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := scanBundle(context.Background(), fakeBundle(t, c.header, c.entries...), l)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if !errors.Is(err, ErrPackLimit) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want ErrPackLimit containing %q, got %v", c.want, err)
			}
		})
	}
	over := PackLimits{MaxInflated: 2<<20 + 1}
	if err := scanBundle(context.Background(), fakeBundle(t, "", blob(1<<20), blob(1<<20), blob(1)), over); err != nil {
		t.Fatalf("at the inflated cap: %v", err)
	}
	if err := scanBundle(context.Background(), fakeBundle(t, "", blob(1<<20), blob(1<<20), blob(2)), over); err == nil || !strings.Contains(err.Error(), "inflate to more than") {
		t.Fatalf("want an inflated-sum refusal, got %v", err)
	}
}

func TestScanBundleAcceptsGitBundles(t *testing.T) {
	_, work := newTestRemote(t, repo)
	dir := t.TempDir()
	commitOn(t, work, "agent/s", "s1.txt")
	// A large file edited twice: the full bundle carries a delta.
	big := bytes.Repeat([]byte("lever line of text for a delta\n"), 8000)
	os.WriteFile(filepath.Join(work, "big.txt"), big, 0o644)
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-q", "-m", "big")
	os.WriteFile(filepath.Join(work, "big.txt"), append(big, "one more\n"...), 0o644)
	gitT(t, work, "commit", "-q", "-am", "big 2")
	gitT(t, work, "bundle", "create", filepath.Join(dir, "full.bundle"), "agent/s")
	gitT(t, work, "bundle", "create", filepath.Join(dir, "inc.bundle"), "origin/main..agent/s")
	// Make sure the test covers a delta: index the full bundle's pack.
	bare := filepath.Join(dir, "check.git")
	gitT(t, dir, "init", "-q", "--bare", bare)
	gitT(t, bare, "fetch", "-q", filepath.Join(dir, "full.bundle"), "agent/s:agent/s")
	idx, _ := filepath.Glob(filepath.Join(bare, "objects", "pack", "*.idx"))
	if len(idx) != 1 || !strings.Contains(gitT(t, bare, "verify-pack", "-v", idx[0]), "chain length") {
		t.Fatal("the full bundle carries no delta")
	}
	for _, b := range []string{"full.bundle", "inc.bundle"} {
		if err := scanBundle(context.Background(), filepath.Join(dir, b), PackLimits{MaxObjects: 100, MaxObjectSize: 1 << 20, MaxInflated: 1 << 20}); err != nil {
			t.Errorf("%s: %v", b, err)
		}
	}
}

func TestPushRefusesPackOverLimitsBeforeImport(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, argvs := newPusher(t, r)
	p.Limits = PackLimits{MaxObjectSize: 1 << 20}
	gitT(t, work, "checkout", "-q", "-B", "agent/big", "origin/main")
	// 2 MiB of zeros: a bundle of a few KiB.
	if err := os.WriteFile(filepath.Join(work, "zero.bin"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-q", "-m", "zeros")
	bundle(t, work, tree, "big.bundle", "origin/main..agent/big")
	_, err := p.Push(context.Background(), "manager", repo, "agent/big", "big.bundle")
	if !errors.Is(err, ErrPackLimit) {
		t.Fatalf("want ErrPackLimit, got %v", err)
	}
	for _, a := range *argvs {
		if slices.Contains(a, "bundle") || slices.Contains(a, "fetch") {
			t.Fatalf("git ran on the refused bundle: %v", a)
		}
	}
	if r.headSHA(t, repo, "agent/big") != "" {
		t.Fatal("pushed")
	}
}
