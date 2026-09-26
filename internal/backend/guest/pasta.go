package guest

import "strings"

// PastaHostAddr is the address at which an agent container reaches the
// guest's loopback (where the hub listens) through pasta. podman resolves
// host.containers.internal to it, so scion's auto-computed container hub
// endpoint (host.containers.internal:PORT) reaches the VM-loopback hub.
const PastaHostAddr = "169.254.1.2"

// pastaDropInPath is lever's podman drop-in. A containers.conf.d drop-in so
// lever never clobbers a base containers.conf.
const pastaDropInPath = "$HOME/.config/containers/containers.conf.d/10-lever-pasta.conf"

// pastaDropInModern is for a pasta with --map-host-loopback (passt 2024-08
// and later; the OrbStack guest's podman 5), which maps PastaHostAddr to the
// guest's loopback directly. podman 5.3+ already resolves
// host.containers.internal to it; host_containers_internal_ip says so
// explicitly for podman 5.0-5.2, or podman 4.9 with a newer passt, where the
// name would otherwise resolve to the guest's own address (which
// LEVER_EGRESS drops). The key exists since podman 4.7.
const pastaDropInModern = `[containers]
host_containers_internal_ip = "` + PastaHostAddr + `"

[network]
default_rootless_network_cmd = "pasta"
pasta_options = ["--map-host-loopback", "` + PastaHostAddr + `"]
`

// pastaDropInGateway is for an older pasta (Ubuntu 24.04's passt 2024-02-20,
// under podman 4.9), which has no --map-host-loopback: an unknown option
// makes every container fail to start. It maps its gateway address to the
// host's loopback instead (--map-gw; podman 4.9 passes --no-map-gw unless
// told otherwise), so the container gets a link-local address with
// PastaHostAddr as its gateway, and podman is told to resolve
// host.containers.internal to it. Verified on a Lima Ubuntu 24.04 guest: the
// hub is reachable by name; without --map-gw or without
// host_containers_internal_ip it is unreachable. -4 keeps pasta IPv4-only:
// otherwise it copies the guest's IPv6 address and gateway into the
// container, and --map-gw maps that IPv6 gateway to the guest's ::1 — a
// wider loopback exposure than --map-host-loopback, which is IPv4-only.
const pastaDropInGateway = `[containers]
host_containers_internal_ip = "` + PastaHostAddr + `"

[network]
default_rootless_network_cmd = "pasta"
pasta_options = ["-4", "--map-gw", "--address", "169.254.1.1", "--netmask", "16", "--gateway", "` + PastaHostAddr + `"]
`

// pastaDropInScript writes the podman drop-in that matches the guest's pasta
// (lever#35), deciding in the guest from `pasta --help`. Agents run in their
// own pasta netns (no --network=host; see jail.jailEnvFor), which keeps each
// container's 127.0.0.1 private. Both drop-ins set
// default_rootless_network_cmd, because podman 4.9 defaults rootless
// containers to slirp4netns and then ignores pasta_options; there
// host.containers.internal resolves to the guest's own address, which
// LEVER_EGRESS drops, so no agent reaches the hub. The script fails (exit 3)
// rather than leave agents on slirp4netns or write options the installed
// pasta rejects. Rewritten on every apply, so an upgrade replaces an older
// drop-in.
func pastaDropInScript() string {
	var b strings.Builder
	b.WriteString(`set -e
if ! command -v pasta >/dev/null 2>&1; then
  echo "lever: the guest has no pasta (package passt); agents need it to reach the hub" >&2
  exit 3
fi
help=$(pasta --help 2>&1 || true)
mkdir -p "$HOME/.config/containers/containers.conf.d"
case "$help" in
*--map-host-loopback*)
  cat > "` + pastaDropInPath + `" <<'LEVER_EOF'
`)
	b.WriteString(pastaDropInModern)
	b.WriteString(`LEVER_EOF
  ;;
*--no-map-gw*)
  cat > "` + pastaDropInPath + `" <<'LEVER_EOF'
`)
	b.WriteString(pastaDropInGateway)
	b.WriteString(`LEVER_EOF
  ;;
*)
  echo "lever: the guest's pasta supports neither --map-host-loopback nor gateway mapping; agents could not reach the hub" >&2
  exit 3
  ;;
esac
`)
	return b.String()
}
