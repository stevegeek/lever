package daemon

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWritePIDFileRecordsThisProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "x.pid") // parent dir is created
	if err := WritePIDFile(p); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || got != os.Getpid() {
		t.Fatalf("pid file = %q, want %d", string(b), os.Getpid())
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pid mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestPIDFileEmptyPathIsNoop(t *testing.T) {
	if err := WritePIDFile(""); err != nil {
		t.Fatal(err)
	}
	RemovePIDFile("")
}

func TestRemovePIDFileIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.pid")
	RemovePIDFile(p) // absent → no error, no panic
	if err := WritePIDFile(p); err != nil {
		t.Fatal(err)
	}
	RemovePIDFile(p)
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("pid file should be gone after RemovePIDFile")
	}
}

func TestCloseListenersTolerantOfNilAndClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	CloseListeners(nil, ln)
	CloseListeners(ln) // double close is benign
	if _, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		t.Fatal("listener still accepting after CloseListeners")
	}
}

func TestWarnfUsesLeverWarningPrefix(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	Warnf("thing %d failed: %v", 7, "boom")
	os.Stderr = old
	_ = w.Close()
	got, _ := io.ReadAll(r)
	if want := "lever: warning: thing 7 failed: boom\n"; string(got) != want {
		t.Fatalf("Warnf wrote %q, want %q", got, want)
	}
}
