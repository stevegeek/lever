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

// Normalize spells ref the way docker and podman resolve it, so two spellings
// of one image compare equal: the default registry docker.io (index.docker.io
// too), library/ for a single-name Docker Hub image, the implied :latest, and
// a lowercase registry host. A first component is a registry host when it
// holds "." or ":" or is localhost, as in docker's reference grammar.
func Normalize(ref string) string {
	if ref == "" {
		return ""
	}
	name, digest := ref, ""
	if i := strings.Index(ref, "@"); i >= 0 {
		name, digest = ref[:i], ref[i:]
	}
	domain, rest := "docker.io", name
	if i := strings.Index(name, "/"); i > 0 && (strings.ContainsAny(name[:i], ".:") || strings.EqualFold(name[:i], "localhost")) {
		domain, rest = strings.ToLower(name[:i]), name[i+1:]
	}
	if domain == "index.docker.io" {
		domain = "docker.io"
	}
	if domain == "docker.io" && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	if digest == "" && !strings.Contains(rest[strings.LastIndex(rest, "/")+1:], ":") {
		rest += ":latest"
	}
	return domain + "/" + rest + digest
}

// Names returns, normalised, every name the jail's podman holds the configured
// image ref under once it is loaded: the name the load gives it, the
// localhost/ alias for an unqualified ref, and the name agents run it as.
func Names(ref string) []string {
	if ref == "" {
		return nil
	}
	names := []string{Normalize(ref), Normalize(Agent(ref))}
	if !strings.Contains(ref, "@") && !Qualified(ref) && !strings.HasPrefix(ref, "localhost/") {
		names = append(names, Normalize("localhost/"+ref))
	}
	return names
}
