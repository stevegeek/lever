package agent

import (
	"context"
	"fmt"
	"net/http"

	"github.com/stevegeek/lever/internal/buildstamp"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/jailabi"
	"github.com/stevegeek/lever/internal/wire"
)

// Version is the lever-agent build version advertised in the capability MCP
// server's serverInfo, stamped by the Makefile with -ldflags -X. It is not
// cli.Version because the jail binary must not link internal/cli.
var Version = "dev"

// BuildLine is what `<binary> --version` prints: the binary's release, the
// commit it was built from and the Jail ABI it was built with.
func BuildLine(binary, release string) string {
	return fmt.Sprintf("%s %s, jail ABI %d", binary, buildstamp.Describe(release), jailabi.JailABI)
}

// HostVersion asks the broker for the host lever's version and Jail ABI.
func HostVersion(ctx context.Context, brokerURL string, client *http.Client) (wire.VersionResponse, error) {
	var out wire.VersionResponse
	if err := httpjson.Get(ctx, client, brokerURL+wire.PathVersion, &out); err != nil {
		return wire.VersionResponse{}, fmt.Errorf("agent: host version: %w", err)
	}
	return out, nil
}

// VersionReport is what `<binary> version` prints after BuildLine: the
// host's build and a verdict on the pair. hostErr is the GET /version error; a
// 404 is a host lever that predates the route, so one older than this image.
// Any other error is returned. worker selects advice a worker can act on: the
// fix is a host command only the operator runs.
//
// The Jail ABI decides, as in `lever doctor`: equal ABIs share the in-jail
// contract, so a release difference needs no rebuild.
func VersionReport(release string, host wire.VersionResponse, hostErr error, worker bool) (string, error) {
	fix := "rebuild the agent image, then lever up --fresh --keep-conversation"
	if worker {
		fix = "ask the operator to " + fix
	}
	local := buildstamp.Describe(release)
	image := fmt.Sprintf("image %s (ABI %d)", local, jailabi.JailABI)
	if hostErr != nil {
		if httpjson.Status(hostErr) != http.StatusNotFound {
			return "", hostErr
		}
		return fmt.Sprintf("host lever: older than this image (no GET %s)\nmismatch: %s, host older: %s", wire.PathVersion, image, fix), nil
	}
	hostVersion := host.Version
	if hostVersion == "" {
		hostVersion = "unknown"
	}
	line := fmt.Sprintf("host lever %s, jail ABI %d\n", hostVersion, host.JailABI)
	switch {
	case host.JailABI != jailabi.JailABI:
		return line + fmt.Sprintf("mismatch: %s, host %s (ABI %d): %s", image, hostVersion, host.JailABI, fix), nil
	case host.Version == local:
		return line + "match: the image and the host are the same build", nil
	default:
		return line + "compatible: the same jail ABI, so no rebuild is needed (the builds differ)", nil
	}
}
