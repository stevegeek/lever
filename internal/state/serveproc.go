package state

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
)

// Which process a pid file names. broker.pid and remote.pid record the pid
// of `lever broker serve` and `lever remote serve`, but a pid file can
// outlive its process (a crash, a reboot, a SIGKILL) and the OS can give the
// pid to another process. Signal 0 says only that SOME process has it. A
// caller that would stop the process, or restart a broker on the strength of
// it, first checks the process is a lever serve process for THIS state
// directory (ServeProcess); a live pid that is not one is a stale pid file.

// Daemon subcommands, as `lever <sub> serve` runs them.
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
	// IsServe: the daemon of this state directory (ServeProcess).
	IsServe
	// Unknown: a live process whose argv could not be read. Nothing may be
	// signalled on it, and its pid file must not be discarded either: it
	// may be the daemon.
	Unknown
)

// Identify says whether pid is a live `<lever> <sub> serve [config]`
// process that serves this state directory: its argv names the subcommand,
// and when its config argument (the first argument after `serve` that is
// not a flag) is an absolute path, the state directory beside that config
// is s.Dir (the same directory, sameDir). apply starts the daemons with the
// absolute config path (detachedSelfCmd); a relative or absent config (a
// daemon an operator started by hand) is accepted on the subcommand alone.
// The executable is not compared: an upgrade replaces it under a running
// daemon, and the binary can have any name.
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

// ServeProcess reports whether Identify says IsServe.
func (s State) ServeProcess(pid int, sub string) bool {
	return s.Identify(pid, sub) == IsServe
}

// servesHere is Identify's decision on an argv.
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

// ServeStatus is PIDStatus for a daemon's pid file, where alive also
// requires the process to be that daemon for this state directory
// (ServeProcess): a pid the OS gave to another process reads as a stale
// file, found but not alive. So does a process whose argv cannot be read:
// a caller that would act on alive (reuse or restart a daemon) must not act
// on a guess.
func (s State) ServeStatus(path, sub string) (pid int, found, alive bool) {
	pid, found, alive = PIDStatus(path)
	if alive && !s.ServeProcess(pid, sub) {
		alive = false
	}
	return pid, found, alive
}

// BrokerStatus is ServeStatus for broker.pid.
func (s State) BrokerStatus() (pid int, found, alive bool) {
	return s.ServeStatus(s.PID(), BrokerDaemon)
}

// RemoteStatus is ServeStatus for remote.pid.
func (s State) RemoteStatus() (pid int, found, alive bool) {
	return s.ServeStatus(s.RemotePID(), RemoteDaemon)
}

// errNoArgs means a process's argv could not be read (on this OS, or in
// the form expected): Identify says Unknown.
var errNoArgs = errors.New("state: cannot read the process's arguments")

// errNotAProcess means the pid is not a live user process with an argv: it
// is gone, a zombie, or (Linux) a kernel thread. Identify says NotServe.
var errNotAProcess = errors.New("state: not a live process with arguments")

// parseProcArgs2 decodes macOS's kern.procargs2 buffer: the argument count
// (a native-endian int32; every Mac lever runs on is little-endian), the
// executable path, NUL padding, then that many NUL-terminated arguments.
// Pure, so it is tested on every OS. An empty argv[0] is skipped with the
// padding, shifting argv by one; only a process the host user controls
// could do that, and such a process can signal anything of that user's
// anyway.
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
