package guest

import "strings"

// PastaHostAddr is the address at which an agent container reaches the
// guest's loopback (where the hub listens) through pasta. podman resolves
// host.containers.internal to it, so scion's auto-computed container hub
// endpoint (host.containers.internal:PORT) reaches the VM-loopback hub.
const PastaHostAddr = "169.254.1.2"

// pastaDropInPath is a drop-in so lever never clobbers a base
// containers.conf.
const pastaDropInPath = "$HOME/.config/containers/containers.conf.d/10-lever-pasta.conf"

// pastaDropInModern is for a pasta with --map-host-loopback (passt 2024-08
// and later), which maps PastaHostAddr to the guest's loopback directly.
// host_containers_internal_ip is needed for podman before 5.3, where the name
// would otherwise resolve to the guest's own address (which LEVER_EGRESS
// drops). The key exists since podman 4.7.
const pastaDropInModern = `[containers]
host_containers_internal_ip = "` + PastaHostAddr + `"

[network]
default_rootless_network_cmd = "pasta"
pasta_options = ["--map-host-loopback", "` + PastaHostAddr + `"]
`

// pastaDropInGateway is for an older pasta (Ubuntu 24.04's passt 2024-02-20,
// under podman 4.9) with no --map-host-loopback, where an unknown option
// makes every container fail to start. It maps the gateway to the host's
// loopback instead (--map-gw; podman 4.9 passes --no-map-gw unless told
// otherwise), with PastaHostAddr as the gateway. -4 keeps pasta IPv4-only:
// otherwise --map-gw also maps the IPv6 gateway to the guest's ::1, a wider
// loopback exposure than --map-host-loopback.
const pastaDropInGateway = `[containers]
host_containers_internal_ip = "` + PastaHostAddr + `"

[network]
default_rootless_network_cmd = "pasta"
pasta_options = ["-4", "--map-gw", "--address", "169.254.1.1", "--netmask", "16", "--gateway", "` + PastaHostAddr + `"]
`

// pastaDropInScript writes the podman drop-in that matches the guest's pasta,
// deciding in the guest from `pasta --help`. Agents run in their own pasta
// netns, which keeps each container's 127.0.0.1 private. Both drop-ins set
// default_rootless_network_cmd because podman 4.9 defaults to slirp4netns and
// then ignores pasta_options. The script fails (exit 3) rather than leave
// agents on slirp4netns or write options the installed pasta rejects.
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
