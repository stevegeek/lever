// Package imageref names an agent image the way the jail's podman holds it and
// scion is told to run it. It is a stdlib-only leaf so config validation, the
// load step and the agent starts all derive the same name.
package imageref

import "strings"

// Registry is the registry host of every agent image name lever hands scion.
// No registry by this name exists; the image is always loaded into the jail
// before an agent starts on it.
//
// The name must keep a "." in its first component. scion rewrites a reference
// whose first component holds neither "." nor ":" to the hub's image_registry
// at every agent start (config.RewriteImageRegistry, on the hub's dispatch and
// in the broker), and any loopback caller, an agent included, can set that
// registry live. A qualified name is never rewritten.
const Registry = "lever.local"

// Qualified reports whether ref names its registry host, which is what keeps
// scion from rewriting it.
func Qualified(ref string) bool {
	i := strings.Index(ref, "/")
	return i > 0 && strings.ContainsAny(ref[:i], ".:")
}

// Agent returns the name an agent runs the configured image ref under. A
// qualified or digest-pinned ref is returned unchanged: podman cannot tag a
// digest, and scion's rewrite of a digest-pinned ref can only fail the pull.
func Agent(ref string) string {
	if ref == "" || strings.Contains(ref, "@") || Qualified(ref) {
		return ref
	}
	return Registry + "/" + ref
}
