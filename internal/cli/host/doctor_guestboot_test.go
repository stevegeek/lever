package host

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/config"
)

func TestParseGuestBootProbe(t *testing.T) {
	p := parseGuestBootProbe("sudo=1\nlast=1792033445.123456\nclean=0\ngarbage\n")
	if !p.sudo || p.clean || !p.last.Equal(time.Unix(1792033445, 0)) {
		t.Fatalf("parsed %+v", p)
	}
	if p := parseGuestBootProbe("sudo=1\nlast=-- No entries --\nclean=0\n"); !p.last.IsZero() {
		t.Errorf("odd last accepted: %v", p.last)
	}
}

// fakeJournal runs guestBootProbeScript against a sudo on PATH that answers
// the journalctl calls the way the journal would.
func fakeJournal(t *testing.T, last, pid1 string) guestBootProbe {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"[ \"$2\" = true ] && exit 0\n" +
		"case \"$*\" in\n" +
		"*_PID=1*) printf '%s' \"$PID1\" ;;\n" +
		"*short-unix*) printf '%s' \"$LAST\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", guestBootProbeScript)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "LAST="+last, "PID1="+pid1)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	return parseGuestBootProbe(string(out))
}

func TestGuestBootProbeScript(t *testing.T) {
	const last = "1792033445.123456 lever-x systemd[1]: whatever\n"
	if p := fakeJournal(t, last, "Reached target shutdown.target - System Shutdown.\nShutting down.\n"); !p.sudo || !p.clean || p.last.IsZero() {
		t.Errorf("clean shutdown: %+v", p)
	}
	if p := fakeJournal(t, last, "Started session-3.scope.\n"); p.clean || p.last.IsZero() {
		t.Errorf("crash: %+v", p)
	}
	if p := fakeJournal(t, "", ""); !p.last.IsZero() {
		t.Errorf("no previous boot: %+v", p)
	}
}

func TestCheckLastGuestBoot(t *testing.T) {
	logs := t.TempDir()
	clean := guestBootProbe{sudo: true, last: time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC), clean: true}
	crash := clean
	crash.clean = false
	cases := []struct {
		name     string
		backend  string
		p        guestBootProbe
		perr     error
		wantWarn bool
		want     string
	}{
		{"orbstack", config.BackendOrbstack, guestBootProbe{}, nil, false, "not applicable"},
		{"clean", config.BackendLima, clean, nil, false, "ended with a clean shutdown (last journal entry 2026-10-15T03:00:00Z)"},
		{"crash", config.BackendLima, crash, nil, true, "without a clean shutdown (last journal entry 2026-10-15T03:00:00Z)"},
		{"first boot", config.BackendLima, guestBootProbe{sudo: true}, nil, false, "shows no earlier boot"},
		{"no sudo", config.BackendLima, guestBootProbe{}, nil, false, "not checked"},
		{"down", config.BackendLima, guestBootProbe{}, errors.New("vm down"), false, "not checked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := checkLastGuestBoot(tc.backend, filepath.Join(logs, "guest-logs"), tc.p, tc.perr)
			if r.name != "last guest boot" || !r.ok || (r.fix != "") != tc.wantWarn || !strings.Contains(r.detail, tc.want) {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestCheckLastGuestBootPointsAtSavedPanic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "guest-logs")
	set := filepath.Join(dir, "20261015T030405Z")
	if err := os.MkdirAll(set, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(set, "serial.log"), []byte("boot\n[ 9.1] Kernel panic - not syncing: \x1b[31mFatal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clean := guestBootProbe{sudo: true, last: time.Now(), clean: true}
	r := checkLastGuestBoot(config.BackendLima, dir, clean, nil)
	if r.fix == "" || !strings.Contains(r.detail, set) || !strings.Contains(r.detail, "Kernel panic - not syncing:") || strings.Contains(r.detail, "\x1b") {
		t.Fatalf("got %+v", r)
	}
	// The VM being down does not hide the saved panic.
	if r := checkLastGuestBoot(config.BackendLima, dir, guestBootProbe{}, errors.New("down")); r.fix == "" {
		t.Fatalf("got %+v", r)
	}
}
