package jail

// AttachArgv builds the host argv to attach an interactive scion command INSIDE
// the jail. It mirrors Runner.RunIn's orb+env prefix (see runner.go) but returns
// the argv for the caller to exec() directly — interactive TTY handover can't go
// through the Runner. inner is the in-jail command (e.g. the argv from
// scion.Client.AttachArgv). The shared jail env comes from jailEnvFor.
func AttachArgv(machine, user, uid string, inner []string) []string {
	argv := []string{"orb", "-m", machine, "-u", user, "env"}
	argv = append(argv, jailEnvFor(uid)...)
	return append(argv, inner...)
}
