//go:build linux || darwin

package treebackup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// peakHeap samples the heap in use while f runs and returns the most it
// grew above where it started.
func peakHeap(f func()) uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	start := ms.HeapInuse
	var peak atomic.Uint64
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for !stop.Load() {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	f()
	stop.Store(true)
	<-done
	if p := peak.Load(); p > start {
		return p - start
	}
	return 0
}

// longPathTree fills a tree with about n empty files whose paths are about
// 4000 bytes long: the cheapest way to fill the manifest budget with path
// bytes.
func longPathTree(t *testing.T, tree string, n int) string {
	t.Helper()
	d := tree
	for i := 0; i < 15; i++ {
		d = filepath.Join(d, fmt.Sprintf("%02d", i)+strings.Repeat("d", 248))
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%08d", i) + strings.Repeat("f", 242)
		if err := os.WriteFile(filepath.Join(d, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// The memory a snapshot holds is bounded by a small multiple of
// limits.manifest, also in the worst case the review found: long paths, and
// an incomplete newest snapshot so that the cache base and the known base
// are two different manifests.
func TestSnapshotMemoryIsBoundedByTheManifestBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	const manifest = 24 << 20
	e := newSnapEnv(t)
	n := manifest / 4500
	deep := longPathTree(t, e.tree, n)
	o := e.opts(TriggerManual)
	o.Limits.Manifest = manifest
	if _, err := Snapshot(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Limits.DirEntries = int64(n - 10)
	res, err := Snapshot(context.Background(), o)
	if err != nil || res.Header.Complete {
		t.Fatalf("second snapshot: %v (it must be incomplete)", err)
	}
	os.WriteFile(filepath.Join(deep, "x"), nil, 0o600)
	o.Limits.DirEntries = DefaultLimits().DirEntries
	var h *Header
	grew := peakHeap(func() {
		res, err := Snapshot(context.Background(), o)
		if err != nil {
			t.Error(err)
			return
		}
		h = res.Header
	})
	if h == nil {
		return
	}
	if h.CacheBase == h.KnownBase {
		t.Fatalf("the bases are one manifest (%s): the case under test needs two", h.CacheBase)
	}
	ratio := float64(grew) / manifest
	t.Logf("%d files, heap grew %d MiB, %.2f x limits.manifest", h.Counts.ByType[TypeFile], grew>>20, ratio)
	// The documented bound, with room for what sampling HeapInuse adds
	// (fragmentation, the test's own allocations).
	if ratio > memoryPerManifest+0.5 {
		t.Fatalf("heap grew %.2f x limits.manifest; the documented bound is about %d x", ratio, memoryPerManifest)
	}
}
