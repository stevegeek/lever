// Command genrelease builds the prebuilt scion a lever release ships
// (scionbin.GenerateRelease): the gzip-compressed binaries and the web UI
// archive for the release, and the manifest pinning their digests, which the
// lever built right after embeds.
//
// The release workflow runs it twice, from the repository root. A job with
// no write token and no release credentials builds the web UI, whose npm
// install runs third-party scripts (needs node and npm):
//
//	go run ./internal/provision/scionbin/genrelease -web-only -assets build/web-ui
//
// and the release job's goreleaser before-hook builds the binaries and pins
// that archive without running npm:
//
//	go run ./internal/provision/scionbin/genrelease -version 1.2.3 -web-archive build/web-ui
//
// Without -web-archive it builds the web UI itself. A lever built without
// genrelease embeds no manifest and builds scion from source.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/scionbin"
	"github.com/stevegeek/lever/internal/provision/webassets"
)

func main() {
	version := flag.String("version", "", "the lever release being built, without the v (required unless -web-only)")
	pin := flag.String("pin", scionbin.ReleasePin, "the scion version to build")
	assets := flag.String("assets", filepath.Join("build", "scion-release"), "directory to write the release assets into")
	manifest := flag.String("manifest", filepath.Join("internal", "provision", "scionbin", scionbin.ReleaseDir), "directory to write the embedded manifest into")
	webOnly := flag.Bool("web-only", false, "build only the web UI archive into -assets; no binaries, no manifest")
	webArchive := flag.String("web-archive", "", "directory holding the web UI archive a -web-only run wrote, to pin instead of building it")
	flag.Parse()
	ctx := context.Background()
	r := proc.RealRunner{}
	if *webOnly {
		_, dir, err := scionbin.FetchModule(ctx, r, *pin)
		if err == nil {
			_, err = webassets.GenerateRelease(ctx, r, dir, *assets, *pin)
		}
		exit(err)
		return
	}
	if *version == "" {
		fmt.Fprintln(os.Stderr, "genrelease: -version is required")
		os.Exit(2)
	}
	web := func(ctx context.Context, moduleDir, assetDir string) (*scionbin.WebAsset, error) {
		if *webArchive != "" {
			return webassets.ImportRelease(*webArchive, assetDir, *pin)
		}
		return webassets.GenerateRelease(ctx, r, moduleDir, assetDir, *pin)
	}
	exit(scionbin.GenerateRelease(ctx, r, scionbin.GenerateOptions{
		LeverVersion: *version,
		Pin:          *pin,
		AssetDir:     *assets,
		ManifestDir:  *manifest,
		WebUI:        web,
	}))
}

func exit(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "genrelease: %v\n", err)
		os.Exit(1)
	}
}
