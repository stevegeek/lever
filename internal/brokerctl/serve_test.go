package brokerctl

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestWritePIDFileRecordsThisProcess(t *testing.T) {
	st := StateDir(t.TempDir())
	if err := writePIDFile(st); err != nil {
		t.Fatalf("writePIDFile: %v", err)
	}
	b, err := os.ReadFile(st.PID())
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || got != os.Getpid() {
		t.Fatalf("pid file = %q, want %d", string(b), os.Getpid())
	}
	fi, _ := os.Stat(st.PID())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pid mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestRemovePIDFileIsIdempotent(t *testing.T) {
	st := StateDir(t.TempDir())
	removePIDFile(st) // absent → no error, no panic
	if err := writePIDFile(st); err != nil {
		t.Fatal(err)
	}
	removePIDFile(st)
	if _, err := os.Stat(st.PID()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("pid file should be gone after removePIDFile")
	}
}
