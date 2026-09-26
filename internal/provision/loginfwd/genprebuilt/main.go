// Command genprebuilt cross-compiles lever-login-forward for every guest
// architecture lever supports and writes the results, gzip-compressed, into
// internal/provision/loginfwd/prebuilt/, where `//go:embed` picks them up for
// the NEXT build of lever. See the loginfwd package doc for why the forwarder
// ships embedded.
//
// Run from the repository root, before building lever:
//
//	go run ./internal/provision/loginfwd/genprebuilt
//
// `make install`, `make loginfwd-prebuilt` and the release workflow's
// goreleaser before-hook do exactly this. The build is offline (GOPROXY=off,
// stdlib-only program) and uses the same recipe as the apply-time fallback
// (loginfwd.BuildTo).
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/provision/loginfwd"
)

func main() {
	out := flag.String("out", filepath.Join("internal", "provision", "loginfwd", loginfwd.PrebuiltDir), "directory to write the prebuilt forwarders into")
	flag.Parse()
	if err := generate(context.Background(), proc.RealRunner{}, *out); err != nil {
		fmt.Fprintf(os.Stderr, "genprebuilt: %v\n", err)
		os.Exit(1)
	}
}

func generate(ctx context.Context, r proc.Runner, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	// The digest goes first on the way out and last on the way in: while it
	// is absent, loginfwd ignores whatever else the directory holds, so a
	// generation that stops half way can never be embedded as current.
	digestPath := filepath.Join(out, loginfwd.DigestFile)
	if err := os.Remove(digestPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp("", "lever-loginfwd-gen-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, arch := range loginfwd.GuestArches {
		bin := filepath.Join(stage, "lever-login-forward-"+arch)
		if err := loginfwd.BuildTo(ctx, r, arch, stage, bin); err != nil {
			return fmt.Errorf("linux/%s: %w", arch, err)
		}
		raw, err := os.ReadFile(bin)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return err
		}
		if _, err := zw.Write(raw); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		dst := filepath.Join(out, loginfwd.PrebuiltName(arch))
		if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("genprebuilt: %s (%d bytes, %d gzipped)\n", dst, len(raw), buf.Len())
	}
	return os.WriteFile(digestPath, []byte(loginfwd.SourceDigest()+"\n"), 0o644)
}
