package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/wire"
)

// validate checks one broker.tools entry's shape, failing closed. External
// tools are fronted, not spawned: backend required, command forbidden,
// backend a literal loopback IP unless explicitly opted out. gate applies to
// external tools only; coarse replaces the operations list with the wildcard.
// Whether a supervised command is actually spawnable is a host probe
// (Tool.checkHost).
func (t Tool) validate() error {
	if !t.Gate.valid() {
		return fmt.Errorf("config: broker tool %q gate %q invalid (want fine|coarse)", t.Name, t.Gate)
	}
	for _, o := range t.Operations {
		if o.Name == "" {
			return fmt.Errorf("config: broker tool %q has an operation with an empty name", t.Name)
		}
		if o.Name == wire.WildcardOp {
			return fmt.Errorf("config: broker tool %q declares operation %q, which is reserved for gate: coarse", t.Name, wire.WildcardOp)
		}
	}
	if !t.External {
		if t.Gate != "" {
			return fmt.Errorf("config: broker tool %q sets gate but is not external (gate applies to external tools only)", t.Name)
		}
		if t.AllowNonLoopback {
			return fmt.Errorf("config: broker tool %q sets allow_non_loopback but is not external (applies to external tools only)", t.Name)
		}
		if len(t.Command) == 0 {
			return fmt.Errorf("config: broker tool %q has no command (a supervised tool needs one; did you mean external: true?)", t.Name)
		}
		return nil
	}
	if len(t.Command) != 0 {
		return fmt.Errorf("config: external broker tool %q must not set command (the broker fronts external servers, it does not spawn them)", t.Name)
	}
	if t.Backend == "" {
		return fmt.Errorf("config: external broker tool %q needs backend (the server's own listen address, host:port[/path])", t.Name)
	}
	if err := t.validateExternalBackend(); err != nil {
		return err
	}
	if t.EffectiveGate() == GateCoarse {
		if len(t.Operations) != 0 {
			return fmt.Errorf("config: external broker tool %q is gate: coarse and must not declare operations (coarse admits the whole surface; use gate: fine to enumerate)", t.Name)
		}
		if len(t.AllowedValues) != 0 {
			return fmt.Errorf("config: external broker tool %q is gate: coarse and must not declare allowed_values (there are no per-operation params to pin)", t.Name)
		}
		return nil
	}
	if len(t.Operations) == 0 {
		return fmt.Errorf("config: external broker tool %q is gate: fine and needs operations (or set gate: coarse to admit the whole surface)", t.Name)
	}
	return nil
}

// validateExternalBackend confines an external backend to a literal loopback
// IP (127.0.0.1 / [::1]) unless allow_non_loopback is set. The broker proxies
// host-side, so a non-loopback backend would let the jailed agent reach other
// hosts through the broker, circumventing the jail's LAN-drop egress.
// Hostnames (even "localhost") are rejected: only a literal IP can be
// loopback-checked without trusting a resolver.
func (t Tool) validateExternalBackend() error {
	b := t.Backend
	if strings.Contains(b, "://") {
		return fmt.Errorf("config: external broker tool %q backend %q must not carry a scheme (want host:port[/path])", t.Name, b)
	}
	hostport := b
	if i := strings.IndexByte(b, '/'); i >= 0 {
		hostport = b[:i]
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return fmt.Errorf("config: external broker tool %q backend %q: %v (want host:port[/path])", t.Name, b, err)
	}
	if t.AllowNonLoopback {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("config: external broker tool %q backend %q is not a literal loopback IP — the broker proxies host-side, so a non-loopback backend would let the jailed agent reach other hosts through the broker; set allow_non_loopback: true only if that is exactly what you intend", t.Name, b)
	}
	return nil
}

// validateBackend rejects a config's backend unless lever can run it
// (KnownBackends, held in lockstep with backend.Candidates), so nothing is
// ever silently swapped for the default.
func validateBackend(name string) error {
	if name == "" {
		return fmt.Errorf("config: backend is required (valid: %s)", strings.Join(backendNames(), ", "))
	}
	if !knownBackend(name) {
		return fmt.Errorf("config: unknown backend %q (valid: %s)", name, strings.Join(backendNames(), ", "))
	}
	return nil
}

// nameRE constrains an instance/worker name: it becomes a jail machine name
// (`lever-<name>`) and a shell token in the scion-install path.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var diskRE = regexp.MustCompile(`^[0-9]+(MiB|GiB|MB|GB)$`)

