package jail

import (
	"context"
	"fmt"
	"os/exec"
)

// LoadImageArgs returns the full host argv (including the prefix binary) that
// loads a docker-archive (read on stdin) into the jail's rootless podman,
// e.g. for the OrbStack prefix:
//
//	orb -m <machine> -u <user> env XDG_RUNTIME_DIR=/run/user/<uid> podman load
//
// Fallback (if pipe proves unreliable): mount-staging — docker save -o
// <tree>/.img.tar on the host, then in-jail: podman load -i <tree>/.img.tar.
// Implement the pipe first.
func LoadImageArgs(prefix []string, uid string) []string {
	argv := append([]string{}, prefix...)
	return append(argv,
		"env",
		"XDG_RUNTIME_DIR=/run/user/"+uid,
		"podman", "load",
	)
}

// LoadImage streams a docker image from the host into the jail's rootless
// podman by piping `docker save <imageRef>` into the backend's LoadImageArgs
// argv. Uses os/exec directly because the payload can be multi-GB — the
// exec.Runner abstraction buffers stdout in memory, which is unsuitable here.
func LoadImage(ctx context.Context, prefix []string, uid, imageRef string) error {
	save := exec.CommandContext(ctx, "docker", "save", imageRef)
	args := LoadImageArgs(prefix, uid)
	load := exec.CommandContext(ctx, args[0], args[1:]...)

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
		return fmt.Errorf("loadimage: jail podman load start: %w", err)
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
