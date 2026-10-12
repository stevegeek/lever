package state

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stevegeek/lever/internal/testutil"
)

func TestServesHere(t *testing.T) {
	root := t.TempDir()
	st := ForConfig(root)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.MkdirAll(ForConfig(other).Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "lever.yaml")
	for name, c := range map[string]struct {
		argv []string
		want bool
	}{
		"apply's broker":               {[]string{"/usr/local/bin/lever", "broker", "serve", cfg}, true},
		"by hand, relative config":     {[]string{"lever", "broker", "serve", "lever.yaml"}, true},
		"by hand, no config":           {[]string{"lever", "broker", "serve"}, true},
		"another instance's broker":    {[]string{"lever", "broker", "serve", filepath.Join(other, "lever.yaml")}, false},
		"the remote proxy":             {[]string{"lever", "remote", "serve", cfg}, false},
		"another lever command":        {[]string{"lever", "broker", "status", cfg}, false},
		"an unrelated process":         {[]string{"/usr/bin/sleep", "60"}, false},
		"broker as argv[0] only":       {[]string{"broker", "serve"}, false},
		"empty":                        {nil, false},
		"a config that does not exist": {[]string{"lever", "broker", "serve", "/nonexistent/lever.yaml"}, false},
	} {
		if got := st.servesHere(c.argv, BrokerDaemon); got != c.want {
			t.Errorf("%s: servesHere(%q) = %v, want %v", name, c.argv, got, c.want)
		}
	}
}

func TestParseProcArgs2(t *testing.T) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, 4)
	buf = append(buf, "/usr/local/bin/lever\x00\x00\x00\x00lever\x00broker\x00serve\x00/Users/a b/lever.yaml\x00HOME=/Users/a\x00"...)
	argv, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lever", "broker", "serve", "/Users/a b/lever.yaml"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %q, want %q", argv, want)
		}
	}
	for name, bad := range map[string][]byte{
		"short":     {1, 0},
		"truncated": append(append([]byte{}, buf[:4]...), "/x\x00lever\x00broker"...),
		"no argc":   append([]byte{0, 0, 0, 0}, "/x\x00a\x00"...),
	} {
		if _, err := parseProcArgs2(bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// A real process: ServeProcess recognises the argv of a lever daemon for
// this state dir and nothing else; a pid that is gone is not one, nor is
// this test process.
func TestServeProcessReadsARealProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("argv is read on linux and darwin only")
	}
	root := t.TempDir()
	st := ForConfig(root)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, done := testutil.StartServeProcess(t, BrokerDaemon, filepath.Join(root, "lever.yaml"))
	pid := cmd.Process.Pid
	if !st.ServeProcess(pid, BrokerDaemon) {
		args, err := processArgs(pid)
		t.Fatalf("a lever broker for this dir was not recognised: argv %q, %v", args, err)
	}
	if st.ServeProcess(pid, RemoteDaemon) {
		t.Fatal("the broker was taken for the remote proxy")
	}
	if st.ServeProcess(os.Getpid(), BrokerDaemon) {
		t.Fatal("this test process was taken for a broker")
	}
	if err := os.WriteFile(st.PID(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, alive := st.BrokerStatus(); !found || alive {
		t.Fatalf("a live pid that is not a broker: found=%v alive=%v, want a stale file", found, alive)
	}
	if err := os.WriteFile(st.PID(), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, alive := st.BrokerStatus(); !found || !alive {
		t.Fatalf("the broker: found=%v alive=%v", found, alive)
	}
	_ = cmd.Process.Kill()
	<-done
	if st.ServeProcess(pid, BrokerDaemon) {
		t.Fatal("a process that is gone was recognised")
	}
}