// validateDisk checks the optional Lima disk size. Empty is valid (default).
func validateDisk(d string) error {
	if d == "" {
		return nil
	}
	if !diskRE.MatchString(d) {
		return fmt.Errorf("config: disk %q invalid (want e.g. 24GiB, 40GiB)", d)
	}
	return nil
}

// toolNameRE constrains a broker tool name: it becomes the /mcp/<name>/ tool
// route and a `claude mcp add <name>` token. Underscores are admitted (real MCP
// server names use them, e.g. `apple_notes`), unlike a jail machine name; but
// whitespace, `/`, `*`, and uppercase are still excluded so the name is safe in
// a URL path and can never collide with the reserved wildcard op.
var toolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// imageRE constrains a container image reference to safe OCI-ref characters
// (no whitespace or shell metacharacters).
var imageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]*$`)

// digestRE matches an image pinned by content digest (e.g. `…@sha256:<hex>`).
var digestRE = regexp.MustCompile(`@[a-z0-9]+:[0-9a-fA-F]{32,}$`)

// validateImage checks an image ref against the charset, the optional registry
// allowlist, and the optional digest-pin requirement. field names the source
// for error messages (e.g. "manager.image").
func (s Security) validateImage(field, ref string) error {
	if !imageRE.MatchString(ref) {
		return fmt.Errorf("config: %s %q has invalid characters", field, ref)
	}
	if len(s.AllowedImageRegistries) > 0 && !registryAllowed(ref, s.AllowedImageRegistries) {
		return fmt.Errorf("config: %s %q is not from an allowed registry (allowed: %s)", field, ref, strings.Join(s.AllowedImageRegistries, ", "))
	}
	if s.RequireImageDigest && !digestRE.MatchString(ref) {
		return fmt.Errorf("config: %s %q must be pinned by digest (…@sha256:<hex>); a mutable tag is not allowed", field, ref)
	}
	return nil
}

// registryAllowed reports whether ref starts with one of the allowed prefixes,
// matched on whole path components (so "scionlocal" allows "scionlocal/x" but
// not "scionlocalevil/x").
func registryAllowed(ref string, prefixes []string) bool {
	for _, p := range prefixes {
		if ref == p || strings.HasPrefix(ref, p+"/") {
			return true
		}
	}
	return false
}

// Validate checks the config's shape. It is pure: nothing on the host is
// read or probed — that is CheckHost, which Load runs right after.
func (a *App) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("config: name is required")
	}
	if !nameRE.MatchString(a.Name) {
		return fmt.Errorf("config: name %q must match %s (it becomes the jail machine name and a shell token)", a.Name, nameRE)
	}
	if err := validateBackend(a.Backend); err != nil {
		return err
	}
	if !a.Broker.AutoReenrol.valid() {
		return fmt.Errorf("config: broker.auto_reenrol %q must be one of all|manager|off (or unset = all)", a.Broker.AutoReenrol)
	}
	if err := validateDisk(a.Disk); err != nil {
		return err
	}
	if a.Tree == "" {
		return fmt.Errorf("config: tree is required")
	}
	if a.Manager.Image != "" {
		if err := a.Security.validateImage("manager.image", a.Manager.Image); err != nil {
			return err
		}
	}
	// manager.allow_ports opens a host-loopback port to the jailed agent (via
	// the egress allowlist's per-port ACCEPT on the host alias); the broker's
	// admin port (/bootstrap, /revoke, /bump-epoch — unauthenticated, meant to
	// be reachable only from the host loopback) must never be listed there,
	// or the allowlist — the ONLY thing isolating that API from the guest —
	// would hand the jail a direct path to it.
	adminPort := a.EffectiveAdminPort()
	for _, p := range a.Manager.AllowPorts {
		if p == adminPort {
			return fmt.Errorf("config: manager.allow_ports must not include the broker admin port (%d) — this would hand the jailed agent a direct, unauthenticated path to /bootstrap, /revoke, /bump-epoch (the egress allowlist is the only thing isolating the host-loopback admin API from the guest)", adminPort)
		}
	}
	// prompt_file is host-only (read at the root, NOT in the mount) and must stay
	// inside the instance root.
	if a.Manager.PromptFile != "" && !confinedRel(a.Manager.PromptFile) {
		return fmt.Errorf("config: manager.prompt_file %q must be a relative path inside the instance root (no \"..\", not absolute)", a.Manager.PromptFile)
	}
	for _, g := range a.Workers {
		if err := a.validateWorker(g); err != nil {
			return err
		}
	}
	if err := a.validateWorkerDirsDisjoint(); err != nil {
		return err
	}
	if err := a.validateBroker(); err != nil {
		return err
	}
	if err := a.validateOperator(); err != nil {
		return err
	}
	if err := a.validateRemote(); err != nil {
		return err
	}
	return nil
}

// managerAlias is the literal name the broker's operator-directive channel
// resolves to the manager, independent of broker.manager_identity (see
// broker.resolveDirectiveAgent). Reserved for that reason.
const managerAlias = "manager"

// validateWorkerDirsDisjoint rejects two workers whose dirs overlap — the same
// subtree, or one an ancestor of the other.
//
// Sibling isolation rests on each worker mounting only its own subtree: a
// sibling's directory is "simply not a mount source". An overlap voids that for
// the pair. The outer worker reads and writes the inner one's whole workspace,
// which includes the fresh, UNSPENT enrolment ticket the broker stages at
// <dir>/.lever/bootstrap.json on every resume — redeem it first and the outer
// worker enrols as the inner worker's CN, taking its capability grants.
func (a *App) validateWorkerDirsDisjoint() error {
	for i, outer := range a.Workers {
		for j, inner := range a.Workers {
			if i >= j {
				continue
			}
			if pathOverlaps(outer.Dir, inner.Dir) {
				return fmt.Errorf("config: workers %q (dir %q) and %q (dir %q) overlap — one is the other, or contains it; "+
					"each worker must mount a subtree no other worker can reach, or it can read its sibling's workspace and steal the enrolment ticket staged there",
					outer.Name, outer.Dir, inner.Name, inner.Dir)
			}
		}
	}
	return nil
}

// pathOverlaps reports whether two relative paths name the same directory or one
// contains the other. Comparison is component-wise on cleaned paths, so
// "workers/x" does not "contain" the unrelated "workers/xy".
func pathOverlaps(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(ca, cb+sep) || strings.HasPrefix(cb, ca+sep)
}

// validateWorker validates a single worker declaration: name shape, collisions
// with the manager's identities, dir confinement, and image policy.
func (a *App) validateWorker(g Worker) error {
	if g.Name == "" || g.Dir == "" {
		return fmt.Errorf("config: worker needs name + dir (got %+v)", g)
	}
	if !nameRE.MatchString(g.Name) {
		return fmt.Errorf("config: worker name %q must match %s", g.Name, nameRE)
	}
	if g.Name == a.ManagerCN() {
		return fmt.Errorf("config: worker name %q collides with the manager identity — a worker must not share the manager's CN", g.Name)
	}
	if g.Name == managerAlias {
		// The directive channel resolves the literal "manager" to the manager
		// whatever broker.manager_identity says (broker.resolveDirectiveAgent),
		// so a worker holding that name is silently shadowed: an operator
		// directive aimed at the worker is delivered to the most privileged
		// agent instead. The ManagerCN check above does not cover it, because a
		// custom manager_identity moves the CN off this word.
		return fmt.Errorf("config: worker name %q is reserved — it is the alias the operator-directive channel resolves to the manager, so a worker of that name could never be addressed", g.Name)
	}
	if g.Name == a.Name {
		// The manager's scion agent slug IS the app name (apply dispatches
		// it as Worker: app.Name), and the broker routes manager-recipient
		// matches by slug — a worker named like the app would be shadowed
		// (messages to it silently route to the manager).
		return fmt.Errorf("config: worker name %q collides with the manager agent (the app name) — rename the worker or the app", g.Name)
	}
	if filepath.IsAbs(g.Dir) || strings.HasPrefix(filepath.Clean(g.Dir), "..") {
		return fmt.Errorf("config: worker dir %q must be relative and inside the tree", g.Dir)
	}
	if filepath.Clean(g.Dir) == "." {
		// A "." dir makes WorkerDir(g) == a.Tree, so the worker's workspace would
		// be the whole tree — mounting root defeats R4 sibling isolation (the
		// worker could read every sibling's subdir). Workers must occupy a strict
		// subdir of the shared instance project. (confinedRel rejects "." for
		// `tree` for the analogous root-is-the-mount reason.)
		return fmt.Errorf("config: worker %q dir must be a subdir of the tree, not %q (which collides with the manager's mount root)", g.Name, g.Dir)
	}
	if g.Image != "" {
		if err := a.Security.validateImage(fmt.Sprintf("worker %q image", g.Name), g.Image); err != nil {
			return err
		}
	}
	return nil
}

// validateOperator fails closed on operator-directive-channel config
// mistakes: allowed_signers, if set, must be a path confined inside the
// instance root (like manager.prompt_file); the expiry pair must be sane —
// directive_expiry <= directive_expiry_max <= opsig.MaxExpiry, the hard
// ceiling the directive-signing protocol assumes. Checked unconditionally (even with the
// channel disabled) so a nonsensical expiry pair is caught at config-load
// time rather than silently ignored until allowed_signers is later set.
func (a *App) validateOperator() error {
	if a.Operator.AllowedSigners != "" && !confinedRel(a.Operator.AllowedSigners) {
		return fmt.Errorf("config: operator: allowed_signers %q must be a relative path inside the instance root (no \"..\", not absolute)", a.Operator.AllowedSigners)
	}
	if a.Operator.DirectiveExpiry < 0 {
		return fmt.Errorf("config: operator: directive_expiry %s must not be negative", a.Operator.DirectiveExpiry)
	}
	if a.Operator.DirectiveExpiryMax < 0 {
		return fmt.Errorf("config: operator: directive_expiry_max %s must not be negative", a.Operator.DirectiveExpiryMax)
	}
	if a.EffectiveDirectiveExpiryMax() > opsig.MaxExpiry {
		return fmt.Errorf("config: operator: directive_expiry_max %s exceeds the 24h hard ceiling", a.EffectiveDirectiveExpiryMax())
	}
	if a.EffectiveDirectiveExpiry() > a.EffectiveDirectiveExpiryMax() {
		return fmt.Errorf("config: operator: directive_expiry %s exceeds directive_expiry_max %s", a.EffectiveDirectiveExpiry(), a.EffectiveDirectiveExpiryMax())
	}
	return nil
}

// validateRemote rejects a remote block that could not serve safely: an
// unvalidated backend, a port colliding with the broker's listeners, a proxy
// port the jail is allowed to dial, a missing or malformed base_url, or a
// blank allowed_users entry (which would pin to nothing and read as "allow
// none" while acting as "allow this header value" with an empty string).
// Skipped entirely while disabled, so a stale port/base_url left from a
// previous config doesn't block loading until remote is re-enabled.
func (a *App) validateRemote() error {
	if !a.Remote.Enabled {
		return nil
	}
	if a.Backend != BackendOrbstack {
		// NOT a reachability limit. The proxy dials the hub THROUGH the jail
		// (remoteproxy.JailDial), which every backend supports and which needs
		// no guest→host forwarding at all — that was the old rationale, and it
		// no longer describes the transport. The gate stays only because the
		// Lima path has never been live-validated; lifting it is a live-test
		// question, not a code one.
		//
		// Rejecting at load time rather than at serve time closes a trap:
		// without this check the config loads, `apply` returns 0, and the
		// proxy child spawned by `lever remote serve` silently dies into
		// remote.log. newRemoteServeCmd carries the same check at runtime
		// (internal/cli/remote.go) as belt-and-braces defense-in-depth.
		return fmt.Errorf("config: remote: requires the orbstack backend in v1 (the Lima path is not live-validated yet)")
	}
	rp := a.EffectiveRemotePort()
	if rp == a.EffectiveJailPort() || rp == a.EffectiveAdminPort() {
		return fmt.Errorf("config: remote: port %d collides with a broker listener", rp)
	}
	if rp == GuestLoginIssuerPort {
		// Same reason as login_port below: the container runtime mirrors the
		// jail's login forwarder onto this host port, so whichever of lever's
		// two host listeners names it cannot bind. The proxy's failure is loud
		// now (apply waits for it to bind), but the guard belongs on both
		// listeners, not just the one that met the failure first.
		return fmt.Errorf("config: remote: port %d is the port the jail's login forwarder is mirrored onto "+
			"by the container runtime, so the proxy cannot bind it — pick another", rp)
	}
	if slices.Contains(a.Manager.AllowPorts, rp) {
		// Not a bind collision like the checks above — a trust-boundary one.
		// The proxy's gate rests on "only `tailscale serve` reaches this
		// loopback listener", which is what makes it safe for the proxy to
		// believe the Tailscale-User-Login header it is handed (see
		// listenLoopback and the package doc's stated precondition in
		// internal/remoteproxy). A port listed in manager.allow_ports gets an
		// egress ACCEPT for jail→host on exactly that number
		// (EffectiveAllowedPorts → internal/egress), so naming the proxy's port
		// there hands every jailed agent a direct route to the gate: it sets
		// the header itself and receives the injected remote PAT, which
		// carries agent:attach on every agent in the project. That is the
		// cross-agent escalation the per-agent netns closed in v0.7.0,
		// re-opened by one line of config.
		//
		// remote.login_port is deliberately NOT rejected: the guest's login
		// forwarder exists to reach it, EffectiveAllowedPorts grants it on
		// purpose, and what answers there is the OIDC provider, which mints
		// nothing without an in-process call (internal/remoteproxy/oidc.go).
		return fmt.Errorf("config: remote: manager.allow_ports lists %d, which is the remote proxy's own port — "+
			"that grant lets any jailed agent reach the proxy directly and forge the Tailscale-User-Login header "+
			"it trusts, receiving the remote PAT (agent:attach on every agent in the project); remove %d from "+
			"manager.allow_ports, or move the proxy with remote.port", rp, rp)
	}
	lp := a.EffectiveRemoteLoginPort()
	if lp == a.EffectiveJailPort() || lp == a.EffectiveAdminPort() {
		return fmt.Errorf("config: remote: login_port %d collides with a broker listener", lp)
	}
	if lp == rp {
		// Two listeners, deliberately: the proxy answers the operator's
		// browser through `tailscale serve`, the provider answers the hub's
		// back channel from inside the jail. One port cannot be both.
		return fmt.Errorf("config: remote: login_port %d collides with the proxy port", lp)
	}
	if lp == GuestLoginIssuerPort {
		// Not a host listener lever owns, but one it causes: OrbStack mirrors
		// the guest forwarder onto the host at this number, so the provider
		// could not bind here even though nothing in lever's own config says
		// so. This was a live failure, not a theoretical one.
		return fmt.Errorf("config: remote: login_port %d is the port the jail's login forwarder is mirrored onto "+
			"by the container runtime, so the provider cannot bind it — pick another", lp)
	}
	if a.Remote.BaseURL == "" {
		// base_url sets the proxy's ServeHost (remoteServeHost, internal/cli/
		// remote.go); an empty ServeHost fails every request closed — the
		// proxy would start and serve nothing at all. Reject that state at
		// load time rather than let it "succeed" into a proxy that 403s
		// 100% of traffic, which otherwise only surfaced later as a
		// confusing doctor healthz failure pointing at remote.log.
		return fmt.Errorf("config: remote: base_url is required when remote is enabled (without it the proxy refuses all requests)")
	}
	u, err := url.Parse(a.Remote.BaseURL)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("config: remote: base_url %q must be an absolute https URL", a.Remote.BaseURL)
	}
	for _, au := range a.Remote.AllowedUsers {
		if strings.TrimSpace(au) == "" {
			return fmt.Errorf("config: remote: allowed_users contains an empty entry")
		}
	}
	return nil
}

// validateBroker fails closed on capability-config mistakes: duplicate tool
// names, grants referencing an undeclared tool/op, and delegate.to naming an
// undeclared agent. A worker with no grants is fine (default-deny ⇒ no path).
func (a *App) validateBroker() error {
	// LLM-auth: validate the enum and, when any agent is api-key, require an
	// api_key_file (that it exists at 0600 is a host probe: CheckHost).
	if !a.Broker.LLMAuth.valid() {
		return fmt.Errorf("config: broker.llm_auth %q invalid (want subscription|api-key)", a.Broker.LLMAuth)
	}
	if !a.Manager.LLMAuth.valid() {
		return fmt.Errorf("config: manager.llm_auth %q invalid (want subscription|api-key)", a.Manager.LLMAuth)
	}
	for _, g := range a.Workers {
		if !g.LLMAuth.valid() {
			return fmt.Errorf("config: worker %s llm_auth %q invalid (want subscription|api-key)", g.Name, g.LLMAuth)
		}
	}
	// Mixed instances are UNSUPPORTED: the OAuth credential is a single jail-wide
	// Hub secret and egress is applied jail-wide, so a subscription agent forces
	// the real token into — and open internet egress for — the api-key agents'
	// containers, defeating their key isolation. An instance must be uniformly
	// api-key OR uniformly subscription. See security-model-credentials.md §6.1. (Resolved
	// modes, so a broker/manager default that disagrees with a worker override is
	// caught too.)
	if a.mixedLLMAuth() {
		return fmt.Errorf("config: llm_auth is mixed across the instance (both api-key and subscription agents) — this is unsupported; an instance must be uniformly api-key or uniformly subscription (see security-model-credentials.md §6.1)")
	}
	// Egress is an independent posture (not derived from llm_auth). `closed`
	// requires a uniformly api-key instance — a subscription agent needs direct
	// internet to reach Anthropic, which a closed jail forbids.
	if !a.Egress.valid() {
		return fmt.Errorf("config: egress %q invalid (want open|closed)", a.Egress)
	}
	if a.Egress == EgressClosed {
		if _, anySub := a.llmAuthModes(); anySub {
			return fmt.Errorf("config: egress: closed requires every agent to be api-key (a subscription agent needs direct internet to reach Anthropic)")
		}
	}
	if a.AnyAPIKeyAgent() {
		if a.Broker.APIKeyFile == "" {
			return fmt.Errorf("config: broker.api_key_file is required when llm_auth is api-key")
		}
	}
	return a.validateBrokerGrants()
}

// validateBrokerGrants validates tool declarations, grant references, and
// delegate targets. Called by validateBroker after the LLM-auth block.
func (a *App) validateBrokerGrants() error {
	// Known tools + their op sets.
	toolOps := map[string]map[string]bool{}
	// Built-in reserved pseudo-tool: llm (broker /llm proxy, no backend subprocess).
	toolOps[wire.ReservedLLMTool] = map[string]bool{wire.ReservedLLMOp: true}
	for _, t := range a.Broker.Tools {
		if t.Name == "" {
			return fmt.Errorf("config: broker.tools entry has empty name")
		}
		if !toolNameRE.MatchString(t.Name) {
			return fmt.Errorf("config: broker tool name %q must match %s (it becomes the /mcp/%s/ tool route and a claude mcp add token)", t.Name, toolNameRE, t.Name)
		}
		if _, dup := toolOps[t.Name]; dup {
			return fmt.Errorf("config: duplicate broker tool %q", t.Name)
		}
		if err := t.validate(); err != nil {
			return err
		}
		ops := map[string]bool{}
		if t.External && t.EffectiveGate() == GateCoarse {
			ops[wire.WildcardOp] = true
		}
		for _, o := range t.Operations {
			ops[o.Name] = true
		}
		toolOps[t.Name] = ops
	}
	// Known agent identities: the manager CN + every worker name.
	agents := map[string]bool{a.ManagerCN(): true}
	for _, g := range a.Workers {
		agents[g.Name] = true
	}
	checkCap := func(who, tool, op string) error {
		ops, ok := toolOps[tool]
		if !ok {
			return fmt.Errorf("config: %s grants tool %q which is not a declared broker.tool", who, tool)
		}
		if !ops[op] {
			if op == wire.WildcardOp {
				return fmt.Errorf("config: %s grants op %q on tool %q, but the wildcard is honored only for a gate: coarse external tool", who, op, tool)
			}
			return fmt.Errorf("config: %s grants %q on tool %q which has no such operation", who, op, tool)
		}
		return nil
	}
	checkAgentGrants := func(who string, obtain []Grant, delegate []DelegateGrant) error {
		for _, g := range obtain {
			if err := checkCap(who+".obtain", g.Tool, g.Op); err != nil {
				return err
			}
		}
		for _, d := range delegate {
			if err := checkCap(who+".delegate", d.Tool, d.Op); err != nil {
				return err
			}
			for _, to := range d.To {
				if !agents[to] {
					return fmt.Errorf("config: %s delegates to %q which is not a declared agent", who, to)
				}
			}
		}
		return nil
	}
	if err := checkAgentGrants("manager", a.Manager.Obtain, a.Manager.Delegate); err != nil {
		return err
	}
	for _, g := range a.Workers {
		if err := checkAgentGrants("worker "+g.Name, g.Obtain, g.Delegate); err != nil {
			return err
		}
	}
	return nil
}
