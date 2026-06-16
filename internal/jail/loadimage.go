package jail

import (
	"context"
	"fmt"
	"os/exec"
)

// LoadImageArgs returns the orb argv that loads a docker-archive (read on stdin)
// into the jail's rootless podman:
//
//	orb -m <machine> -u <user> env XDG_RUNTIME_DIR=/run/user/<uid> podman load
//
// Fallback (if pipe proves unreliable): mount-staging — docker save -o
// <tree>/.img.tar on the host, then in-jail: podman load -i <tree>/.img.tar.
// Implement the pipe first.
func LoadImageArgs(machine, user, uid string) []string {
	return []string{
		"-m", machine,
		"-u", user,
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"podman", "load",
	}
}

// LoadImage streams a docker image from the host into the jail's rootless
// podman by piping `docker save <imageRef>` into `orb <LoadImageArgs...>`.
// Uses os/exec directly because the payload can be multi-GB — the exec.Runner
// abstraction buffers stdout in memory, which is unsuitable here.
func LoadImage(ctx context.Context, machine, user, uid, imageRef string) error {
	save := exec.CommandContext(ctx, "docker", "save", imageRef)
	load := exec.CommandContext(ctx, "orb", LoadImageArgs(machine, user, uid)...)

	pipe, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("loadimage: stdout pipe: %w", err)
	}
	load.Stdin = pipe

	if err := save.Start(); err != nil {
		return fmt.Errorf("loadimage: docker save start: %w", err)
	}
	if err := load.Start(); err != nil {
		_ = save.Process.Kill()
		return fmt.Errorf("loadimage: orb podman load start: %w", err)
	}

	// Wait for both; collect errors from both sides.
	saveErr := save.Wait()
	loadErr := load.Wait()

	if saveErr != nil {
		return fmt.Errorf("loadimage: docker save: %w", saveErr)
	}
	if loadErr != nil {
		return fmt.Errorf("loadimage: podman load: %w", loadErr)
	}
	return nil
}
