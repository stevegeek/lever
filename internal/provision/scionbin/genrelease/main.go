// Command genrelease builds the prebuilt scion binaries a lever release
// ships (scionbin.GenerateRelease): the gzip-compressed assets for the
// release, and the manifest pinning their digests, which the lever built
// right after embeds.
//
// The release build's goreleaser before-hook runs it from the repository
// root:
//
//	go run ./internal/provision/scionbin/genrelease -version 1.2.3
//
// A lever built without it embeds no manifest and builds scion from source.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
)

func main() {
	version := flag.String("version", "", "the lever release being built, without the v (required)")
	pin := flag.String("pin", scionbin.ReleasePin, "the scion version to build")
	assets := flag.String("assets", filepath.Join("build", "scion-release"), "directory to write the release assets into")
	manifest := flag.String("manifest", filepath.Join("internal", "provision", "scionbin", scionbin.ReleaseDir), "directory to write the embedded manifest into")
	flag.Parse()
	if *version == "" {
		fmt.Fprintln(os.Stderr, "genrelease: -version is required")
		os.Exit(2)
	}
	err := scionbin.GenerateRelease(context.Background(), proc.RealRunner{}, scionbin.GenerateOptions{
		LeverVersion: *version,
		Pin:          *pin,
		AssetDir:     *assets,
		ManifestDir:  *manifest,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "genrelease: %v\n", err)
		os.Exit(1)
	}
}
