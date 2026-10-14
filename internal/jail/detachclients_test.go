package jail

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestAgentProbeDetachClients(t *testing.T) {
	for _, c := range []struct {
		name    string
		res     proc.Result
		want    int
		wantErr bool
	}{
		{"two detached", proc.Result{Stdout: "d\nd\n"}, 2, false},
		{"none", proc.Result{}, 0, false},
		{"other output is not counted", proc.Result{Stdout: "x\nd d\n"}, 2, false},
		{"container not running", proc.Result{Code: 125, Stderr: "Error: can only create exec sessions on running containers"}, 0, true},
		{"no such container", proc.Result{Code: 125, Stderr: "Error: no such container lever--assistant"}, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls []string
			jr := New(Config{Host: codeRunner{c.res, &calls}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
			n, err := AgentProbe{R: jr}.DetachClients(context.Background(), "lever--assistant")
			if n != c.want || (err != nil) != c.wantErr {
				t.Fatalf("n=%d err=%v, want %d err=%t", n, err, c.want, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "exec sessions") {
				t.Errorf("no container text may reach the error: %v", err)
			}
			if len(calls) != 1 || !strings.Contains(calls[0], "podman exec --user scion lever--assistant sh -c") {
				t.Errorf("calls %q", calls)
			}
		})
	}
}

func TestAgentProbeDetachClientsRefusesFlagRef(t *testing.T) {
	host := proc.NewFakeRunner()
	if _, err := (AgentProbe{R: host}).DetachClients(context.Background(), "--all"); err == nil || len(host.Calls) != 0 {
		t.Fatal("a flag-shaped ref must be refused before any call")
	}
}

// runDetachScript runs detachClientsScript under a real sh with a fake
// tmux on PATH that lists clients and logs each detach-client, and a fake
// /proc. It returns the script's output and the detach log.
func runDetachScript(t *testing.T, clients string, ppids map[string]string, listFails bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	procRoot := filepath.Join(dir, "proc")
	log := filepath.Join(dir, "detach.log")
	for _, d := range []string{bin, procRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	list := "printf '%s' \"$FAKE_CLIENTS\""
	if listFails {
		list = "echo 'no server running on /tmp/tmux-1000/default' >&2; exit 1"
	}
	tmux := "#!/bin/sh\ncase \"$1\" in\nlist-clients) " + list + " ;;\ndetach-client) echo \"$*\" >> \"$FAKE_LOG\" ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(tmux), 0o755); err != nil {
		t.Fatal(err)
	}
	for pid, ppid := range ppids {
		if err := os.MkdirAll(filepath.Join(procRoot, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		status := "Name:\ttmux: client\nState:\tS (sleeping)\nPPid:\t" + ppid + "\nUid:\t1000\n"
		if err := os.WriteFile(filepath.Join(procRoot, pid, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", detachClientsScript, "sh", procRoot)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_CLIENTS="+clients, "FAKE_LOG="+log)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v: %s", err, out)
	}
	logged, _ := os.ReadFile(log)
	return string(out), string(logged)
}

// TestDetachClientsScriptSparesTheContainersClient: under podman the
// container's main process is itself a tmux client of the scion session
// (scion's start command ends in attach-session). Detaching it would stop
// the agent, so only clients a podman exec started (PPid 0 in the
// container's pid namespace) are detached.
func TestDetachClientsScriptSparesTheContainersClient(t *testing.T) {
	clients := "7 /dev/pts/0\n412 /dev/pts/1\n530 /dev/pts/2\n"
	out, logged := runDetachScript(t, clients, map[string]string{"7": "1", "412": "0", "530": "0"}, false)
	if strings.Count(out, "d\n") != 2 || strings.Contains(out, "No such file") {
		t.Errorf("output %q, want two detaches", out)
	}
	if strings.Contains(logged, "/dev/pts/0") {
		t.Fatalf("the container's own client was detached: %q", logged)
	}
	if !strings.Contains(logged, "detach-client -t /dev/pts/1") || !strings.Contains(logged, "detach-client -t /dev/pts/2") {
		t.Errorf("exec'd clients not detached: %q", logged)
	}
}

// A client whose status cannot be read, an odd pid, no tty, or PID 1
// (PPid 0 too, but the container's main process) is left attached; no
// server lists nothing and detaches nothing.
func TestDetachClientsScriptFailsSafe(t *testing.T) {
	clients := "999 /dev/pts/3\nabc /dev/pts/4\n412\n-1 /dev/pts/5\n1 /dev/console\n"
	out, logged := runDetachScript(t, clients, map[string]string{"412": "0", "1": "0"}, false)
	if strings.TrimSpace(out) != "" || logged != "" {
		t.Errorf("out %q log %q, want nothing detached", out, logged)
	}
	out, logged = runDetachScript(t, "", nil, true)
	if strings.TrimSpace(out) != "" || logged != "" {
		t.Errorf("no server: out %q log %q", out, logged)
	}
}
