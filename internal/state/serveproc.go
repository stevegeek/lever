package state

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
)

// A pid file can outlive its process (a crash, a reboot, a SIGKILL) and the
// OS can reuse the pid; signal 0 says only that some process has it. A
// caller that would stop or restart a daemon on the strength of a pid first
// checks it is a lever serve process for this state directory
// (ServeProcess); a live pid that is not one is a stale pid file.

const (
	BrokerDaemon = "broker"
	RemoteDaemon = "remote"
)

// Identity is what a pid is, as far as a lever daemon is concerned.
type Identity int

const (
	// NotServe: no such process, a zombie, or a process whose argv is not
	// the daemon's (a reused pid).
	NotServe Identity = iota
	IsServe
	// Unknown: a live process whose argv could not be read. Nothing may be
	// signalled on it, and its pid file must not be discarded: it may be
	// the daemon.
	Unknown
)

// Identify says whether pid is a live `<lever> <sub> serve [config]` process
// for this state directory: its argv names the subcommand and, when its
// config argument is an absolute path, the state directory beside that
// config is s.Dir (sameDir). A relative or absent config (a daemon started
// by hand) is accepted on the subcommand alone. The executable is not
// compared: an upgrade replaces it under a running daemon, and the binary
// can have any name.
func (s State) Identify(pid int, sub string) Identity {
	if pid <= 0 || !ProcessAlive(pid) {
		return NotServe
	}
	argv, err := processArgs(pid)
	switch {
	case errors.Is(err, errNotAProcess):
		return NotServe
	case err != nil:
		return Unknown
	case s.servesHere(argv, sub):
		return IsServe
	}
	return NotServe
}

func (s State) ServeProcess(pid int, sub string) bool {
	return s.Identify(pid, sub) == IsServe
}

func (s State) servesHere(argv []string, sub string) bool {
	if len(argv) < 3 || argv[1] != sub || argv[2] != "serve" {
		return false
	}
	for _, a := range argv[3:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if !filepath.IsAbs(a) {
			return true
		}
		return sameDir(ForConfig(filepath.Dir(a)).Dir, s.Dir)
	}
	return true
}

// ServeStatus is PIDStatus where alive also requires ServeProcess: a reused
// pid, or a process whose argv cannot be read, reads as a stale file (found,
// not alive), so a caller never reuses or restarts a daemon on a guess.
func (s State) ServeStatus(path, sub string) (pid int, found, alive bool) {
	pid, found, alive = PIDStatus(path)
	if alive && !s.ServeProcess(pid, sub) {
		alive = false
	}
	return pid, found, alive
}

func (s State) BrokerStatus() (pid int, found, alive bool) {
	return s.ServeStatus(s.PID(), BrokerDaemon)
}

func (s State) RemoteStatus() (pid int, found, alive bool) {
	return s.ServeStatus(s.RemotePID(), RemoteDaemon)
}

// errNoArgs means a process's argv could not be read: Identify says Unknown.
var errNoArgs = errors.New("state: cannot read the process's arguments")

// errNotAProcess means the pid is not a live user process with an argv: it
// is gone, a zombie, or (Linux) a kernel thread. Identify says NotServe.
var errNotAProcess = errors.New("state: not a live process with arguments")

// parseProcArgs2 decodes macOS's kern.procargs2 buffer: the argument count
// (native-endian int32; every supported Mac is little-endian), the
// executable path, NUL padding, then the NUL-terminated arguments. Pure, so
// it is tested on every OS. An empty argv[0] is skipped with the padding,
// shifting argv by one; only a process the host user controls could do that,
// and it can signal that user's processes anyway.
func parseProcArgs2(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errNoArgs
	}
	argc := int(int32(binary.LittleEndian.Uint32(b)))
	rest := b[4:]
	i := 0
	for i < len(rest) && rest[i] != 0 { // the executable path
		i++
	}
	for i < len(rest) && rest[i] == 0 { // its padding
		i++
	}
	if argc <= 0 || i >= len(rest) {
		return nil, errNoArgs
	}
	rest = rest[i:]
	var argv []string
	for len(argv) < argc {
		j := 0
		for j < len(rest) && rest[j] != 0 {
			j++
		}
		if j == len(rest) {
			return nil, errNoArgs // truncated
		}
		argv = append(argv, string(rest[:j]))
		rest = rest[j+1:]
	}
	return argv, nil
}
