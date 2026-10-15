package scionbin

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// WebAsset is the release asset holding scion's built web UI: a gzip of a
// tar of its dist/client directory (internal/provision/webassets packs and
// unpacks it). The UI is the same on every architecture, so a release ships
// one.
type WebAsset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// MaxWebArchiveBytes bounds the web asset's compressed size, in the manifest
// and so in the download.
const MaxWebArchiveBytes = 256 << 20

// WebAssetName is the release asset name of the web UI built from pin.
func WebAssetName(pin string) string { return "scion-web_" + pin + ".tar.gz" }

var errNoWebRelease = fmt.Errorf("%w: no prebuilt scion web UI for this version", errReleaseUnavailable)

// ReleaseWebUI returns this release's web UI asset when lever downloads it
// for scion.version instead of building it: the manifest carries one, the
// version is the release pin, and downloads are not turned off. An error for
// which ReleaseUnavailable is true means there is nothing to download; any
// other is a broken embedded manifest.
func ReleaseWebUI(version string) (WebAsset, error) {
	rel, err := currentRelease()
	if err != nil {
		return WebAsset{}, err
	}
	if rel == nil || rel.Web == nil || downloadsDisabled() || !rel.Matches(version) {
		return WebAsset{}, errNoWebRelease
	}
	return *rel.Web, nil
}

// DownloadsWebUI reports whether lever downloads the prebuilt web UI for
// version. `lever doctor` asks it to know whether node and Go are needed.
func DownloadsWebUI(version string) bool {
	_, err := ReleaseWebUI(version)
	return err == nil
}

// FetchWebUI writes this release's web UI asset for version into w and
// checks it against the size and sha256 embedded in lever. The caller must not use what w received when it returns an error.
//
// An error for which ReleaseUnavailable is true means: build the UI from
// source instead. ErrReleaseTampered and any other error stop the apply.
func FetchWebUI(ctx context.Context, version string, w io.Writer) error {
	a, err := ReleaseWebUI(version)
	if err != nil {
		return err
	}
	rel, err := currentRelease()
	if err != nil {
		return err
	}
	err = download(ctx, site, rel, Asset{Name: a.Name, SHA256: a.SHA256, Size: a.Size}, w)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ReleaseUnavailable reports whether err from FetchWebUI means the download
// did not happen (no network, no such asset, a broken transfer), as opposed
// to a refusal.
func ReleaseUnavailable(err error) bool { return errors.Is(err, errReleaseUnavailable) }
