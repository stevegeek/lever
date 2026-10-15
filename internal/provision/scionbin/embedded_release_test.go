package scionbin_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stevegeek/lever/internal/provision/scionbin"
)

// The release build runs this with LEVER_REQUIRE_SCION_RELEASE set to the
// version being released, after genrelease and before building lever: it
// fails the release when the manifest lever is about to embed is missing,
// names another release or pin, or does not match the assets the release
// uploads.
func TestReleaseManifestEmbedded(t *testing.T) {
	want := os.Getenv("LEVER_REQUIRE_SCION_RELEASE")
	if want == "" {
		t.Skip("LEVER_REQUIRE_SCION_RELEASE not set (only the release build requires the manifest)")
	}
	rel, err := scionbin.EmbeddedRelease()
	if err != nil || rel == nil {
		t.Fatalf("no valid release manifest embedded from %s: %v", scionbin.ReleaseDir, err)
	}
	if rel.LeverVersion != want || rel.ScionVersion != scionbin.ReleasePin {
		t.Fatalf("manifest is for lever %s, scion %s; want lever %s, scion %s",
			rel.LeverVersion, rel.ScionVersion, want, scionbin.ReleasePin)
	}
	assets := filepath.Join("..", "..", "..", "build", "scion-release")
	for _, arch := range scionbin.ReleaseArches {
		a, ok := rel.Assets[arch]
		if !ok {
			t.Fatalf("manifest has no asset for linux/%s", arch)
		}
		gz, err := os.ReadFile(filepath.Join(assets, a.Name))
		if err != nil {
			t.Fatalf("asset for linux/%s: %v", arch, err)
		}
		if int64(len(gz)) != a.Size || sum(gz) != a.SHA256 {
			t.Fatalf("asset %s does not match the manifest", a.Name)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatalf("asset %s: %v", a.Name, err)
		}
		bin, err := io.ReadAll(zr)
		if err != nil || int64(len(bin)) != a.BinarySize || sum(bin) != a.BinarySHA256 {
			t.Fatalf("asset %s does not unpack to the manifest's binary (err %v)", a.Name, err)
		}
	}
	// The release uploads every asset in the directory: exactly these.
	all, err := filepath.Glob(filepath.Join(assets, "scion_*_linux_*.gz"))
	if err != nil || len(all) != len(rel.Assets) {
		t.Fatalf("asset directory holds %v, want only the manifest's %d assets", all, len(rel.Assets))
	}
	// Remote access on a host without Go or node needs the web UI too.
	// genrelease unpacked it with lever's rules before writing the manifest.
	if rel.Web == nil || rel.Web.Name != scionbin.WebAssetName(scionbin.ReleasePin) {
		t.Fatalf("manifest web asset = %+v, want %s", rel.Web, scionbin.WebAssetName(scionbin.ReleasePin))
	}
	web, err := os.ReadFile(filepath.Join(assets, rel.Web.Name))
	if err != nil || int64(len(web)) != rel.Web.Size || sum(web) != rel.Web.SHA256 {
		t.Fatalf("web asset %s does not match the manifest (err %v)", rel.Web.Name, err)
	}
	all, err = filepath.Glob(filepath.Join(assets, scionbin.WebAssetName("*")))
	if err != nil || len(all) != 1 {
		t.Fatalf("asset directory holds web assets %v, want only %s", all, rel.Web.Name)
	}
}

// The examples pin the scion the release ships prebuilt, so a new instance
// copied from one downloads it instead of building it.
func TestExamplesPinTheReleaseScion(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "examples", "*", "lever.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no example configs found: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s+version:\s*(\S+)`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllSubmatch(b, -1) {
			if string(m[1]) != scionbin.ReleasePin {
				t.Errorf("%s pins scion %s, want %s (scionbin.ReleasePin)", f, m[1], scionbin.ReleasePin)
			}
		}
	}
}
