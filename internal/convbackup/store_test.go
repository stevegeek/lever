package convbackup

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) Store {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return Store{Dir: filepath.Join(state, DirName)}
}

func save(t *testing.T, s Store, at time.Time) Meta {
	t.Helper()
	src := mkTar(t, entry{name: "projects/-workspace/a.jsonl", typ: tar.TypeReg, body: "hi\n"})
	m, err := s.Save(Meta{Agent: "mgr", Created: at, Reason: "manual"}, func(w io.Writer) (Stats, error) {
		return Copy(w, bytes.NewReader(src), "projects", DefaultLimits)
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestStoreSavesPrivately: the store directory and each backup are 0700,
// the files 0600, and the backup lists, reads and opens.
func TestStoreSavesPrivately(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 10, 12, 9, 30, 5, 0, time.UTC)
	m := save(t, s, at)
	if m.ID != "20261012T093005Z" || m.Files != 1 || m.Newest != "-workspace/a.jsonl" {
		t.Fatalf("meta = %+v", m)
	}
	for p, want := range map[string]os.FileMode{
		s.Dir:                                    0o700,
		s.Path(m.ID):                             0o700,
		filepath.Join(s.Path(m.ID), archiveName): 0o600,
		filepath.Join(s.Path(m.ID), metaName):    0o600,
	} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %v", p, fi, err, want)
		}
	}
	// A second backup in the same second gets a counter.
	m2 := save(t, s, at)
	if m2.ID != "20261012T093005Z-2" {
		t.Fatalf("second id = %q", m2.ID)
	}
	got, err := s.Get("")
	if err != nil || got.ID != m2.ID {
		t.Fatalf("newest = %+v %v", got, err)
	}
	rc, err := s.Open(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	st, err := Copy(io.Discard, rc, "", DefaultLimits)
	if err != nil || st.Files != 1 {
		t.Fatalf("stored archive: %+v %v", st, err)
	}
	if _, err := s.Get("20200101T000000Z"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.Open("../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

// TestStoreFailedWriteLeavesNothing: a write that fails leaves no backup and
// no temporary directory.
func TestStoreFailedWriteLeavesNothing(t *testing.T) {
	s := newStore(t)
	_, err := s.Save(Meta{Agent: "mgr"}, func(w io.Writer) (Stats, error) {
		_, _ = w.Write([]byte("partial"))
		return Stats{}, errors.New("boom")
	})
	if err == nil {
		t.Fatal("want an error")
	}
	names, _ := os.ReadDir(s.Dir)
	if len(names) != 0 {
		t.Fatalf("left behind: %v", names)
	}
}

// TestStoreRefusesUnsafeDir: a store another user could read, or a link in
// its place, is refused.
func TestStoreRefusesUnsafeDir(t *testing.T) {
	s := newStore(t)
	if err := os.Mkdir(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Meta{}, func(io.Writer) (Stats, error) { return Stats{}, nil }); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("0755: %v", err)
	}
	if _, err := s.List(); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("list 0755: %v", err)
	}

	s2 := newStore(t)
	target := t.TempDir()
	if err := os.Symlink(target, s2.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Save(Meta{}, func(io.Writer) (Stats, error) { return Stats{}, nil }); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlink: %v", err)
	}
}

// TestStorePruneKeepsNewest: prune removes the oldest backups and nothing
// else in the directory.
func TestStorePruneKeepsNewest(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 4 {
		ids = append(ids, save(t, s, t0.Add(time.Duration(i)*time.Hour)).ID)
	}
	other := filepath.Join(s.Dir, "keep-me")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Prune(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != ids[1] || removed[1] != ids[0] {
		t.Fatalf("removed = %v (ids %v)", removed, ids)
	}
	all, _ := s.List()
	if len(all) != 2 || all[0].ID != ids[3] || all[1].ID != ids[2] {
		t.Fatalf("left = %+v", all)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("prune removed a file that is not a backup: %v", err)
	}
}

// TestStoreListMissingIsEmpty: no store directory is no backups.
func TestStoreListMissingIsEmpty(t *testing.T) {
	s := newStore(t)
	if all, err := s.List(); err != nil || len(all) != 0 {
		t.Fatalf("%v %v", all, err)
	}
	if _, err := s.Get(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get newest of none: %v", err)
	}
}
