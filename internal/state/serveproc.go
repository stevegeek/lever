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

// ServeProcess reports whether pid is a live `<lever> <sub> serve [config]`
// process that serves this state directory: its argv names the subcommand,
// and when its config argument is an absolute path, the state directory
// beside that config is s.Dir (the same directory, sameDir). apply starts the daemons with the absolute
// config path (detachedSelfCmd); a relative or absent config (a daemon an
// operator started by hand) is accepted on the subcommand alone. The
// executable is not compared: an upgrade replaces it under a running
// daemon, and the binary can have any name. A process whose argv cannot be
// read (gone, a zombie, another user's, an OS this does not support) is not
// one.
func (s State) ServeProcess(pid int, sub string) bool {
	if pid <= 0 || !ProcessAlive(pid) {
		return false
	}
	argv, err := processArgs(pid)
	if err != nil {
		return false
	}
	return s.servesHere(argv, sub)
}

// servesHere is ServeProcess's decision on an argv.
func (s State) servesHere(argv []string, sub string) bool {
	if len(argv) < 3 || argv[1] != sub || argv[2] != "serve" {
		return false
	}
	if len(argv) == 3 {
		return true
	}
	cfg := argv[len(argv)-1]
	if strings.HasPrefix(cfg, "-") || !filepath.IsAbs(cfg) {
		return true
	}
	return sameDir(ForConfig(filepath.Dir(cfg)).Dir, s.Dir)
}

// ServeStatus is PIDStatus for a daemon's pid file, where alive also
// requires the process to be that daemon for this state directory
// (ServeProcess): a pid the OS gave to another process reads as a stale
// file, found but not alive.
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

// errNoArgs means a process's argv could not be read on this OS.
var errNoArgs = errors.New("state: cannot read a process's arguments here")

// parseProcArgs2 decodes macOS's kern.procargs2 buffer: the argument count
// (a native-endian int32; every Mac lever runs on is little-endian), the
// executable path, NUL padding, then that many NUL-terminated arguments.
// Pure, so it is tested on every OS.
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
